/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package lvm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/diff/walking"
	"github.com/containerd/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/diff/apply"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/epoch"
	apis "github.com/openebs/lvm-localpv/pkg/apis/openebs.io/lvm/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devboxlvm "github.com/containerd/containerd/snapshots/devbox/lvm"
)

type lvmDiff struct {
	store  content.Store
	walker diff.Comparer
}

const blockDiffPathLabel = "containerd.io/diff/lvm.block-diff-path"

// NewLVMDiff returns an LVM-aware comparer. The thin_send based implementation
// will replace the fallback path.
func NewLVMDiff(store content.Store) diff.Comparer {
	return &lvmDiff{
		store:  store,
		walker: walking.NewWalkingDiff(store),
	}
}

// WithBlockDiffPath passes a block-diff file path to the LVM comparer.
// The block-diff file is expected to be a text file containing "start length"
// pairs in bytes (one range per line, '#' for comments).
func WithBlockDiffPath(path string) diff.Opt {
	return func(c *diff.Config) error {
		if path == "" {
			return nil
		}
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		c.Labels[blockDiffPathLabel] = path
		return nil
	}
}

func (d *lvmDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (ocispec.Descriptor, error) {
	var config diff.Config
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	if tm := epoch.FromContext(ctx); tm != nil && config.SourceDateEpoch == nil {
		config.SourceDateEpoch = tm
	}

	upperVG, upperLV, ok := parseDeviceSource(upper)
	if !ok {
		return d.walker.Compare(ctx, lower, upper, opts...)
	}

	snapName := "snap-" + randHex(8)
	if err := createReadonlySnapshot(ctx, upperVG, upperLV, snapName); err != nil {
		return ocispec.Descriptor{}, err
	}
	defer func() {
		_ = destroyReadonlySnapshot(ctx, upperVG, snapName, upperLV)
	}()

	upperSnap := []mount.Mount{
		{
			Source:  fmt.Sprintf("/dev/%s/%s", upperVG, snapName),
			Type:    "ext4",
			Options: []string{"ro"},
		},
	}

	if blockDiffPath := config.Labels[blockDiffPathLabel]; blockDiffPath != "" {
		return d.compareWithBlockDiff(ctx, lower, upperSnap, blockDiffPath, &config)
	}

	return d.walker.Compare(ctx, lower, upperSnap, opts...)
}

func (d *lvmDiff) compareWithBlockDiff(ctx context.Context, lower, upper []mount.Mount, blockDiffPath string, config *diff.Config) (ocispec.Descriptor, error) {
	if blockDiffPath == "" {
		return ocispec.Descriptor{}, fmt.Errorf("block diff path is empty")
	}
	if config.Compressor != nil && config.MediaType == "" {
		return ocispec.Descriptor{}, errors.New("media type must be explicitly specified when using custom compressor")
	}
	if config.MediaType == "" {
		config.MediaType = ocispec.MediaTypeImageLayerGzip
	}
	switch config.MediaType {
	case ocispec.MediaTypeImageLayer, ocispec.MediaTypeImageLayerGzip:
	default:
		return ocispec.Descriptor{}, fmt.Errorf("unsupported diff media type: %v: %w", config.MediaType, errdefs.ErrNotImplemented)
	}

	ranges, err := readBlockRanges(blockDiffPath)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	return writeDiffFromBlockRanges(ctx, d.store, lower, upper, ranges, config)
}

func parseDeviceSource(mounts []mount.Mount) (string, string, bool) {
	if len(mounts) != 1 {
		return "", "", false
	}
	src := mounts[0].Source
	if !strings.HasPrefix(src, "/dev/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(src, "/dev/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func randHex(n int) string {
	if n <= 0 {
		return "0"
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0"
	}
	return hex.EncodeToString(b)
}

func createReadonlySnapshot(ctx context.Context, vgName, originLV, snapName string) error {
	snap := &apis.LVMSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapName,
		},
		Spec: apis.LVMSnapshotSpec{
			VolGroup: vgName,
			SnapSize: "",
		},
	}
	snap.Labels = map[string]string{
		devboxlvm.LVMVolKey: originLV,
	}
	return devboxlvm.CreateSnapshot(ctx, snap)
}

func destroyReadonlySnapshot(ctx context.Context, vgName, snapName, originLV string) error {
	snap := &apis.LVMSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapName,
		},
		Spec: apis.LVMSnapshotSpec{
			VolGroup: vgName,
		},
	}
	snap.Labels = map[string]string{
		devboxlvm.LVMVolKey: originLV,
	}
	return devboxlvm.DestroySnapshot(ctx, snap)
}

type lvmApplier struct {
	delegate diff.Applier
}

// NewLVMApplier returns an applier for LVM-backed mounts.
func NewLVMApplier(store content.Provider) diff.Applier {
	return &lvmApplier{
		delegate: apply.NewFileSystemApplier(store),
	}
}

func (a *lvmApplier) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (ocispec.Descriptor, error) {
	return a.delegate.Apply(ctx, desc, mounts, opts...)
}

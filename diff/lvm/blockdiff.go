package lvm

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/labels"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type blockRange struct {
	Start  uint64
	Length uint64
}

func (r blockRange) End() uint64 {
	return r.Start + r.Length
}

func readBlockRanges(path string) ([]blockRange, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open block diff file: %w", err)
	}
	defer f.Close()

	ranges, err := parseBlockRanges(f)
	if err != nil {
		return nil, err
	}
	if len(ranges) == 0 {
		return nil, fmt.Errorf("block diff file contains no ranges")
	}
	return normalizeRanges(ranges), nil
}

// parseBlockRanges parses a text file of "start length" ranges in bytes.
// Lines starting with '#' are ignored.
func parseBlockRanges(r io.Reader) ([]blockRange, error) {
	var ranges []blockRange
	scanner := bufio.NewScanner(r)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("invalid block range line %d: %q", lineNo, line)
		}
		start, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid start at line %d: %w", lineNo, err)
		}
		length, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid length at line %d: %w", lineNo, err)
		}
		if length == 0 {
			continue
		}
		ranges = append(ranges, blockRange{Start: start, Length: length})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read block diff file: %w", err)
	}
	return ranges, nil
}

func normalizeRanges(in []blockRange) []blockRange {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].Start == in[j].Start {
			return in[i].Length < in[j].Length
		}
		return in[i].Start < in[j].Start
	})
	merged := []blockRange{in[0]}
	for _, r := range in[1:] {
		last := &merged[len(merged)-1]
		if r.Start <= last.End() {
			if r.End() > last.End() {
				last.Length = r.End() - last.Start
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func writeDiffFromBlockRanges(ctx context.Context, store content.Store, lower, upper []mount.Mount, ranges []blockRange, config *diff.Config) (ocispec.Descriptor, error) {
	var ocidesc ocispec.Descriptor
	var writeOpts []archive.ChangeWriterOpt
	if config.SourceDateEpoch != nil {
		writeOpts = append(writeOpts,
			archive.WithModTimeUpperBound(*config.SourceDateEpoch),
			archive.WithWhiteoutTime(*config.SourceDateEpoch),
		)
	}

	if err := mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
		return mount.WithReadonlyTempMount(ctx, upper, func(upperRoot string) error {
			changes, err := blockRangesToChanges(ctx, lowerRoot, upperRoot, ranges)
			if err != nil {
				return err
			}

			var newReference bool
			if config.Reference == "" {
				newReference = true
				config.Reference = uniqueRef()
			}
			cw, err := store.Writer(ctx,
				content.WithRef(config.Reference),
				content.WithDescriptor(ocispec.Descriptor{MediaType: config.MediaType}),
			)
			if err != nil {
				return fmt.Errorf("failed to open writer: %w", err)
			}

			var errOpen error
			defer func() {
				if errOpen != nil {
					cw.Close()
					if newReference {
						if abortErr := store.Abort(ctx, config.Reference); abortErr != nil {
							log.G(ctx).WithError(abortErr).WithField("ref", config.Reference).Warnf("failed to delete diff upload")
						}
					}
				}
			}()
			if !newReference {
				if errOpen = cw.Truncate(0); errOpen != nil {
					return errOpen
				}
			}

			isCompressed := config.MediaType == ocispec.MediaTypeImageLayerGzip || config.Compressor != nil
			if isCompressed {
				dgstr := digest.SHA256.Digester()
				var compressed io.WriteCloser
				if config.Compressor != nil {
					compressed, errOpen = config.Compressor(cw, config.MediaType)
					if errOpen != nil {
						return fmt.Errorf("failed to get compressed stream: %w", errOpen)
					}
				} else {
					compressed, errOpen = compression.CompressStream(cw, compression.Gzip)
					if errOpen != nil {
						return fmt.Errorf("failed to get compressed stream: %w", errOpen)
					}
				}
				errOpen = writeChanges(ctx, io.MultiWriter(compressed, dgstr.Hash()), upperRoot, changes, writeOpts)
				compressed.Close()
				if errOpen != nil {
					return fmt.Errorf("failed to write compressed diff: %w", errOpen)
				}
				if config.Labels == nil {
					config.Labels = map[string]string{}
				}
				config.Labels[labels.LabelUncompressed] = dgstr.Digest().String()
			} else {
				if errOpen = writeChanges(ctx, cw, upperRoot, changes, writeOpts); errOpen != nil {
					return fmt.Errorf("failed to write diff: %w", errOpen)
				}
			}

			var commitopts []content.Opt
			if config.Labels != nil {
				commitopts = append(commitopts, content.WithLabels(config.Labels))
			}

			dgst := cw.Digest()
			if errOpen = cw.Commit(ctx, 0, dgst, commitopts...); errOpen != nil {
				if !errdefs.IsAlreadyExists(errOpen) {
					return fmt.Errorf("failed to commit: %w", errOpen)
				}
				errOpen = nil
			}

			info, err := store.Info(ctx, dgst)
			if err != nil {
				return fmt.Errorf("failed to get info from content store: %w", err)
			}
			if info.Labels == nil {
				info.Labels = make(map[string]string)
			}
			if _, ok := info.Labels[labels.LabelUncompressed]; !ok && config.Labels != nil {
				info.Labels[labels.LabelUncompressed] = config.Labels[labels.LabelUncompressed]
				if _, err := store.Update(ctx, info, "labels."+labels.LabelUncompressed); err != nil {
					return fmt.Errorf("error setting uncompressed label: %w", err)
				}
			}

			ocidesc = ocispec.Descriptor{
				MediaType: config.MediaType,
				Size:      info.Size,
				Digest:    info.Digest,
			}
			return nil
		})
	}); err != nil {
		return ocispec.Descriptor{}, err
	}

	return ocidesc, nil
}

type change struct {
	Kind fs.ChangeKind
	Path string
	Info os.FileInfo
}

func blockRangesToChanges(ctx context.Context, lowerRoot, upperRoot string, ranges []blockRange) ([]change, error) {
	upperPaths, err := collectChangedPaths(upperRoot, ranges)
	if err != nil {
		return nil, err
	}
	lowerPaths, err := collectChangedPaths(lowerRoot, ranges)
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	var changes []change
	for p, info := range upperPaths {
		seen[p] = struct{}{}
		if _, ok := lowerPaths[p]; ok {
			changes = append(changes, change{Kind: fs.ChangeKindModify, Path: p, Info: info})
		} else {
			changes = append(changes, change{Kind: fs.ChangeKindAdd, Path: p, Info: info})
		}
	}
	for p := range lowerPaths {
		if _, ok := seen[p]; ok {
			continue
		}
		changes = append(changes, change{Kind: fs.ChangeKindDelete, Path: p})
	}

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].Path < changes[j].Path
	})
	return changes, nil
}

func collectChangedPaths(root string, ranges []blockRange) (map[string]os.FileInfo, error) {
	paths := map[string]os.FileInfo{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !(info.Mode().IsRegular() || info.IsDir()) {
			return nil
		}
		extents, err := fiemapExtents(path)
		if err != nil {
			return err
		}
		if intersectsRanges(extents, ranges) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.Join(string(os.PathSeparator), rel)
			paths[rel] = info
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

func intersectsRanges(extents []fileExtent, ranges []blockRange) bool {
	if len(extents) == 0 || len(ranges) == 0 {
		return false
	}
	for _, ext := range extents {
		if rangeIntersect(ranges, ext.Start, ext.Length) {
			return true
		}
	}
	return false
}

func rangeIntersect(ranges []blockRange, start, length uint64) bool {
	if length == 0 {
		return false
	}
	end := start + length
	idx := sort.Search(len(ranges), func(i int) bool {
		return ranges[i].End() > start
	})
	if idx >= len(ranges) {
		return false
	}
	return ranges[idx].Start < end
}

func writeChanges(ctx context.Context, w io.Writer, upperRoot string, changes []change, opts []archive.ChangeWriterOpt) error {
	cw := archive.NewChangeWriter(w, upperRoot, opts...)
	for _, ch := range changes {
		if err := cw.HandleChange(ch.Kind, ch.Path, ch.Info, nil); err != nil {
			return err
		}
	}
	return cw.Close()
}

func uniqueRef() string {
	t := time.Now()
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", t.UnixNano())
	}
	return fmt.Sprintf("%d-%s", t.UnixNano(), base64.URLEncoding.EncodeToString(b[:]))
}

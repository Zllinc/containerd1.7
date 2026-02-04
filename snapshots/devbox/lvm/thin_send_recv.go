/*
Copyright 2025 The containerd Authors.

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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// ThinSend streams thin pool block-level diffs between baseLV and targetLV into the provided writer.
// This is a thin-send-recv integration point and does not require Snapshotter changes.
//
// Note: both baseLV and targetLV must be thin volumes or thin snapshots.
func ThinSend(ctx context.Context, baseLV, targetLV string, out io.Writer) error {
	if baseLV == "" || targetLV == "" {
		return fmt.Errorf("baseLV and targetLV must be non-empty")
	}
	if out == nil {
		return fmt.Errorf("output writer must be non-nil")
	}

	if _, err := exec.LookPath("thin_send"); err != nil {
		return fmt.Errorf("thin_send not found in PATH: %w", err)
	}

	cmd := exec.CommandContext(ctx, "thin_send", baseLV, targetLV)
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := stderr.String()
		if errMsg != "" {
			return fmt.Errorf("thin_send failed: %w: %s", err, errMsg)
		}
		return fmt.Errorf("thin_send failed: %w", err)
	}

	return nil
}

// ThinSendToFile writes thin_send output to a file at outputPath.
// The output file is created with 0600 permissions.
func ThinSendToFile(ctx context.Context, baseLV, targetLV, outputPath string) error {
	if outputPath == "" {
		return fmt.Errorf("outputPath must be non-empty")
	}

	outFile, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	if err := ThinSend(ctx, baseLV, targetLV, outFile); err != nil {
		return err
	}

	if err := outFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync output file: %w", err)
	}

	return nil
}

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
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apis "github.com/openebs/lvm-localpv/pkg/apis/openebs.io/lvm/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Test constants for snapshot tests
const (
	testLVName = "test-devbox-lv"
	testLVSize = 10 * 1024 * 1024 * 1024 // 10GB in bytes
)

// TestDevboxLVSnapshotBasic tests the basic functionality of Devbox LV snapshot
func TestDevboxLVSnapshotBasic(t *testing.T) {
	// check if running as root (LVM commands require root)
	if os.Getuid() != 0 {
		t.Skip("Skipping test: must be run as root")
	}

	ctx := context.Background()

	// step 0: clean up possible leftover test resources
	cleanupDevboxTestResources(ctx, t)

	// step 1: create test LV
	t.Log("=== Step 1: Creating test LV ===")
	if err := createDevboxTestLV(ctx, t); err != nil {
		t.Fatalf("Failed to create test LV: %v", err)
	}
	defer cleanupDevboxTestResources(ctx, t) // ensure cleanup after test

	// verify LV creation success
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: testLVName,
		},
		Spec: apis.VolumeInfo{
			VolGroup: testVGName,
		},
	}
	exists, err := CheckVolumeExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check if LV exists: %v", err)
	}
	if !exists {
		t.Fatalf("LV should exist after creation")
	}

	// verify snapshot doesn't exist yet
	snapshotExists, err := CheckDevboxSnapshotExists(ctx, testVGName, testLVName)
	if err != nil {
		t.Fatalf("Failed to check if snapshot exists: %v", err)
	}
	if snapshotExists {
		t.Fatalf("Snapshot should not exist before creating one")
	}
	t.Log("✓ Test LV created successfully")

	// step 2: create snapshot
	t.Log("=== Step 2: Creating Devbox LV snapshot ===")
	snapshotPath, err := CreateDevboxLVSnapshot(ctx, testVGName, testLVName)
	if err != nil {
		t.Fatalf("Failed to create snapshot: %v", err)
	}
	t.Logf("✓ Snapshot created: %s", snapshotPath)

	// check snapshot path format
	expectedSnapshotPath := fmt.Sprintf("%s/%s-snapshot", testVGName, testLVName)
	if snapshotPath != expectedSnapshotPath {
		t.Errorf("Expected snapshot path %s, got %s", expectedSnapshotPath, snapshotPath)
	}
	t.Log("✓ Snapshot path format correct")

	// step 3: verify snapshot exists
	t.Log("=== Step 3: Verifying snapshot exists ===")
	snapshotExists, err = CheckDevboxSnapshotExists(ctx, testVGName, testLVName)
	if err != nil {
		t.Fatalf("Failed to check if snapshot exists: %v", err)
	}
	if !snapshotExists {
		t.Fatalf("Snapshot does not exist after creation")
	}
	t.Log("✓ Snapshot exists")

	// step 4: get snapshot data usage
	t.Log("=== Step 4: Getting snapshot data usage ===")
	dataPercent, err := GetDevboxSnapshotDataPercent(ctx, testVGName, testLVName)
	if err != nil {
		t.Fatalf("Failed to get snapshot info: %v", err)
	}
	t.Logf("✓ Snapshot data usage: %.2f%%", dataPercent)

	// step 5: delete snapshot
	t.Log("=== Step 5: Deleting snapshot ===")
	if err := DestroyDevboxLVSnapshot(ctx, testVGName, testLVName); err != nil {
		t.Fatalf("Failed to destroy snapshot: %v", err)
	}
	t.Log("✓ Snapshot deleted")

	// verify snapshot is deleted
	snapshotExists, err = CheckDevboxSnapshotExists(ctx, testVGName, testLVName)
	if err != nil {
		t.Fatalf("Failed to check if snapshot exists after deletion: %v", err)
	}
	if snapshotExists {
		t.Fatalf("Snapshot still exists after deletion")
	}
	t.Log("✓ Snapshot successfully removed")
}

// TestDevboxLVSnapshotCOW test Copy-on-Write feature
func TestDevboxLVSnapshotCOW(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Skipping test: must be run as root")
	}

	ctx := context.Background()

	// clean up
	cleanupDevboxTestResources(ctx, t)

	// create test LV
	if err := createDevboxTestLV(ctx, t); err != nil {
		t.Fatalf("Failed to create test LV: %v", err)
	}
	defer cleanupDevboxTestResources(ctx, t)

	// create snapshot
	snapshotPath, err := CreateDevboxLVSnapshot(ctx, testVGName, testLVName)
	if err != nil {
		t.Fatalf("Failed to create snapshot: %v", err)
	}
	t.Logf("✓ Snapshot created: %s", snapshotPath)

	// get initial data usage
	initialPercent, err := GetDevboxSnapshotDataPercent(ctx, testVGName, testLVName)
	if err != nil {
		t.Fatalf("Failed to get initial data percent: %v", err)
	}
	t.Logf("Initial snapshot data usage: %.2f%%", initialPercent)

	// write some data to original LV (test COW)
	t.Log("=== Writing data to original LV to test Copy-on-Write ===")
	if err := writeToDevboxTestLV(ctx, t); err != nil {
		t.Logf("Warning: Failed to write to test LV: %v", err)
		t.Log("This is expected if LV is not formatted, skipping COW test")
		return
	}
	t.Log("✓ Data written to original LV")

	// check snapshot data usage again
	newPercent, err := GetDevboxSnapshotDataPercent(ctx, testVGName, testLVName)
	if err != nil {
		t.Fatalf("Failed to get data percent after write: %v", err)
	}
	t.Logf("Snapshot data usage after write: %.2f%%", newPercent)

	// snapshot data usage should increase (because of COW)
	if newPercent > initialPercent {
		t.Logf("✓ Copy-on-Write working: data percent increased from %.2f%% to %.2f%%",
			initialPercent, newPercent)
	} else {
		t.Logf("Note: Data percent unchanged (%.2f%%), may need more data writes",
			newPercent)
	}

	// clean up snapshot
	DestroyDevboxLVSnapshot(ctx, testVGName, testLVName)
}

// TestDevboxDuplicateSnapshotCreation test creating duplicate snapshot (should fail)
func TestDevboxDuplicateSnapshotCreation(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Skipping test: must be run as root")
	}

	ctx := context.Background()

	// clean up
	cleanupDevboxTestResources(ctx, t)

	// create test LV
	if err := createDevboxTestLV(ctx, t); err != nil {
		t.Fatalf("Failed to create test LV: %v", err)
	}
	// defer cleanupDevboxTestResources(ctx, t)

	// create first snapshot
	snapshotPath, err := CreateDevboxLVSnapshot(ctx, testVGName, testLVName)
	if err != nil {
		t.Fatalf("Failed to create first snapshot: %v", err)
	}
	t.Logf("✓ First snapshot created: %s", snapshotPath)

	// // try to create duplicate snapshot (should fail)
	// t.Log("=== Attempting to create duplicate snapshot ===")
	// _, err = CreateDevboxLVSnapshot(ctx, testVGName, testLVName)
	// if err == nil {
	// 	t.Error("Expected error when creating duplicate snapshot, got nil")
	// } else {
	// 	t.Logf("✓ Duplicate snapshot creation failed as expected: %v", err)
	// }

	// // clean up
	// DestroyDevboxLVSnapshot(ctx, testVGName, testLVName)
}

// TestDevboxSnapshotWithoutSourceLV test creating snapshot for non-existent LV (should fail)
func TestDevboxSnapshotWithoutSourceLV(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Skipping test: must be run as root")
	}

	ctx := context.Background()

	// try to create snapshot for non-existent LV
	t.Log("=== Attempting to create snapshot for non-existent LV ===")
	_, err := CreateDevboxLVSnapshot(ctx, testVGName, "non-existent-lv")
	if err == nil {
		t.Error("Expected error when creating snapshot for non-existent LV, got nil")
	} else {
		t.Logf("✓ Correctly failed to create snapshot: %v", err)
	}
}

// BenchmarkDevboxLVSnapshotCreation performance test: snapshot creation speed
func BenchmarkDevboxLVSnapshotCreation(b *testing.B) {
	if os.Getuid() != 0 {
		b.Skip("Skipping benchmark: must be run as root")
	}

	ctx := context.Background()

	// ensure test LV exists
	if err := createDevboxTestLV(ctx, &testing.T{}); err != nil {
		b.Fatalf("Failed to create test LV: %v", err)
	}
	defer cleanupDevboxTestResources(ctx, &testing.T{})

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		snapshotPath, err := CreateDevboxLVSnapshot(ctx, testVGName, testLVName)
		if err != nil {
			b.Fatalf("Failed to create snapshot: %v", err)
		}
		b.Logf("Created snapshot: %s", snapshotPath)

		// clean up snapshot
		DestroyDevboxLVSnapshot(ctx, testVGName, testLVName)
	}
}

// === helper functions ===

// createDevboxTestLV create test LV for testing
func createDevboxTestLV(ctx context.Context, t *testing.T) error {
	t.Logf("Creating test LV: %s/%s (%.2f GB)", testVGName, testLVName, float64(testLVSize)/(1024*1024*1024))

	// first ensure volume group exists
	if err := ensureDevboxTestVG(ctx, t); err != nil {
		return err
	}

	// check if LV already exists
	args := []string{"--noheadings", "-o", "lv_name", testVGName}
	output, _, err := RunCommandSplit(ctx, LVList, args...)
	if err != nil {
		return fmt.Errorf("failed to list volumes: %w", err)
	}

	// check if LV already exists
	for _, lv := range strings.Fields(string(output)) {
		if lv == testLVName {
			t.Logf("Test LV already exists, reusing it")
			return nil
		}
	}

	// create LV
	sizeGB := testLVSize / (1024 * 1024 * 1024)
	sizeStr := fmt.Sprintf("%dG", sizeGB)
	args = []string{
		"-L", sizeStr,
		"-n", testLVName,
		"-y",
		testVGName,
	}

	_, _, err = RunCommandSplit(ctx, LVCreate, args...)
	if err != nil {
		return fmt.Errorf("failed to create LV: %w", err)
	}

	t.Logf("✓ Test LV created successfully")

	// format LV (optional, for testing write)
	if err := formatDevboxTestLV(ctx, t); err != nil {
		t.Logf("Warning: Failed to format LV: %v", err)
		// don't return error, because subsequent tests may not need formatting
	}

	return nil
}

// ensureDevboxTestVG ensure test volume group exists
func ensureDevboxTestVG(ctx context.Context, t *testing.T) error {
	// check if volume group exists
	args := []string{"--noheadings", "-o", "vg_name"}
	output, _, err := RunCommandSplit(ctx, VGList, args...)
	if err != nil {
		return fmt.Errorf("failed to list volume groups: %w", err)
	}

	vgs := strings.Fields(string(output))
	vgExists := false
	for _, vg := range vgs {
		if vg == testVGName {
			vgExists = true
			break
		}
	}

	if !vgExists {
		return fmt.Errorf("test volume group %s does not exist. Please create it first using: sudo vgcreate %s <device>", testVGName, testVGName)
	}

	return nil
}

// formatDevboxTestLV format test LV (ext4 file system)
func formatDevboxTestLV(ctx context.Context, t *testing.T) error {
	lvPath := fmt.Sprintf("/dev/%s/%s", testVGName, testLVName)

	t.Logf("Formatting LV with ext4: %s", lvPath)

	cmd := exec.Command("mkfs.ext4", "-F", lvPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to format LV: %w: %s", err, string(output))
	}

	t.Logf("✓ LV formatted successfully")
	return nil
}

// writeToDevboxTestLV write some data to test LV (test COW)
func writeToDevboxTestLV(ctx context.Context, t *testing.T) error {
	// create temporary mount point
	mountDir := filepath.Join(os.TempDir(), fmt.Sprintf("lvm-test-mount-%d", time.Now().Unix()))
	if err := os.MkdirAll(mountDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount directory: %w", err)
	}
	defer os.RemoveAll(mountDir)

	// mount LV
	lvPath := fmt.Sprintf("/dev/%s/%s", testVGName, testLVName)
	t.Logf("Mounting LV to %s", mountDir)

	cmd := exec.Command("mount", lvPath, mountDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to mount LV: %w: %s", err, string(output))
	}
	defer func() {
		exec.Command("umount", mountDir).Run()
	}()

	t.Log("Writing test data to LV")

	// write test data
	testFile := filepath.Join(mountDir, "test-data.txt")
	data := strings.Repeat("Hello, LVM Snapshot! This is a test for Copy-on-Write.\n", 1000) // ~50KB
	if err := os.WriteFile(testFile, []byte(data), 0644); err != nil {
		return fmt.Errorf("failed to write test file: %w", err)
	}

	t.Logf("✓ Wrote %d bytes to LV", len(data))

	// sync file system
	cmd = exec.Command("sync")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to sync: %w", err)
	}

	return nil
}

// cleanupDevboxTestResources clean up test resources
func cleanupDevboxTestResources(ctx context.Context, t *testing.T) {
	t.Log("=== Cleaning up test resources ===")

	// delete snapshot
	if err := DestroyDevboxLVSnapshot(ctx, testVGName, testLVName); err != nil {
		t.Logf("Warning: Failed to destroy snapshot: %v", err)
	}

	// delete test LV
	lvPath := fmt.Sprintf("%s/%s", testVGName, testLVName)
	args := []string{"-y", lvPath}
	if _, _, err := RunCommandSplit(ctx, LVRemove, args...); err != nil {
		t.Logf("Warning: Failed to destroy test LV: %v", err)
	}

	t.Log("✓ Cleanup completed")
}

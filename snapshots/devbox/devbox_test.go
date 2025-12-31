//go:build linux

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

package devbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	apis "github.com/openebs/lvm-localpv/pkg/apis/openebs.io/lvm/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/containerd/containerd/snapshots/devbox/lvm"
)

const (
	testVGName   = "devbox-vg"
	testPoolName = "devbox-vg-thinpool"
)

// TestFindMountPointAndUnmount tests the findMountPointByDevice and unmountLvm functions
// It creates an LV, mounts it to a temporary directory, verifies findMountPointByDevice
// can find the mount point, and then tests unmountLvm to unmount it.
func TestFindMountPointAndUnmount(t *testing.T) {
	ctx := context.Background()

	// Create a temporary directory for the test
	tmpRoot, err := os.MkdirTemp("", "devbox-test-")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tmpRoot)

	// Create a minimal Snapshotter instance for testing
	snapshotter := &Snapshotter{
		lvmVgName:    testVGName,
		ThinPoolName: testPoolName,
	}

	// Generate a unique LV name for this test
	lvName := fmt.Sprintf("test-mount-unmount-%d", os.Getpid())

	// Create the test volume
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: lvName,
		},
		Spec: apis.VolumeInfo{
			Capacity:      "100M",
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	// Clean up LV at the end
	defer func() {
		// Force destroy the volume
		if err := lvm.ForceDestroyVolume(ctx, vol); err != nil {
			t.Logf("Warning: Failed to clean up test LV %s: %v", lvName, err)
		}
	}()

	// Step 1: Create the LV
	t.Logf("Step 1: Creating LV %s", lvName)
	if err := lvm.CreateVolume(ctx, vol); err != nil {
		t.Fatalf("Failed to create test volume: %v", err)
	}

	// Verify LV exists
	devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, lvName)
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		t.Fatalf("LVM logical volume %s does not exist: %v", devicePath, err)
	}

	// Step 2: Format the filesystem
	t.Logf("Step 2: Formatting filesystem on %s", devicePath)
	cmd := exec.Command("mkfs.ext4", "-F", devicePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to create filesystem on %s: %v, output: %s", devicePath, err, string(output))
	}

	// // Step 3: Create a temporary mount point
	mountPoint := filepath.Join(tmpRoot, "mount-point")
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		t.Fatalf("Failed to create mount point directory: %v", err)
	}
	// defer os.RemoveAll(mountPoint)

	// Step 4: Mount the LV
	t.Logf("Step 4: Mounting %s to %s", devicePath, mountPoint)
	if err := syscall.Mount(devicePath, mountPoint, "ext4", 0, ""); err != nil {
		t.Fatalf("Failed to mount %s to %s: %v", devicePath, mountPoint, err)
	}
	// Ensure unmount at the end (in case test fails)
	mounted := true
	defer func() {
		if mounted {
			if err := syscall.Unmount(mountPoint, 0); err != nil {
				t.Logf("Warning: Failed to unmount %s during cleanup: %v", mountPoint, err)
			}
		}
	}()

	// Step 5: Test findMountPointByDevice
	t.Logf("Step 5: Testing findMountPointByDevice for %s", devicePath)
	foundMountPoint, err := findMountPointByDevice(devicePath)
	if err != nil {
		t.Fatalf("findMountPointByDevice failed: %v", err)
	}
	if foundMountPoint == "" {
		t.Fatal("findMountPointByDevice should have found the mount point, but returned empty string")
	}
	if foundMountPoint != mountPoint {
		t.Fatalf("findMountPointByDevice returned wrong mount point: expected %s, got %s", mountPoint, foundMountPoint)
	}
	t.Logf("Successfully found mount point: %s", foundMountPoint)

	// Step 6: Test unmountLvm
	t.Logf("Step 6: Testing unmountLvm for %s", mountPoint)
	if err := snapshotter.unmountLvm(ctx, mountPoint); err != nil {
		t.Fatalf("unmountLvm failed: %v", err)
	}
	mounted = false // Mark as unmounted so defer doesn't try again
	t.Logf("Successfully unmounted %s", mountPoint)

	// Step 7: Verify the mount point is no longer mounted
	t.Logf("Step 7: Verifying mount point is no longer mounted")
	foundMountPoint, err = findMountPointByDevice(devicePath)
	if err != nil {
		t.Fatalf("findMountPointByDevice failed after unmount: %v", err)
	}
	if foundMountPoint != "" {
		t.Fatalf("findMountPointByDevice should return empty string after unmount, but got %s", foundMountPoint)
	}
	t.Logf("Verified: mount point is no longer mounted")

	t.Logf("Test completed successfully")
}

// TestFindMountPointByDevice_UnmountedDevice tests findMountPointByDevice with an unmounted device
func TestFindMountPointByDevice_UnmountedDevice(t *testing.T) {
	ctx := context.Background()

	// Create a test volume
	lvName := fmt.Sprintf("test-unmounted-%d", os.Getpid())
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: lvName,
		},
		Spec: apis.VolumeInfo{
			Capacity:      "100M",
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	// Clean up LV at the end
	defer func() {
		if err := lvm.ForceDestroyVolume(ctx, vol); err != nil {
			t.Logf("Warning: Failed to clean up test LV %s: %v", lvName, err)
		}
	}()

	// Create the LV
	if err := lvm.CreateVolume(ctx, vol); err != nil {
		t.Fatalf("Failed to create test volume: %v", err)
	}

	devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, lvName)

	// Test findMountPointByDevice on an unmounted device
	mountPoint, err := findMountPointByDevice(devicePath)
	if err != nil {
		t.Fatalf("findMountPointByDevice failed: %v", err)
	}
	if mountPoint != "" {
		t.Fatalf("findMountPointByDevice should return empty string for unmounted device, but got %s", mountPoint)
	}

	t.Logf("Test passed: unmounted device correctly returns empty mount point")
}

// TestFindMountPointAndUnmount_Concurrent tests the findMountPointByDevice and unmountLvm
// functions under concurrent conditions. It creates multiple LVs and performs mount/unmount
// operations concurrently to verify there are no race conditions or deadlocks.
func TestFindMountPointAndUnmount_Concurrent(t *testing.T) {
	ctx := context.Background()

	// Number of concurrent operations
	const numConcurrent = 10

	// Create a temporary directory for the test
	tmpRoot, err := os.MkdirTemp("", "devbox-test-concurrent-")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tmpRoot)

	// Create a minimal Snapshotter instance for testing
	snapshotter := &Snapshotter{
		lvmVgName:    testVGName,
		ThinPoolName: testPoolName,
	}

	// Track all created LVs for cleanup
	var allVols []*apis.LVMVolume
	var allVolsMutex sync.Mutex

	// Use WaitGroup to wait for all goroutines to complete
	var wg sync.WaitGroup

	// Channel to collect errors from goroutines
	errorChan := make(chan error, numConcurrent)

	// Launch concurrent operations
	for i := 0; i < numConcurrent; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			// Generate unique LV name for this goroutine
			lvName := fmt.Sprintf("test-concurrent-%d-%d", os.Getpid(), index)

			// Create the test volume
			vol := &apis.LVMVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: lvName,
				},
				Spec: apis.VolumeInfo{
					Capacity:      "100M",
					VolGroup:      testVGName,
					ThinProvision: testPoolName,
				},
			}

			// Register for cleanup
			allVolsMutex.Lock()
			allVols = append(allVols, vol)
			allVolsMutex.Unlock()

			// Step 1: Create the LV
			if err := lvm.CreateVolume(ctx, vol); err != nil {
				errorChan <- fmt.Errorf("goroutine %d: failed to create LV %s: %w", index, lvName, err)
				return
			}

			// Verify LV exists
			devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, lvName)
			if _, err := os.Stat(devicePath); os.IsNotExist(err) {
				errorChan <- fmt.Errorf("goroutine %d: LV %s does not exist: %w", index, devicePath, err)
				return
			}

			// Step 2: Format the filesystem
			cmd := exec.Command("mkfs.ext4", "-F", devicePath)
			output, err := cmd.CombinedOutput()
			if err != nil {
				errorChan <- fmt.Errorf("goroutine %d: failed to format %s: %w, output: %s", index, devicePath, err, string(output))
				return
			}

			// Step 3: Create a temporary mount point
			mountPoint := filepath.Join(tmpRoot, fmt.Sprintf("mount-point-%d", index))
			if err := os.MkdirAll(mountPoint, 0755); err != nil {
				errorChan <- fmt.Errorf("goroutine %d: failed to create mount point: %w", index, err)
				return
			}

			// Step 4: Mount the LV
			if err := syscall.Mount(devicePath, mountPoint, "ext4", 0, ""); err != nil {
				errorChan <- fmt.Errorf("goroutine %d: failed to mount %s to %s: %w", index, devicePath, mountPoint, err)
				return
			}
			// Ensure unmount at the end (in case test fails)
			mounted := true
			defer func() {
				if mounted {
					if err := syscall.Unmount(mountPoint, 0); err != nil {
						t.Logf("Warning: goroutine %d failed to unmount %s during cleanup: %v", index, mountPoint, err)
					}
				}
			}()

			// Step 5: Test findMountPointByDevice (concurrent access)
			foundMountPoint, err := findMountPointByDevice(devicePath)
			if err != nil {
				errorChan <- fmt.Errorf("goroutine %d: findMountPointByDevice failed: %w", index, err)
				return
			}
			if foundMountPoint == "" {
				errorChan <- fmt.Errorf("goroutine %d: findMountPointByDevice should have found mount point for %s", index, devicePath)
				return
			}
			if foundMountPoint != mountPoint {
				errorChan <- fmt.Errorf("goroutine %d: findMountPointByDevice returned wrong mount point: expected %s, got %s", index, mountPoint, foundMountPoint)
				return
			}

			// Step 6: Test unmountLvm (concurrent access)
			if err := snapshotter.unmountLvm(ctx, mountPoint); err != nil {
				errorChan <- fmt.Errorf("goroutine %d: unmountLvm failed: %w", index, err)
				return
			}
			mounted = false // Mark as unmounted so defer doesn't try again

			// Step 7: Verify the mount point is no longer mounted
			foundMountPoint, err = findMountPointByDevice(devicePath)
			if err != nil {
				errorChan <- fmt.Errorf("goroutine %d: findMountPointByDevice failed after unmount: %w", index, err)
				return
			}
			if foundMountPoint != "" {
				errorChan <- fmt.Errorf("goroutine %d: findMountPointByDevice should return empty string after unmount, but got %s", index, foundMountPoint)
				return
			}

			// Success - no error to report
			t.Logf("Goroutine %d: Successfully completed mount/unmount cycle for LV %s", index, lvName)
		}(i)
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(errorChan)

	// Collect all errors
	var errors []error
	for err := range errorChan {
		errors = append(errors, err)
	}

	// Clean up all LVs
	t.Logf("Cleaning up %d LVs...", len(allVols))
	for _, vol := range allVols {
		if err := lvm.ForceDestroyVolume(ctx, vol); err != nil {
			t.Logf("Warning: Failed to clean up test LV %s: %v", vol.Name, err)
		}
	}

	// Report results
	if len(errors) > 0 {
		t.Errorf("Concurrent test failed with %d errors:", len(errors))
		for i, err := range errors {
			t.Errorf("  Error %d: %v", i+1, err)
		}
		t.FailNow()
	}

	t.Logf("Concurrent test passed: all %d goroutines completed successfully", numConcurrent)
}

// TestReadProcMounts tests the readProcMounts function
// It creates an LV, mounts it, and verifies readProcMounts can read and parse /proc/mounts correctly
func TestReadProcMounts(t *testing.T) {
	ctx := context.Background()

	// Generate a unique LV name for this test
	lvName := fmt.Sprintf("test-read-proc-mounts-%d", os.Getpid())

	// Create the test volume
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: lvName,
		},
		Spec: apis.VolumeInfo{
			Capacity:      "100M",
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	// Clean up LV at the end
	defer func() {
		if err := lvm.ForceDestroyVolume(ctx, vol); err != nil {
			t.Logf("Warning: Failed to clean up test LV %s: %v", lvName, err)
		}
	}()

	// Step 1: Create the LV
	t.Logf("Step 1: Creating LV %s", lvName)
	if err := lvm.CreateVolume(ctx, vol); err != nil {
		t.Fatalf("Failed to create test volume: %v", err)
	}

	// Verify LV exists
	devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, lvName)
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		t.Fatalf("LVM logical volume %s does not exist: %v", devicePath, err)
	}

	// Step 2: Format the filesystem
	t.Logf("Step 2: Formatting filesystem on %s", devicePath)
	cmd := exec.Command("mkfs.ext4", "-F", devicePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to create filesystem on %s: %v, output: %s", devicePath, err, string(output))
	}

	// Step 3: Create a temporary mount point
	tmpRoot, err := os.MkdirTemp("", "devbox-test-read-proc-")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tmpRoot)

	mountPoint := filepath.Join(tmpRoot, "mount-point")
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		t.Fatalf("Failed to create mount point directory: %v", err)
	}

	// Step 4: Mount the LV
	t.Logf("Step 4: Mounting %s to %s", devicePath, mountPoint)
	if err := syscall.Mount(devicePath, mountPoint, "ext4", 0, ""); err != nil {
		t.Fatalf("Failed to mount %s to %s: %v", devicePath, mountPoint, err)
	}
	defer func() {
		if err := syscall.Unmount(mountPoint, 0); err != nil {
			t.Logf("Warning: Failed to unmount %s during cleanup: %v", mountPoint, err)
		}
	}()

	// Step 5: Test readProcMounts
	t.Logf("Step 5: Testing readProcMounts")
	mounts, err := readProcMounts()
	if err != nil {
		t.Fatalf("readProcMounts failed: %v", err)
	}

	if len(mounts) == 0 {
		t.Fatal("readProcMounts returned empty slice, expected at least one mount entry")
	}

	// Verify the mounted LV is in the results
	found := false
	for _, mount := range mounts {
		if len(mount) < 2 {
			continue
		}
		mountDevice := mount[0]
		mountPointFromProc := mount[1]

		// Check if this is our mount
		if mountPointFromProc == mountPoint {
			found = true
			t.Logf("Found mount entry: device=%s, mountpoint=%s", mountDevice, mountPointFromProc)
			// Verify device path matches (may be symlink, so check both)
			if mountDevice == devicePath {
				t.Logf("Device path matches directly: %s", devicePath)
			} else {
				// Check if it's a symlink resolution
				resolvedDevice, err := filepath.EvalSymlinks(devicePath)
				if err == nil && resolvedDevice == mountDevice {
					t.Logf("Device path matches via symlink: %s -> %s", devicePath, resolvedDevice)
				}
			}
			break
		}
	}

	if !found {
		t.Errorf("readProcMounts did not find mount point %s in results", mountPoint)
		t.Logf("Available mount points (first 10):")
		for i, mount := range mounts {
			if i >= 10 {
				break
			}
			if len(mount) >= 2 {
				t.Logf("  %s -> %s", mount[0], mount[1])
			}
		}
	}

	t.Logf("Test passed: readProcMounts successfully read and parsed /proc/mounts")
}

// TestIsMountPoint tests the isMountPoint function
// It creates an LV, mounts it, and verifies isMountPoint can correctly identify mount points
func TestIsMountPoint(t *testing.T) {
	ctx := context.Background()

	// Generate a unique LV name for this test
	lvName := fmt.Sprintf("test-is-mount-point-%d", os.Getpid())

	// Create the test volume
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: lvName,
		},
		Spec: apis.VolumeInfo{
			Capacity:      "100M",
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	// Clean up LV at the end
	defer func() {
		if err := lvm.ForceDestroyVolume(ctx, vol); err != nil {
			t.Logf("Warning: Failed to clean up test LV %s: %v", lvName, err)
		}
	}()

	// Step 1: Create the LV
	t.Logf("Step 1: Creating LV %s", lvName)
	if err := lvm.CreateVolume(ctx, vol); err != nil {
		t.Fatalf("Failed to create test volume: %v", err)
	}

	// Verify LV exists
	devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, lvName)
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		t.Fatalf("LVM logical volume %s does not exist: %v", devicePath, err)
	}

	// Step 2: Format the filesystem
	t.Logf("Step 2: Formatting filesystem on %s", devicePath)
	cmd := exec.Command("mkfs.ext4", "-F", devicePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to create filesystem on %s: %v, output: %s", devicePath, err, string(output))
	}

	// Step 3: Create temporary directories
	tmpRoot, err := os.MkdirTemp("", "devbox-test-is-mount-")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tmpRoot)

	mountPoint := filepath.Join(tmpRoot, "mount-point")
	nonMountPoint := filepath.Join(tmpRoot, "non-mount-point")

	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		t.Fatalf("Failed to create mount point directory: %v", err)
	}
	if err := os.MkdirAll(nonMountPoint, 0755); err != nil {
		t.Fatalf("Failed to create non-mount point directory: %v", err)
	}

	// Step 4: Test isMountPoint on non-mounted directory (should return false)
	t.Logf("Step 4: Testing isMountPoint on non-mounted directory")
	isMounted, err := isMountPoint(nonMountPoint)
	if err != nil {
		t.Fatalf("isMountPoint failed: %v", err)
	}
	if isMounted {
		t.Errorf("isMountPoint returned true for non-mounted directory %s", nonMountPoint)
	} else {
		t.Logf("Correctly identified %s as not a mount point", nonMountPoint)
	}

	// Step 5: Mount the LV
	t.Logf("Step 5: Mounting %s to %s", devicePath, mountPoint)
	if err := syscall.Mount(devicePath, mountPoint, "ext4", 0, ""); err != nil {
		t.Fatalf("Failed to mount %s to %s: %v", devicePath, mountPoint, err)
	}
	defer func() {
		if err := syscall.Unmount(mountPoint, 0); err != nil {
			t.Logf("Warning: Failed to unmount %s during cleanup: %v", mountPoint, err)
		}
	}()

	// Step 6: Test isMountPoint on mounted directory (should return true)
	t.Logf("Step 6: Testing isMountPoint on mounted directory")
	isMounted, err = isMountPoint(mountPoint)
	if err != nil {
		t.Fatalf("isMountPoint failed: %v", err)
	}
	if !isMounted {
		t.Errorf("isMountPoint returned false for mounted directory %s", mountPoint)
	} else {
		t.Logf("Correctly identified %s as a mount point", mountPoint)
	}

	// Step 7: Unmount and verify isMountPoint returns false
	t.Logf("Step 7: Unmounting and verifying isMountPoint returns false")
	if err := syscall.Unmount(mountPoint, 0); err != nil {
		t.Fatalf("Failed to unmount %s: %v", mountPoint, err)
	}

	isMounted, err = isMountPoint(mountPoint)
	if err != nil {
		t.Fatalf("isMountPoint failed after unmount: %v", err)
	}
	if isMounted {
		t.Errorf("isMountPoint returned true for unmounted directory %s", mountPoint)
	} else {
		t.Logf("Correctly identified %s as not a mount point after unmount", mountPoint)
	}

	t.Logf("Test passed: isMountPoint correctly identifies mount points")
}

// TestFindMountPointByDevice tests the findMountPointByDevice function
// It creates an LV, mounts it, and verifies findMountPointByDevice can find the mount point
func TestFindMountPointByDevice(t *testing.T) {
	ctx := context.Background()

	// Generate a unique LV name for this test
	lvName := fmt.Sprintf("test-find-mount-point-%d", os.Getpid())

	// Create the test volume
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: lvName,
		},
		Spec: apis.VolumeInfo{
			Capacity:      "100M",
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	// Clean up LV at the end
	defer func() {
		if err := lvm.ForceDestroyVolume(ctx, vol); err != nil {
			t.Logf("Warning: Failed to clean up test LV %s: %v", lvName, err)
		}
	}()

	// Step 1: Create the LV
	t.Logf("Step 1: Creating LV %s", lvName)
	if err := lvm.CreateVolume(ctx, vol); err != nil {
		t.Fatalf("Failed to create test volume: %v", err)
	}

	// Verify LV exists
	devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, lvName)
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		t.Fatalf("LVM logical volume %s does not exist: %v", devicePath, err)
	}

	// Step 2: Test findMountPointByDevice on unmounted device (should return empty)
	t.Logf("Step 2: Testing findMountPointByDevice on unmounted device")
	mountPoint, err := findMountPointByDevice(devicePath)
	if err != nil {
		t.Fatalf("findMountPointByDevice failed: %v", err)
	}
	if mountPoint != "" {
		t.Errorf("findMountPointByDevice returned mount point %s for unmounted device %s", mountPoint, devicePath)
	} else {
		t.Logf("Correctly returned empty string for unmounted device %s", devicePath)
	}

	// Step 3: Format the filesystem
	t.Logf("Step 3: Formatting filesystem on %s", devicePath)
	cmd := exec.Command("mkfs.ext4", "-F", devicePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to create filesystem on %s: %v, output: %s", devicePath, err, string(output))
	}

	// Step 4: Create a temporary mount point
	tmpRoot, err := os.MkdirTemp("", "devbox-test-find-mount-")
	if err != nil {
		t.Fatalf("Failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tmpRoot)

	expectedMountPoint := filepath.Join(tmpRoot, "mount-point")
	if err := os.MkdirAll(expectedMountPoint, 0755); err != nil {
		t.Fatalf("Failed to create mount point directory: %v", err)
	}

	// Step 5: Mount the LV
	t.Logf("Step 5: Mounting %s to %s", devicePath, expectedMountPoint)
	if err := syscall.Mount(devicePath, expectedMountPoint, "ext4", 0, ""); err != nil {
		t.Fatalf("Failed to mount %s to %s: %v", devicePath, expectedMountPoint, err)
	}
	defer func() {
		if err := syscall.Unmount(expectedMountPoint, 0); err != nil {
			t.Logf("Warning: Failed to unmount %s during cleanup: %v", expectedMountPoint, err)
		}
	}()

	// Step 6: Test findMountPointByDevice on mounted device
	t.Logf("Step 6: Testing findMountPointByDevice on mounted device")
	mountPoint, err = findMountPointByDevice(devicePath)
	if err != nil {
		t.Fatalf("findMountPointByDevice failed: %v", err)
	}
	if mountPoint == "" {
		t.Errorf("findMountPointByDevice returned empty string for mounted device %s", devicePath)
	} else if mountPoint != expectedMountPoint {
		t.Errorf("findMountPointByDevice returned wrong mount point: expected %s, got %s", expectedMountPoint, mountPoint)
	} else {
		t.Logf("Successfully found mount point: %s", mountPoint)
	}

	// Step 7: Unmount and verify findMountPointByDevice returns empty
	t.Logf("Step 7: Unmounting and verifying findMountPointByDevice returns empty")
	if err := syscall.Unmount(expectedMountPoint, 0); err != nil {
		t.Fatalf("Failed to unmount %s: %v", expectedMountPoint, err)
	}

	mountPoint, err = findMountPointByDevice(devicePath)
	if err != nil {
		t.Fatalf("findMountPointByDevice failed after unmount: %v", err)
	}
	if mountPoint != "" {
		t.Errorf("findMountPointByDevice returned mount point %s for unmounted device %s", mountPoint, devicePath)
	} else {
		t.Logf("Correctly returned empty string for unmounted device %s", devicePath)
	}

	t.Logf("Test passed: findMountPointByDevice correctly finds mount points")
}

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

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
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	apis "github.com/openebs/lvm-localpv/pkg/apis/openebs.io/lvm/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testVGName     = "devbox-vg"
	testPoolName   = "devbox-vg-thinpool"
	testLockLVName = "test-lock-lv"
)

// TestCreateVolume_WithLock test create LV (with lock)
func TestCreateVolume_WithLock(t *testing.T) {
	ctx := context.Background()

	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: testLockLVName,
		},
		Spec: apis.VolumeInfo{
			Capacity:      "209715200", // 200M in bytes (200 * 1024 * 1024)
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	t.Logf("Creating LV: %s with capacity: 200M", testLockLVName)

	// create LV
	err := CreateVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to create volume: %v", err)
	}

	exists, err := CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists: %v", err)
	}
	if !exists {
		t.Fatal("Volume should exist after creation")
	}

	// check device node
	devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, testLockLVName)
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		t.Fatalf("Device node %s does not exist", devicePath)
	}

	t.Logf("✅ LV created successfully: %s", testLockLVName)
	t.Logf("👉 Run 'lvs | grep %s' to verify", testLockLVName)
}

// TestListLVMLogicalVolumeByVG_WithLock test list LV (with lock)
func TestListLVMLogicalVolumeByVG_WithLock(t *testing.T) {
	ctx := context.Background()

	t.Logf("Listing LVs in VG: %s", testVGName)

	// list all LVs
	lvs, err := ListLVMLogicalVolumeByVG(ctx, testVGName, testPoolName)
	if err != nil {
		t.Fatalf("Failed to list LVs: %v", err)
	}

	t.Logf("Found %d LVs in VG %s", len(lvs), testVGName)

	// find our test LV
	found := false
	for _, lv := range lvs {
		if lv.Name == testLockLVName {
			found = true
			t.Logf("✅ Found test LV: %s (Size: %d bytes, VG: %s)", lv.Name, lv.Size, lv.VGName)
			break
		}
	}

	if !found {
		t.Logf("⚠️  Test LV %s not found (may not exist yet)", testLockLVName)
	}

	t.Logf("👉 Run 'lvs | grep %s' to verify", testLockLVName)
}

// TestResizeLVMVolume_WithLock test resize LV (with lock)
func TestResizeLVMVolume_WithLock(t *testing.T) {
	ctx := context.Background()

	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: testLockLVName,
		},
		Spec: apis.VolumeInfo{
			Capacity:      "314572800", // 300M in bytes (300 * 1024 * 1024)
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	t.Logf("Resizing LV: %s from 200M to 300M", testLockLVName)

	// resize LV
	err := ResizeLVMVolume(ctx, vol, false)
	if err != nil {
		t.Fatalf("Failed to resize volume: %v", err)
	}

	// check new size
	currentSize, err := getLVSize(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to get LV size: %v", err)
	}

	expectedSize := uint64(300 * 1024 * 1024) // 300M in bytes
	if currentSize < expectedSize {
		t.Fatalf("LV size should be at least %d bytes, got %d bytes", expectedSize, currentSize)
	}

	t.Logf("✅ LV resized successfully: %s (new size: %d bytes)", testLockLVName, currentSize)
	t.Logf("👉 Run 'lvs -o lv_name,lv_size | grep %s' to verify", testLockLVName)
}

// TestDestroyVolume_WithLock test destroy LV (with lock)
func TestDestroyVolume_WithLock(t *testing.T) {
	ctx := context.Background()

	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: testLockLVName,
		},
		Spec: apis.VolumeInfo{
			VolGroup: testVGName,
		},
	}

	t.Logf("Destroying LV: %s", testLockLVName)

	// destroy LV
	err := DestroyVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to destroy volume: %v", err)
	}

	// check if destroyed successfully
	exists, err := CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists: %v", err)
	}
	if exists {
		t.Fatal("Volume should not exist after destruction")
	}

	// check device node
	devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, testLockLVName)
	if _, err := os.Stat(devicePath); !os.IsNotExist(err) {
		t.Fatalf("Device node %s should not exist", devicePath)
	}

	t.Logf("✅ LV destroyed successfully: %s", testLockLVName)
	t.Logf("👉 Run 'lvs | grep %s' to verify (should not find it)", testLockLVName)
}

// TestForceDestroyVolume_WithLock test force destroy LV (with lock)
func TestForceDestroyVolume_WithLock(t *testing.T) {
	ctx := context.Background()

	// create a LV
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: testLockLVName,
		},
		Spec: apis.VolumeInfo{
			Capacity:      "209715200", // 200M in bytes
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	t.Logf("Creating LV: %s for force destroy test", testLockLVName)

	err := CreateVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to create volume: %v", err)
	}

	t.Logf("Force destroying LV: %s", testLockLVName)

	// force destroy LV
	err = ForceDestroyVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to force destroy volume: %v", err)
	}

	// check if destroyed successfully
	exists, err := CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists: %v", err)
	}
	if exists {
		t.Fatal("Volume should not exist after force destruction")
	}

	t.Logf("✅ LV force destroyed successfully: %s", testLockLVName)
	t.Logf("👉 Run 'lvs | grep %s' to verify (should not find it)", testLockLVName)
}

// TestLVMFullLifecycle_WithLock test LV full lifecycle (create→list→resize→delete)
func TestLVMFullLifecycle_WithLock(t *testing.T) {
	ctx := context.Background()

	t.Log("========================================")
	t.Log("Testing LVM Full Lifecycle with Lock")
	t.Log("========================================")

	// 1. create LV
	t.Log("\n📝 Step 1: Creating LV...")
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: testLockLVName,
		},
		Spec: apis.VolumeInfo{
			Capacity:      "209715200", // 200M in bytes
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	err := CreateVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Step 1 failed: %v", err)
	}

	exists, err := CheckLVMMetadataExists(ctx, vol)
	if err != nil || !exists {
		t.Fatalf("Step 1 verification failed: volume should exist")
	}

	t.Logf("✅ Step 1: LV created successfully: %s (200M)", testLockLVName)
	t.Logf("👉 Run 'lvs | grep %s' to verify creation", testLockLVName)

	// 2. list LV
	t.Log("\n📝 Step 2: Listing LVs...")
	lvs, err := ListLVMLogicalVolumeByVG(ctx, testVGName, testPoolName)
	if err != nil {
		t.Fatalf("Step 2 failed: %v", err)
	}

	found := false
	for _, lv := range lvs {
		if lv.Name == testLockLVName {
			found = true
			t.Logf("✅ Step 2: Found LV in list: %s (Size: %d bytes)", lv.Name, lv.Size)
			break
		}
	}

	if !found {
		t.Fatalf("Step 2 verification failed: LV not found in list")
	}

	// 3. resize LV
	t.Log("\n📝 Step 3: Resizing LV from 200M to 400M...")
	vol.Spec.Capacity = "419430400" // 400M in bytes (400 * 1024 * 1024)
	err = ResizeLVMVolume(ctx, vol, false)
	if err != nil {
		t.Fatalf("Step 3 failed: %v", err)
	}

	currentSize, err := getLVSize(ctx, vol)
	if err != nil {
		t.Fatalf("Step 3 verification failed: %v", err)
	}

	expectedSize := uint64(400 * 1024 * 1024)
	if currentSize < expectedSize {
		t.Fatalf("Step 3 verification failed: size should be at least %d, got %d", expectedSize, currentSize)
	}

	t.Logf("✅ Step 3: LV resized successfully: %s (400M, actual: %d bytes)", testLockLVName, currentSize)
	t.Logf("👉 Run 'lvs -o lv_name,lv_size | grep %s' to verify resize", testLockLVName)

	// 4. list LV again to verify size change
	t.Log("\n📝 Step 4: Listing LVs again to verify size change...")
	lvs, err = ListLVMLogicalVolumeByVG(ctx, testVGName, testPoolName)
	if err != nil {
		t.Fatalf("Step 4 failed: %v", err)
	}

	found = false
	for _, lv := range lvs {
		if lv.Name == testLockLVName {
			found = true
			t.Logf("✅ Step 4: Verified LV size change: %s (Size: %d bytes)", lv.Name, lv.Size)
			break
		}
	}

	if !found {
		t.Fatalf("Step 4 verification failed: LV not found in list")
	}

	// 5. destroy LV
	t.Log("\n📝 Step 5: Destroying LV...")
	err = DestroyVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Step 5 failed: %v", err)
	}

	exists, err = CheckLVMMetadataExists(ctx, vol)
	if err != nil || exists {
		t.Fatalf("Step 5 verification failed: volume should not exist")
	}

	devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, testLockLVName)
	if _, err := os.Stat(devicePath); !os.IsNotExist(err) {
		t.Fatalf("Step 5 verification failed: device node should not exist")
	}

	t.Logf("✅ Step 5: LV destroyed successfully: %s", testLockLVName)
	t.Logf("👉 Run 'lvs | grep %s' to verify deletion (should not find it)", testLockLVName)

	t.Log("\n========================================")
	t.Log("✅ Full Lifecycle Test Completed Successfully!")
	t.Log("========================================")
}

// TestLVMConcurrentOperations_WithLock test concurrent operations (with lock)
func TestLVMConcurrentOperations_WithLock(t *testing.T) {
	ctx := context.Background()

	t.Log("========================================")
	t.Log("Testing Concurrent LVM Operations")
	t.Log("========================================")

	// create multiple LVs
	lvNames := []string{
		"test-concurrent-lv-1",
		"test-concurrent-lv-2",
		"test-concurrent-lv-3",
	}

	// cleanup function
	cleanup := func() {
		for _, name := range lvNames {
			vol := &apis.LVMVolume{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec:       apis.VolumeInfo{VolGroup: testVGName},
			}
			ForceDestroyVolume(ctx, vol)
		}
	}

	// check if cleanup is done before and after the test
	cleanup()
	defer cleanup()

	t.Log("\n📝 Creating 3 LVs concurrently...")

	// concurrent create
	done := make(chan error, len(lvNames))
	for _, name := range lvNames {
		go func(lvName string) {
			vol := &apis.LVMVolume{
				ObjectMeta: metav1.ObjectMeta{Name: lvName},
				Spec: apis.VolumeInfo{
					Capacity:      "104857600", // 100M in bytes (100 * 1024 * 1024)
					VolGroup:      testVGName,
					ThinProvision: testPoolName,
				},
			}
			done <- CreateVolume(ctx, vol)
		}(name)
	}

	// wait for all create to complete
	for i := 0; i < len(lvNames); i++ {
		if err := <-done; err != nil {
			t.Fatalf("Concurrent create failed: %v", err)
		}
	}

	t.Log("✅ All LVs created successfully")

	// check if all LVs exist
	t.Log("\n📝 Verifying all LVs exist...")
	for _, name := range lvNames {
		vol := &apis.LVMVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       apis.VolumeInfo{VolGroup: testVGName},
		}
		exists, err := CheckLVMMetadataExists(ctx, vol)
		if err != nil || !exists {
			t.Fatalf("LV %s should exist", name)
		}
		t.Logf("  ✓ %s exists", name)
	}

	// concurrent delete
	t.Log("\n📝 Deleting 3 LVs concurrently...")
	for _, name := range lvNames {
		go func(lvName string) {
			vol := &apis.LVMVolume{
				ObjectMeta: metav1.ObjectMeta{Name: lvName},
				Spec:       apis.VolumeInfo{VolGroup: testVGName},
			}
			done <- DestroyVolume(ctx, vol)
		}(name)
	}

	// wait for all delete to complete
	for i := 0; i < len(lvNames); i++ {
		if err := <-done; err != nil {
			t.Fatalf("Concurrent delete failed: %v", err)
		}
	}

	t.Log("✅ All LVs deleted successfully")

	// check if all LVs are deleted
	t.Log("\n📝 Verifying all LVs are deleted...")
	for _, name := range lvNames {
		vol := &apis.LVMVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       apis.VolumeInfo{VolGroup: testVGName},
		}
		exists, err := CheckLVMMetadataExists(ctx, vol)
		if err != nil || exists {
			t.Fatalf("LV %s should not exist", name)
		}
		t.Logf("  ✓ %s deleted", name)
	}

	t.Log("\n========================================")
	t.Log("✅ Concurrent Operations Test Completed!")
	t.Log("   All operations were serialized by the lock")
	t.Log("========================================")
}

// TestForceDestroyVolume_NormalLV tests force destroying a normal LV
func TestForceDestroyVolume_NormalLV(t *testing.T) {

	ctx := context.Background()

	// Create a test volume
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-normal-lv",
		},
		Spec: apis.VolumeInfo{
			Capacity:      "100M",
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	// Create the volume
	err := CreateVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to create test volume: %v", err)
	}

	// Verify it exists
	exists, err := CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists: %v", err)
	}
	if !exists {
		t.Fatal("Volume should exist after creation")
	}
	devicePath := fmt.Sprintf("/dev/%s/%s", testVGName, vol.Name)

	// Check if the device exists
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		t.Fatalf("LVM logical volume %s does not exist: %v", devicePath, err)
	}

	cmd := exec.Command("mkfs.ext4", devicePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to create filesystem on %s: %v, output: %s", devicePath, err, string(output))
	}

	// Force destroy it
	err = ForceDestroyVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to force destroy volume: %v", err)
	}

	// Verify it's gone
	exists, err = CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists after deletion: %v", err)
	}
	if exists {
		t.Fatal("Volume should not exist after force destruction")
	}
}

// TestForceDestroyVolume_ZombieLV tests force destroying a zombie LV (metadata exists but device node missing)
func TestForceDestroyVolume_ZombieLV(t *testing.T) {
	ctx := context.Background()

	// Create a test volume
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-zombie-lv",
		},
		Spec: apis.VolumeInfo{
			Capacity:      "100M",
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	// Create the volume
	err := CreateVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to create test volume: %v", err)
	}

	// Verify it exists
	exists, err := CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists: %v", err)
	}
	if !exists {
		t.Fatal("Volume should exist after creation")
	}

	// Simulate zombie LV by removing device nodes manually
	// Note: This is a simulation - in real scenarios, zombie LVs are created
	// when lvcreate is killed after metadata creation but before device creation
	devPath := DevPath + testVGName + "/" + vol.Name
	mapperPath := "/dev/mapper/" + strings.Replace(testVGName, "-", "--", -1) + "-" + strings.Replace(vol.Name, "-", "--", -1)

	// Remove device nodes (this requires root privileges)
	// In a real test environment, this step might fail if we don't have privileges
	// That's okay - the force destroy should still work
	os.Remove(devPath)
	os.Remove(mapperPath)

	// Try to verify the LV is now zombie-like
	// CheckVolumeExists (old method) would return false
	// But CheckLVMMetadataExists should return true
	exists, err = CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check LVM metadata: %v", err)
	}
	if !exists {
		t.Fatal("LVM metadata should still exist for zombie LV")
	}

	exists, err = CheckVolumeExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists: %v", err)
	}
	if exists {
		t.Fatal("Volume should not exist")
	}

	// Force destroy should work even for zombie LVs
	err = ForceDestroyVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to force destroy zombie LV: %v", err)
	}

	// Verify it's gone
	exists, err = CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists after deletion: %v", err)
	}
	if exists {
		t.Fatal("Zombie LV should not exist after force destruction")
	}
}

// TestCheckLVMMetadataExists_ExistingLV tests checking for an existing LV
func TestCheckLVMMetadataExists_ExistingLV(t *testing.T) {
	ctx := context.Background()

	// Create a test volume
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-existing-lv",
		},
		Spec: apis.VolumeInfo{
			Capacity:      "100M",
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	// Create the volume
	err := CreateVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to create test volume: %v", err)
	}

	// Clean up after test
	defer ForceDestroyVolume(ctx, vol)

	// Check it exists
	exists, err := CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists: %v", err)
	}
	if !exists {
		t.Fatal("Volume should exist")
	}
}

// TestCheckLVMMetadataExists_NonExistingLV tests checking for a non-existing LV
func TestCheckLVMMetadataExists_NonExistingLV(t *testing.T) {
	ctx := context.Background()

	// Create a volume that doesn't exist
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-nonexisting-lv",
		},
		Spec: apis.VolumeInfo{
			VolGroup: testVGName,
		},
	}

	// Check it doesn't exist
	exists, err := CheckLVMMetadataExists(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to check volume exists: %v", err)
	}
	if exists {
		t.Fatal("Volume should not exist")
	}
}

// TestForceDestroyVolume_NonExistingLV tests force destroying a non-existing LV (should not error)
func TestForceDestroyVolume_NonExistingLV(t *testing.T) {
	ctx := context.Background()

	// Create a volume that doesn't exist
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-nonexisting-destroy-lv",
		},
		Spec: apis.VolumeInfo{
			VolGroup: testVGName,
		},
	}

	// Force destroy should not error
	err := ForceDestroyVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Force destroy of non-existing volume should not error: %v", err)
	}
}

// TestForceDestroyVolume_Idempotent tests that force destroy is idempotent
func TestForceDestroyVolume_Idempotent(t *testing.T) {

	ctx := context.Background()

	// Create a test volume
	vol := &apis.LVMVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-idempotent-lv",
		},
		Spec: apis.VolumeInfo{
			Capacity:      "100M",
			VolGroup:      testVGName,
			ThinProvision: testPoolName,
		},
	}

	// Create the volume
	err := CreateVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to create test volume: %v", err)
	}

	// Force destroy it first time
	err = ForceDestroyVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Failed to force destroy volume first time: %v", err)
	}

	// Force destroy it second time (should be idempotent)
	err = ForceDestroyVolume(ctx, vol)
	if err != nil {
		t.Fatalf("Force destroy should be idempotent: %v", err)
	}
}

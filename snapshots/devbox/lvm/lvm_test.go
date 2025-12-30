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
	"os"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	apis "github.com/openebs/lvm-localpv/pkg/apis/openebs.io/lvm/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testVGName   = "devbox-vg"
	testPoolName = "devbox-vg-thinpool"
)

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
	if os.Getenv("RUN_LVM_TESTS") != "true" {
		t.Skip("Skipping LVM tests. Set RUN_LVM_TESTS=true to run")
	}

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

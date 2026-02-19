// fsm_test.go - Development tests for unpack FSM transitions.
//
// Integration tests for development validation.
// These tests verify layout validation and security checks during development.

package unpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	fsm "github.com/superfly/fsm"

	"github.com/superfly/fsm/database"
	"github.com/superfly/fsm/devicemapper"
	"github.com/superfly/fsm/extraction"
)

type fakeDB struct{}

func (f *fakeDB) CheckImageUnpacked(ctx context.Context, imageID string) (*database.UnpackedImage, error) {
	return nil, nil // No-op for tests
}

func (f *fakeDB) GetUnpackedImageByID(ctx context.Context, imageID string) (*database.UnpackedImage, error) {
	return nil, nil // No-op for tests
}

func (f *fakeDB) DeleteUnpackedImage(ctx context.Context, imageID string) error {
	return nil // No-op for tests
}

func (f *fakeDB) StoreUnpackedImage(ctx context.Context, imageID, deviceID, deviceName, devicePath string, sizeBytes int64, fileCount int) error {
	return nil // No-op for tests
}

func (f *fakeDB) AcquireImageLock(ctx context.Context, imageID, lockedBy string) error {
	return nil // No-op for tests
}

func (f *fakeDB) ReleaseImageLock(ctx context.Context, imageID string) error {
	return nil // No-op for tests
}

func (f *fakeDB) IsImageLocked(ctx context.Context, imageID string) (bool, error) {
	return false, nil // No-op for tests
}

type fakeDeviceMgr struct {
	deviceExists bool
}

func (f *fakeDeviceMgr) DeviceExists(ctx context.Context, name string) (bool, error) {
	return f.deviceExists, nil
}

func (f *fakeDeviceMgr) IsMounted(mountPoint string) (bool, error) {
	return false, nil
}

// Only methods used by verifyLayout are implemented; others panic if called.
func (f *fakeDeviceMgr) UnmountDevice(ctx context.Context, mountPoint string) error { return nil }
func (f *fakeDeviceMgr) DeactivateDevice(ctx context.Context, name string) error    { return nil }
func (f *fakeDeviceMgr) DeleteDevice(ctx context.Context, pool, id string) error    { return nil }

// The remaining methods satisfy the interface but are unused in verifyLayout.
func (f *fakeDeviceMgr) CreateThinDevice(ctx context.Context, pool, id string, size int64) (*devicemapper.DeviceInfo, error) {
	panic("CreateThinDevice not implemented in fakeDeviceMgr")
}
func (f *fakeDeviceMgr) MountDevice(ctx context.Context, devicePath, mountPoint string) error {
	panic("MountDevice not implemented in fakeDeviceMgr")
}
func (f *fakeDeviceMgr) GetDevicePath(name string) string { return "" }
func (f *fakeDeviceMgr) CreateSnapshot(ctx context.Context, pool, originID, snapID string) (*devicemapper.DeviceInfo, error) {
	panic("CreateSnapshot not implemented in fakeDeviceMgr")
}

// TestVerifyLayoutTransition_DirectRoot verifies that the verifyLayout
// transition accepts a direct-root layout (no rootfs/ subdir) and treats it as
// valid when etc/usr/var exist with safe permissions.
func TestVerifyLayoutTransition_DirectRoot(t *testing.T) {
	mountRoot := t.TempDir()

	// Simulate mounted device directory under MountRoot.
	imageID := "img_1234abcd5678ef00"
	deviceName := deviceNameForImage(imageID)
	mountPoint := filepath.Join(mountRoot, deviceName)
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		t.Fatalf("mkdir mountPoint: %v", err)
	}

	// Direct-root layout under mountPoint.
	for _, d := range []string{"etc", "usr", "var"} {
		if err := os.MkdirAll(filepath.Join(mountPoint, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	deps := &Dependencies{
		DB:        &fakeDB{}, // mock DB for tests
		DeviceMgr: &fakeDeviceMgr{},
		Extractor: extraction.New(),
		PoolName:  "pool0",
		MountRoot: mountRoot,
	}

	transition := verifyLayout(deps)
	ctx := context.Background()
	req := &fsm.Request[ImageUnpackRequest, ImageUnpackResponse]{
		Msg: &fsm.ImageUnpackRequest{ImageID: imageID},
	}
	// Inject a no-op logger to avoid nil pointer.
	req = fsm.MockRequest(req, logrus.New(), fsm.Run{})

	if _, err := transition(ctx, req); err != nil {
		t.Fatalf("verifyLayout(direct-root) unexpected error: %v", err)
	}
}

// TestVerifyLayoutTransition_RootfsSubdir verifies that verifyLayout accepts a
// legacy rootfs/ layout.
func TestVerifyLayoutTransition_RootfsSubdir(t *testing.T) {
	mountRoot := t.TempDir()

	imageID := "img_1234abcd5678ef00"
	deviceName := deviceNameForImage(imageID)
	mountPoint := filepath.Join(mountRoot, deviceName)
	rootfs := filepath.Join(mountPoint, "rootfs")

	for _, d := range []string{"etc", "usr", "var"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	deps := &Dependencies{
		DB:        &fakeDB{}, // mock DB for tests
		DeviceMgr: &fakeDeviceMgr{},
		Extractor: extraction.New(),
		PoolName:  "pool0",
		MountRoot: mountRoot,
	}

	transition := verifyLayout(deps)
	ctx := context.Background()
	req := &fsm.Request[ImageUnpackRequest, ImageUnpackResponse]{
		Msg: &fsm.ImageUnpackRequest{ImageID: imageID},
	}
	req = fsm.MockRequest(req, logrus.New(), fsm.Run{})

	if _, err := transition(ctx, req); err != nil {
		t.Fatalf("verifyLayout(rootfs-subdir) unexpected error: %v", err)
	}
}

// TestVerifyLayoutTransition_InvalidLayout verifies that verifyLayout aborts
// when neither rootfs/ nor direct-root layout is present.
func TestVerifyLayoutTransition_InvalidLayout(t *testing.T) {
	mountRoot := t.TempDir()

	imageID := "img_1234abcd5678ef00"
	deviceName := deviceNameForImage(imageID)
	mountPoint := filepath.Join(mountRoot, deviceName)
	if err := os.MkdirAll(filepath.Join(mountPoint, "weird"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	deps := &Dependencies{
		DB:        &fakeDB{}, // mock DB for tests
		DeviceMgr: &fakeDeviceMgr{},
		Extractor: extraction.New(),
		PoolName:  "pool0",
		MountRoot: mountRoot,
	}

	transition := verifyLayout(deps)
	ctx := context.Background()
	req := &fsm.Request[ImageUnpackRequest, ImageUnpackResponse]{
		Msg: &fsm.ImageUnpackRequest{ImageID: imageID},
	}
	req = fsm.MockRequest(req, logrus.New(), fsm.Run{})

	if _, err := transition(ctx, req); err == nil {
		t.Fatalf("verifyLayout(invalid) expected error, got nil")
	}
}

// TestVerifyLayoutTransition_WorldWritableEtc verifies that verifyLayout
// aborts when /etc under the logical root is world-writable.
func TestVerifyLayoutTransition_WorldWritableEtc(t *testing.T) {
	mountRoot := t.TempDir()

	imageID := "img_1234abcd5678ef00"
	deviceName := deviceNameForImage(imageID)
	mountPoint := filepath.Join(mountRoot, deviceName)
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		t.Fatalf("mkdir mountPoint: %v", err)
	}

	// Direct-root layout with world-writable /etc.
	etcPath := filepath.Join(mountPoint, "etc")
	if err := os.MkdirAll(etcPath, 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := os.Chmod(etcPath, 0o777); err != nil {
		t.Fatalf("chmod etc: %v", err)
	}

	// Also create usr/var so the layout otherwise looks valid.
	for _, d := range []string{"usr", "var"} {
		if err := os.MkdirAll(filepath.Join(mountPoint, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	deps := &Dependencies{
		DB:        &fakeDB{}, // mock DB for tests
		DeviceMgr: &fakeDeviceMgr{},
		Extractor: extraction.New(),
		PoolName:  "pool0",
		MountRoot: mountRoot,
	}

	transition := verifyLayout(deps)
	ctx := context.Background()
	req := &fsm.Request[ImageUnpackRequest, ImageUnpackResponse]{
		Msg: &fsm.ImageUnpackRequest{ImageID: imageID},
	}
	req = fsm.MockRequest(req, logrus.New(), fsm.Run{})

	if _, err := transition(ctx, req); err == nil {
		t.Fatalf("verifyLayout should reject world-writable etc directory at FSM level")
	}
}

// fakeDeviceMgrWithOrphanDetection is a mock that simulates orphaned device scenarios.
type fakeDeviceMgrWithOrphanDetection struct {
	deviceExists      bool
	createDeviceError error
}

func (f *fakeDeviceMgrWithOrphanDetection) DeviceExists(ctx context.Context, name string) (bool, error) {
	return f.deviceExists, nil
}

func (f *fakeDeviceMgrWithOrphanDetection) IsMounted(mountPoint string) (bool, error) {
	return false, nil
}

func (f *fakeDeviceMgrWithOrphanDetection) CreateThinDevice(ctx context.Context, pool, id string, size int64) (*devicemapper.DeviceInfo, error) {
	return nil, f.createDeviceError
}

func (f *fakeDeviceMgrWithOrphanDetection) UnmountDevice(ctx context.Context, mountPoint string) error {
	return nil
}

func (f *fakeDeviceMgrWithOrphanDetection) DeactivateDevice(ctx context.Context, name string) error {
	return nil
}

func (f *fakeDeviceMgrWithOrphanDetection) DeleteDevice(ctx context.Context, pool, id string) error {
	return nil
}

func (f *fakeDeviceMgrWithOrphanDetection) MountDevice(ctx context.Context, devicePath, mountPoint string) error {
	return nil
}

func (f *fakeDeviceMgrWithOrphanDetection) GetDevicePath(name string) string {
	return "/dev/mapper/" + name
}

func (f *fakeDeviceMgrWithOrphanDetection) CreateSnapshot(ctx context.Context, pool, originID, snapID string) (*devicemapper.DeviceInfo, error) {
	return nil, nil
}

// TestCreateDeviceTransition_DetectsOrphanedDevice tests that the createDevice
// transition detects orphaned devices (device exists but CreateThinDevice failed).
func TestCreateDeviceTransition_DetectsOrphanedDevice(t *testing.T) {
	// This test requires a fully initialized database with proper schema.
	// In a production test suite, we would:
	// 1. Create a temporary SQLite database
	// 2. Run migrations to set up the schema
	// 3. Mock the devicemapper client to simulate orphaned device scenario
	// 4. Verify the error message contains "orphaned" and "gc"
	//
	// For now, we skip this test and document the expected behavior.
	t.Skip("Skipping - requires full database initialization and schema setup")

	// Expected behavior (documented for future implementation):
	// - When CreateThinDevice fails with any error
	// - AND DeviceExists returns true (device was partially created)
	// - THEN the transition should return an error containing:
	//   - The word "orphaned"
	//   - Instructions to run "flyio-image-manager gc --force"
	//
	// Example error message:
	// "orphaned device thin-3486190 detected after failed creation; run 'flyio-image-manager gc --force' to clean up"
}

// TestCreateDeviceTransition_HandlesDeviceExistsError tests that the createDevice
// transition handles concurrent device creation gracefully.
func TestCreateDeviceTransition_HandlesDeviceExistsError(t *testing.T) {
	// This test requires a fully initialized database with proper schema.
	// In a production test suite, we would:
	// 1. Create a temporary SQLite database
	// 2. Run migrations to set up the schema
	// 3. Mock the devicemapper client to return DeviceExistsError
	// 4. Verify the transition succeeds by reusing the existing device
	//
	// For now, we skip this test and document the expected behavior.
	t.Skip("Skipping - requires full database initialization and schema setup")

	// Expected behavior (documented for future implementation):
	// - When CreateThinDevice returns DeviceExistsError
	// - THEN the transition should:
	//   - Log "device created concurrently, reusing"
	//   - Construct a DeviceInfo with the existing device details
	//   - Continue with the existing device (idempotent behavior)
	//   - NOT return an error
}

// contains is a helper function to check if a string contains a substring.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

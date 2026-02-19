// extract_test.go - Development tests for extraction package.
//
// Integration tests for development validation.
// These tests exist solely for development verification of security checks
// (layout validation, world-writable directory detection, path traversal prevention).

package extraction

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyLayout_DirectRootSuccess verifies that VerifyLayout accepts a
// standard OCI layout where the root filesystem lives directly under the
// mount root (etc/, usr/, var/).
func TestVerifyLayout_DirectRootSuccess(t *testing.T) {
	t.TempDir()
	ctx := context.Background()
	_ = ctx // reserved for future use if we wire Extract into tests

	root := t.TempDir()

	// Create minimal direct-root layout: etc/, usr/, var/ under root.
	for _, d := range []string{"etc", "usr", "var"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	ex := New()
	if err := ex.VerifyLayout(root); err != nil {
		t.Fatalf("VerifyLayout(direct-root) unexpected error: %v", err)
	}
}

// TestVerifyLayout_RootfsSubdirSuccess verifies that VerifyLayout accepts the
// legacy layout with a rootfs/ subdirectory.
func TestVerifyLayout_RootfsSubdirSuccess(t *testing.T) {
	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")

	for _, d := range []string{"etc", "usr", "var"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	ex := New()
	if err := ex.VerifyLayout(root); err != nil {
		t.Fatalf("VerifyLayout(rootfs-subdir) unexpected error: %v", err)
	}
}

// TestVerifyLayout_InvalidLayout verifies that VerifyLayout rejects a directory
// tree that does not look like either supported layout.
func TestVerifyLayout_InvalidLayout(t *testing.T) {
	root := t.TempDir()

	// No rootfs/, and no etc/usr/var at top-level.
	if err := os.MkdirAll(filepath.Join(root, "not-root"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ex := New()
	if err := ex.VerifyLayout(root); err == nil {
		t.Fatalf("VerifyLayout(invalid) expected error, got nil")
	}
}

// TestVerifyLayout_WorldWritableCriticalDir ensures world-writable critical
// directories are rejected.
func TestVerifyLayout_WorldWritableCriticalDir(t *testing.T) {
	root := t.TempDir()

	// direct-root layout with world-writable /etc. We need to explicitly chmod
	// after MkdirAll to avoid umask interference.
	etcPath := filepath.Join(root, "etc")
	if err := os.MkdirAll(etcPath, 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := os.Chmod(etcPath, 0o777); err != nil {
		t.Fatalf("chmod etc: %v", err)
	}

	ex := New()
	if err := ex.VerifyLayout(root); err == nil {
		t.Fatalf("VerifyLayout should reject world-writable etc directory")
	}
}

# Security Design

**Document Type**: Design
**Status**: Approved
**Version**: 1.1
**Last Updated**: 2025-11-24

---

## Overview

This document describes the security strategy for the Fly.io Container Image Management System. The system assumes a **hostile environment** and implements defense-in-depth with multiple validation layers.

**Security Principle**: Fail closed - reject suspicious content rather than allow it.

---

## Threat Model

### Assumptions

1. **Hostile Environment**: Container images may be malicious or corrupted
2. **Untrusted Input**: All S3 content is untrusted until validated
3. **Resource Attacks**: Attackers may attempt resource exhaustion
4. **Privilege Escalation**: Attackers may attempt to escape containers

### Threat Actors

- **Malicious Image Authors**: Create images with path traversal, symlink attacks
- **Compromised S3**: Tampered or corrupted images in S3 bucket
- **Resource Exhaustion Attackers**: Large files, many files, infinite loops

### Assets to Protect

- **Host Filesystem**: Prevent escaping rootfs via path traversal or symlinks
- **System Resources**: CPU, memory, disk space, I/O bandwidth
- **Database Integrity**: Prevent corruption or unauthorized access
- **DeviceMapper Pool**: Prevent pool exhaustion or corruption

---

## Security Layers

### Layer 1: Input Validation

**S3 Key Validation** (`s3/client.go:validateS3Key()`):
- No path traversal (`..` not allowed)
- No absolute paths (must be relative)
- Max length: 1024 characters
- No null bytes
- Alphanumeric + `-_./` only

**Device Name Validation** (`devicemapper/dm.go:validateDeviceName()`):
- Alphanumeric + `-_` only
- Max length: 255 characters
- No special characters (prevent command injection)

**Device ID Validation** (`devicemapper/dm.go:validateDeviceID()`):
- Numeric only
- Max length: 64 characters

### Layer 2: Resource Limits

**File Size Limits**:
- Max image size: 10GB
- Max file size: 1GB per file
- Max device size: 100GB

**File Count Limits**:
- Max files per image: 100,000

**Time Limits**:
- Download timeout: 5 minutes
- Extraction timeout: 30 minutes

**Concurrency Limits**:
- Max concurrent downloads: 5
- Max concurrent unpacking: 1 (reduced from 2 for devicemapper stability)

**Rationale**: Prevent resource exhaustion attacks and ensure predictable resource usage.

### Layer 3: Lock File Security

**Lock File Location and Permissions** (added 2025-11-24):
- **Location**: `<FSMDBPath>/flyio-manager.lock`
- **Permissions**: 0644 (readable by all, writable by owner)
- **Content**: JSON with PID, timestamp, command

**PID Validation and Stale Lock Detection**:
- Lock file contains process PID for diagnostics
- Stale lock detection: Check if PID still exists (future enhancement)
- Manual cleanup: Remove lock file if process is dead

**Race Condition Handling**:
- Atomic file creation: `os.OpenFile` with `O_CREATE|O_EXCL` flags
- TOCTOU protection: File existence check and creation are atomic
- Error on conflict: Returns descriptive error with existing lock info

**--ignore-lock Flag (DANGEROUS)**:
- **Purpose**: Override lock file check in GC command
- **Use Cases**:
  - Emergency cleanup when lock file is stale
  - Manual intervention after process crash
  - Maintenance windows when system is known to be idle
- **Danger**: Running GC while FSMs are active can cause kernel panics
- **Warning**: Logs prominent warning when flag is used

**Security Considerations**:
- Lock file is not cryptographically secure (no signing, no encryption)
- Relies on filesystem atomicity guarantees
- Vulnerable to malicious lock file deletion (requires filesystem access)
- Not suitable for distributed systems (single-host only)

**See Also**:
- [System Architecture - Concurrency Control](SYSTEM_ARCH.md#concurrency-control)
- [ADR-001 - Kernel Panic Mitigation](ADR-001-KERNEL-PANIC-MITIGATION.md)

### Layer 4: Tar Archive Validation

**Tar Structure Validation** (`download/fsm.go:validateTarStructure()`):
- Verify tar can be opened
- Verify tar format is valid
- Reject corrupted archives

**Tar Entry Validation** (`download/fsm.go:performSecurityChecks()`):
- **Path Traversal**: Reject paths containing `..`
- **Absolute Paths**: Reject absolute paths
- **Symlink Targets**: Reject absolute symlink targets
- **Symlink Escaping**: Reject symlinks with `..` in target
- **File Sizes**: Enforce 1GB per file limit
- **File Count**: Enforce 100,000 file limit

**Implementation**:
```go
// Check for path traversal
if strings.Contains(header.Name, "..") {
    return fmt.Errorf("path traversal detected: %s", header.Name)
}

// Check for absolute paths
if filepath.IsAbs(header.Name) {
    return fmt.Errorf("absolute path not allowed: %s", header.Name)
}

// Check symlink targets
if header.Typeflag == tar.TypeSymlink {
    if filepath.IsAbs(header.Linkname) {
        return fmt.Errorf("absolute symlink target: %s -> %s", 
            header.Name, header.Linkname)
    }
    if strings.Contains(header.Linkname, "..") {
        return fmt.Errorf("suspicious symlink: %s -> %s", 
            header.Name, header.Linkname)
    }
}
```

### Layer 4: Extraction Security

**Path Sanitization** (`extraction/extract.go:sanitizePath()`):
- Clean all paths with `filepath.Clean()`
- Reject absolute paths
- Reject paths containing `..`
- Verify paths stay within base directory

**Symlink Validation** (`extraction/extract.go:validateSymlinkTarget()`):
- Resolve symlink targets relative to link directory
- Verify targets stay within base directory
- Reject absolute symlink targets
- Reject symlinks with `..` in target

**Implementation**:
```go
func (e *Extractor) validateSymlinkTarget(baseDir, linkPath, target string) error {
    // Check for absolute symlink targets
    if filepath.IsAbs(target) {
        return fmt.Errorf("absolute symlink targets not allowed: %s", target)
    }

    // Resolve the symlink target relative to the link's directory
    linkDir := filepath.Dir(linkPath)
    targetPath := filepath.Join(linkDir, target)
    cleanTarget := filepath.Clean(targetPath)

    // Verify the target is within the base directory
    if !strings.HasPrefix(cleanTarget, filepath.Clean(baseDir)+string(os.PathSeparator)) {
        return fmt.Errorf("symlink target escapes base directory: %s -> %s", 
            linkPath, target)
    }

    return nil
}
```

### Layer 5: Filesystem Layout Validation

**Layout Verification** (`extraction/extract.go:VerifyLayout()`):
- **Required Directories**: Verify `rootfs/` exists
- **Expected Directories**: Check for `rootfs/etc/`, `rootfs/usr/`, `rootfs/var/`
- **Permission Checks**: Detect world-writable directories in critical paths
- **Setuid/Setgid Detection**: Log setuid/setgid binaries

These invariants are also enforced at the FSM level by the Unpack FSM's
`verify-layout` transition (`unpack/fsm.go`, see `FSM_FLOWS.md`), which treats
structural or permission violations as security failures and aborts with
cleanup (no retries).

**Permission Checks** (`extraction/extract.go:checkPermissions()`):
```go
// Check for world-writable directories in critical paths
if info.IsDir() {
    relPath, _ := filepath.Rel(destDir, path)
    if strings.HasPrefix(relPath, "rootfs/etc") || 
       strings.HasPrefix(relPath, "rootfs/usr") {
        if info.Mode().Perm()&0002 != 0 {
            return fmt.Errorf("world-writable directory in critical path: %s", 
                relPath)
        }
    }
}

// Check for setuid/setgid binaries
if info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 {
    logger.WithField("path", relPath).Warn("setuid/setgid binary found")
}
```

### Layer 6: Checksum Verification

**SHA256 Checksums**:
- Compute during download (single pass)
- Verify after download
- Store in database for future verification

**Implementation** (`s3/client.go:DownloadImage()`):
```go
// Stream to file while computing checksum
hash := sha256.New()
multiWriter := io.MultiWriter(tmpFile, hash)

written, err := io.Copy(multiWriter, getResp.Body)
if err != nil {
    return nil, fmt.Errorf("failed to download file: %w", err)
}

checksum := hex.EncodeToString(hash.Sum(nil))
```

---

## Security Validations by Component

### S3 Client (`s3/client.go`)

**Validations**:
- S3 key validation (no path traversal, length limits)
- Size limit enforcement (10GB max)
- Checksum computation during download

**Error Handling**:
- Access denied → Abort (unrecoverable)
- Size limit exceeded → Abort (security violation)

### Download FSM (`download/fsm.go`)

**Validations**:
- Tar structure validation
- Path traversal detection
- Symlink validation
- File size limits
- File count limits

**Error Handling**:
- Corrupted tar → Abort + cleanup
- Malicious content → Abort + cleanup + log security violation

### Extraction (`extraction/extract.go`)

**Validations**:
- Path sanitization for every entry
- Symlink target validation
- Permission checks
- Layout verification

**Error Handling**:
- Path traversal → Skip entry + log warning
- Symlink escape → Abort + cleanup
- Invalid layout → Abort

### DeviceMapper (`devicemapper/dm.go`)

**Validations**:
- Device name validation (prevent command injection)
- Device ID validation (numeric only)
- Pool name validation
- Size limits (100GB max per device)

**Error Handling**:
- Invalid input → Return error (validation failure)
- Pool full → Return PoolFullError (abort)

---

## Security Logging

### What to Log

**Security Violations**:
- Path traversal attempts
- Symlink attacks
- Oversized files
- Excessive file counts
- Setuid/setgid binaries
- World-writable directories in critical paths

**Log Format**:
```go
logger.WithFields(logrus.Fields{
    "violation": "path_traversal",
    "image_id":  imageID,
    "s3_key":    s3Key,
    "path":      suspiciousPath,
}).Error("security violation detected")
```

### Log Monitoring

**Alerts**:
- Multiple security violations from same S3 key
- Repeated path traversal attempts
- Unusual file sizes or counts

---

## Security Testing

### Test Cases

1. **Path Traversal**:
   - Tar with `../etc/passwd` entry
   - Tar with `../../root/.ssh/authorized_keys` entry

2. **Symlink Attacks**:
   - Symlink to `/etc/passwd`
   - Symlink with `../../../` in target
   - Symlink escaping rootfs

3. **Resource Exhaustion**:
   - Tar with 200,000 files (exceeds limit)
   - Tar with 15GB total size (exceeds limit)
   - Single file > 1GB (exceeds limit)

4. **Malicious Permissions**:
   - Setuid binary in rootfs
   - World-writable /etc directory
   - Device files outside /dev

5. **Corrupted Archives**:
   - Invalid tar format
   - Truncated tar file
   - Checksum mismatch

### Security Validation Script

```bash
#!/bin/bash
# Test security validations

# Test 1: Path traversal
tar -czf malicious-path-traversal.tar ../../../etc/passwd
./flyio-image-manager process-image --tarball malicious-path-traversal.tar
# Expected: Abort with "path traversal detected"

# Test 2: Symlink attack
mkdir -p test/rootfs
ln -s /etc/passwd test/rootfs/passwd
tar -czf malicious-symlink.tar test/
./flyio-image-manager process-image --tarball malicious-symlink.tar
# Expected: Abort with "absolute symlink target"

# Test 3: Oversized file
dd if=/dev/zero of=large-file bs=1M count=2048  # 2GB file
tar -czf malicious-large-file.tar large-file
./flyio-image-manager process-image --tarball malicious-large-file.tar
# Expected: Abort with "file too large"
```

---

## Security Best Practices

### For Developers

1. **Validate All Inputs**: Never trust external data
2. **Fail Closed**: Reject suspicious content rather than allow
3. **Log Security Events**: Log all validation failures
4. **Use Allowlists**: Prefer allowlists over denylists
5. **Limit Resources**: Enforce strict resource limits
6. **Clean Up on Failure**: Remove partial work to prevent exploitation

### For Operators

1. **Monitor Logs**: Watch for security violations
2. **Review Alerts**: Investigate repeated violations
3. **Update Limits**: Adjust resource limits based on usage
4. **Audit Images**: Periodically review processed images
5. **Rotate Credentials**: Regularly rotate AWS credentials

---

## References

- [FSM Flow Design](FSM_FLOWS.md) - Error handling strategies
- [Requirements](../spec/REQUIREMENTS.md) - Security requirements
- [System Architecture](SYSTEM_ARCH.md) - Component interactions
- [Troubleshooting](../guide/TROUBLESHOOTING.md) - Security error resolution


# Requirements Specification

**Document Type**: Specification  
**Status**: Approved  
**Version**: 1.0  
**Last Updated**: 2025-11-21

---

## Overview

This document defines the formal requirements for the Container Image Manager, a production-grade implementation that retrieves container images from S3, unpacks them into devicemapper thinpool devices, and tracks them using SQLite.

**Purpose**: Provide a reference implementation of FSM-based orchestration patterns for building resilient, production-ready systems.

---

## Functional Requirements

### FR-1: Image Download from S3

**Requirement**: The system SHALL download container image tarballs from a configured S3 bucket.

**Acceptance Criteria**:
- Download images using AWS SDK for Go v2
- Stream downloads to avoid memory exhaustion
- Compute SHA256 checksum during download
- Store downloaded images in local filesystem
- Support resuming interrupted downloads (future enhancement)

### FR-2: Idempotency

**Requirement**: The system SHALL check if an image has already been processed before performing work.

**Acceptance Criteria**:
- Query SQLite database before downloading
- Verify file integrity (checksum, size) for existing images
- Skip already-completed operations using FSM Handoff mechanism
- Handle concurrent requests for the same image safely

### FR-3: Image Unpacking

**Requirement**: The system SHALL extract container image tarballs into devicemapper thin devices.

**Acceptance Criteria**:
- Create thin devices in devicemapper pool
- Extract tarball contents to mounted device
- Validate canonical filesystem layout (rootfs/, etc/, usr/, var/)
- Verify extracted content integrity
- Clean up on failure (unmount, deactivate, delete device)

### FR-4: Snapshot Activation

**Requirement**: The system SHALL create devicemapper snapshots to activate images.

**Acceptance Criteria**:
- Create copy-on-write snapshots from unpacked devices
- Activate snapshot devices
- Track active snapshots in database
- Support multiple snapshots per image (future enhancement)

### FR-5: State Tracking

**Requirement**: The system SHALL track all operations in SQLite for persistence and recovery.

**Acceptance Criteria**:
- Record downloaded images with metadata (S3 key, path, checksum, size)
- Record unpacked images with device information
- Record active snapshots with device paths
- Support querying image status
- Enable crash recovery through FSM event replay

---

## Non-Functional Requirements

### NFR-1: Security (Hostile Environment)

**Requirement**: The system SHALL assume a hostile environment and validate all inputs.

**Acceptance Criteria**:
- Validate S3 keys (no path traversal, length limits)
- Validate tar entries (no absolute paths, no `..` components)
- Validate symlink targets (stay within rootfs)
- Reject dangerous file permissions (setuid/setgid)
- Enforce resource limits (file sizes, file counts, timeouts)
- Log all security violations

**Threat Model**:
- Malicious tar archives with path traversal
- Symlink attacks escaping rootfs
- Resource exhaustion (large files, many files)
- Corrupted or tampered images

### NFR-2: Performance

**Requirement**: The system SHALL process images efficiently with controlled resource usage.

**Acceptance Criteria**:
- Stream downloads (no full file in memory)
- Concurrent downloads (max 5 simultaneous)
- Concurrent unpacking (max 2 simultaneous, I/O bound)
- Download timeout: 5 minutes
- Extraction timeout: 30 minutes
- Database operations complete in <100ms (typical)

### NFR-3: Reliability

**Requirement**: The system SHALL handle failures gracefully and support crash recovery.

**Acceptance Criteria**:
- Automatic retry for transient errors (network, database locks)
- Abort for unrecoverable errors (validation failures, resource exhaustion)
- Clean up partial work on failure
- Resume FSMs after crash using event replay
- Maintain database consistency

### NFR-4: Observability

**Requirement**: The system SHALL provide visibility into operations for monitoring and debugging.

**Acceptance Criteria**:
- Structured logging with contextual fields (image_id, s3_key, device_id)
- Log all state transitions
- Log security violations
- Expose Prometheus metrics (FSM library built-in)
- Support OpenTelemetry tracing

---

## System Constraints

### SC-1: Platform Requirements

- **Operating System**: Linux with devicemapper support
- **Go Version**: Go 1.21 or later
- **Privileges**: Root access required for devicemapper operations
- **Dependencies**: dmsetup, losetup, mkfs.ext4

### SC-2: Resource Limits

| Resource | Limit | Rationale |
|----------|-------|-----------|
| Max image size | 10GB | Typical container image size |
| Max file size | 1GB | Prevent individual file attacks |
| Max file count | 100,000 | Prevent directory listing attacks |
| Max device size | 100GB | Devicemapper pool capacity |
| Download timeout | 5 minutes | Network reliability threshold |
| Extraction timeout | 30 minutes | I/O performance threshold |
| Concurrent downloads | 5 | Network bandwidth management |
| Concurrent unpacking | 2 | I/O contention management |

### SC-3: Database Configuration

- **Engine**: SQLite 3.x
- **Journal Mode**: WAL (Write-Ahead Logging)
- **Synchronous**: NORMAL (balance durability/performance)
- **Cache Size**: 10MB
- **Busy Timeout**: 5 seconds
- **Connection Pool**: 10 max open, 5 max idle

### SC-4: S3 Configuration

- **Bucket**: Configurable (set via `--bucket` flag or `S3_BUCKET` environment variable)
- **Region**: us-east-1 (default)
- **Prefix**: `images/`
- **Authentication**: AWS credentials (environment or IAM role)

---

## Success Criteria

### Acceptance Tests

1. **Download Test**:
   - Download an image from S3
   - Verify checksum matches
   - Verify file stored in database
   - Re-run download, verify idempotency (skips download)

2. **Unpack Test**:
   - Unpack downloaded image
   - Verify devicemapper device created
   - Verify filesystem layout is canonical
   - Verify database records device info

3. **Activate Test**:
   - Create snapshot from unpacked image
   - Verify snapshot device active
   - Verify database records snapshot

4. **End-to-End Test**:
   - Process image through complete pipeline (download → unpack → activate)
   - Verify all database entries correct
   - Verify devicemapper devices exist
   - Re-run pipeline, verify idempotency at each stage

5. **Error Handling Test**:
   - Test with invalid S3 key (should abort)
   - Test with corrupted tarball (should abort and cleanup)
   - Test with malicious tar (path traversal, should abort)
   - Test network interruption (should retry)
   - Test crash during unpacking (should resume on restart)

6. **Concurrency Test**:
   - Start 10 downloads simultaneously
   - Verify queue limits respected (max 5 concurrent)
   - Verify no race conditions in database
   - Verify idempotency with concurrent requests

7. **Security Test**:
   - Test with tar containing `../` paths (should reject)
   - Test with absolute symlinks (should reject)
   - Test with setuid binaries (should reject or strip)
   - Test with oversized files (should reject)

---

## Out of Scope

The following are explicitly out of scope for this implementation:

- Image registry integration (Docker Hub, etc.)
- Image compression/decompression (gzip, bzip2, xz)
- Multi-region S3 support with failover
- Image deduplication across layers
- Garbage collection of old images
- Web UI or REST API
- User authentication and authorization
- Multi-tenancy and quotas
- Image signing and verification
- Container runtime integration

---

## References

- [System Architecture](../design/SYSTEM_ARCH.md)
- [FSM Flow Design](../design/FSM_FLOWS.md)
- [Security Design](../design/SECURITY.md)
- [Database Schema](../api/DATABASE.md)


# Container Image Manager

A production-grade container image management system built on the [FSMv2](https://github.com/superfly/fsm) finite state machine library. This project demonstrates FSM-based orchestration patterns for building resilient, crash-recoverable distributed systems.

The system retrieves container images from S3, unpacks them into devicemapper thinpool devices, and tracks them using SQLite — all orchestrated through explicit state machines with built-in crash recovery, idempotency, and observability.

## Key Features

- **FSM-based orchestration** — Every workflow (download, unpack, activate) is modeled as an explicit state machine with persisted state, enabling automatic crash recovery and retry logic
- **Idempotent operations** — Safe to re-run at any point; the system detects already-completed work and skips it
- **DeviceMapper thin provisioning** — Container images are unpacked into copy-on-write thin devices with snapshot support
- **Defense-in-depth security** — Path traversal protection, symlink validation, resource limits, and hostile blob handling
- **Interactive TUI dashboard** — Real-time monitoring, S3 image browser, and system health checks
- **SQLite state tracking** — Local-first persistence with WAL mode for concurrent access

## How It Works

Using the FSM library:
- Retrieve an arbitrary container image from S3
- Only if it hasn't been retrieved already (idempotency)
- Unpack the image into a canonical filesystem layout inside a devicemapper thinpool device
- Only if that hasn't already been done
- "Activate" the image by creating a snapshot of the thinpool device
- Track all available images in SQLite

> **Security Note**: This system is designed to operate in hostile environments. All blobs are treated as untrusted input with multiple validation layers.

### DeviceMapper Pool Setup

Create the thin-provisioned storage pool:
```bash
# Create pool files (optimized sizes based on S3 bucket analysis)
# - 4MB metadata (0.2% of data size, recommended for thin pools)
# - 2GB data (sufficient for 3-4 large images ~500MB each)
fallocate -l 4M pool_meta
fallocate -l 2G pool_data

METADATA_DEV="$(losetup -f --show pool_meta)"
DATA_DEV="$(losetup -f --show pool_data)"

# Create thin pool with optimized block size for performance
# - Block size: 256 sectors (128KB) - optimal for container images
# - Low water mark: 65536 sectors (32MB)
# Note: Original config used 2048 sectors (1MB blocks) which caused severe I/O slowdown
dmsetup create --verifyudev pool --table "0 4194304 thin-pool ${METADATA_DEV} ${DATA_DEV} 256 65536"
```

---

## Implementation Progress

### ✅ Completed Phases

**Phase 1: Architecture & Design** (Complete)
- FSM state machine design
- Database schema design
- Request/Response type definitions
- Complete architecture documentation

**Phase 2: Foundation & Infrastructure** (Complete)
- SQLite database layer with migrations and CRUD operations (`database/`)
- S3 client wrapper with streaming downloads and validation (`s3/`)
- DeviceMapper utilities for thin devices and snapshots (`devicemapper/`)
- Secure tarball extraction with security checks (`extraction/`)

**Phase 3: Download FSM Implementation** (Complete)
- Download FSM with all transitions (`download/fsm.go`)
- Idempotency checks, S3 streaming, validation, metadata storage
- Security validations and error handling

**Phase 4: Unpack FSM Implementation** (Complete)
- Unpack FSM with all transitions (`unpack/fsm.go`)
- Idempotent check-unpacked, device creation, secure extraction, layout verification
- Deterministic device ID mapping and cleanup on failure

**Phase 5: Activate FSM Implementation** (Complete)
- Activate FSM with all transitions (`activate/fsm.go`)
- Snapshot creation, idempotency checks, database registration
- Copy-on-write devicemapper snapshots

**Phase 6: Orchestration & Main Application** (Complete)
- Complete CLI application (`cmd/flyio-image-manager/main.go`)
- FSM manager initialization and registration
- Manual FSM chaining (Download → Unpack → Activate)
- CLI commands: process-image, list-images, list-snapshots, daemon
- Configuration management and graceful shutdown
- Crash recovery support

**Phase 7: Error Handling & Resilience** (Complete)
- Comprehensive error handling across all FSMs
- **Kernel panic mitigation** - "fail-dumb" pattern to prevent system instability
- Automatic cleanup **removed** from error paths (prevents kernel panics)
- Garbage collection command for safe cleanup of orphaned devices
- Retry strategies via FSM library
- Security validations (path traversal, symlinks, resource limits)

**Phase 9: Documentation & Polish** (Complete)
- 19 comprehensive documents (~10,000 lines)
- 100% godoc coverage with examples
- Observability guide (logging, tracing, metrics)
- Usage documentation and integration testing guide
- Code quality review (Grade A)

**Phase 8: Integration & Validation** (Complete - 2025-11-25)
- ✅ DeviceMapper pool tested on Ubuntu 24.04
- ✅ End-to-end pipeline validated with real S3 images
- ✅ Idempotency verified (exit code 0 on re-run, <0.1s execution)
- ✅ Error handling verified (S3 404, graceful abort, no orphaned devices)
- ✅ Lock mechanism prevents concurrent operations (kernel panic protection)

### ✅ Verification Results

| Test | Status |
|------|--------|
| Download FSM (S3 → local) | ✅ PASS |
| Unpack FSM (tarball → thin device) | ✅ PASS |
| Activate FSM (thin → snapshot) | ✅ PASS |
| Idempotency (re-run same image) | ✅ PASS (exit 0, 0.048s) |
| Error handling (invalid S3 key) | ✅ PASS (exit 1, clear error) |
| Database persistence | ✅ PASS |
| DeviceMapper device naming | ✅ PASS (`thin-<id>`, `snap-<image>`) |

### Project Structure

```
flyio/
├── database/          # ✅ SQLite database layer
├── s3/                # ✅ S3 client wrapper
├── devicemapper/      # ✅ DeviceMapper utilities
├── extraction/        # ✅ Tarball extraction
├── download/          # ✅ Download FSM
├── unpack/            # ✅ Unpack FSM
├── activate/          # ✅ Activation FSM
├── cmd/               # ✅ CLI application (4 commands + daemon)
└── docs/              # ✅ Comprehensive documentation (19 documents)
    ├── INDEX.md       # Master navigation hub
    ├── spec/          # Requirements specification
    ├── api/           # API reference (Database, FSM Library, Interfaces)
    ├── design/        # Architecture, FSM flows, Security, ADRs, Durable State
    ├── guide/         # Usage, Development, Observability, Integration Testing
    └── note/          # Optional engineering notes (Fly.io context, kernel panic analysis, diagnostics)
```

## Documentation

**📚 Start here**: [`docs/INDEX.md`](docs/INDEX.md) - Master documentation index with navigation for all personas (developers, operators, architects)

### Quick Links for New Developers

**Getting Started**:
1. [Requirements Specification](docs/spec/REQUIREMENTS.md) - Understand what this system does
2. [Quick Start Guide](docs/guide/QUICKSTART.md) - Zero-to-running setup (devicemapper pool, AWS credentials, first run)
3. [System Architecture](docs/design/SYSTEM_ARCH.md) - High-level overview of components and data flow

**Understanding the Design**:
4. [FSM Flow Design](docs/design/FSM_FLOWS.md) - Complete state machine flows with all transitions
5. [Security Design](docs/design/SECURITY.md) - Defense-in-depth strategy and validation layers
6. [Architecture Decisions](docs/design/DECISIONS.md) - ADRs explaining key design choices

**Development Workflow**:
7. [Development Guide](docs/guide/DEVELOPMENT.md) - Build commands, testing, debugging, contributing
8. [Database API](docs/api/DATABASE.md) - Schema, queries, transaction patterns
9. [FSM Library API](docs/api/FSM_LIBRARY.md) - Registration patterns, error handling


### Documentation Categories

- **`docs/spec/`** - Formal requirements and constraints
- **`docs/api/`** - API reference (Database, FSM Library, CLI)
- **`docs/design/`** - Architecture, FSM flows, Security, ADRs
- **`docs/guide/`** - Quick start, Development, Troubleshooting
- **`docs/note/`** - Optional engineering notes (Engineering context, kernel panic analysis, diagnostics)

---

## License

This project incorporates the [FSMv2 library](https://github.com/superfly/fsm) by Fly.io.

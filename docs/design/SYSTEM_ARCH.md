# System Architecture

**Document Type**: Design
**Status**: Approved
**Version**: 1.3
**Last Updated**: 2025-11-26

---

## Overview

This document describes the high-level architecture of the Container Image Manager, a production-grade system that retrieves container images from S3, unpacks them into devicemapper thinpool devices, and tracks them using SQLite.

This system demonstrates FSM-based orchestration patterns for resilient, production-ready distributed systems.

The system provides:
- **FSM-based orchestration** for reliable, resumable image processing
- **Interactive TUI dashboard** for real-time monitoring and S3 browsing
- **Automated setup/cleanup** scripts for environment management
- **Multi-layer concurrency control** to prevent kernel panics

---

## High-Level Architecture

### System Flow

```
┌─────────────┐
│   S3 Bucket │
│   (images)  │
└──────┬──────┘
       │
       ▼
┌─────────────────┐
│  Download FSM   │ ──────► SQLite (images table)
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│   Unpack FSM    │ ──────► DeviceMapper + SQLite (unpacked_images)
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  Activate FSM   │ ──────► DeviceMapper Snapshot + SQLite (snapshots)
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ Active Container│
│     Image       │
└─────────────────┘
```

### Processing Pipeline

1. **Image Retrieval**: Download container image tarballs from S3
2. **Idempotency Check**: Verify if image already exists before downloading
3. **Image Unpacking**: Extract tarball into canonical filesystem layout
4. **DeviceMapper Integration**: Create thin device for the unpacked image
5. **Snapshot Creation**: Activate image by creating a devicemapper snapshot
6. **State Tracking**: Record all operations in SQLite for persistence and recovery

---

## Core Components

### 1. FSM Library (Foundation)

The FSM library provides the state machine framework with these key components:

**Manager** (`manager.go`):
- Central coordinator for FSM lifecycle
- Manages FSM runs and persistence
- Handles crash recovery through event replay
- Exposes admin interface via Unix socket

**FSM** (`fsm.go`, `builder.go`):
- State machine definitions with transitions
- Fluent builder API for registration
- Strongly-typed Request/Response messages
- Automatic retry and error handling

**Store** (`store.go`):
- Dual-database persistence (BoltDB + memdb)
- Event log for each transition
- Parent-child FSM relationships
- Archival to history database

**Runners** (`runner.go`):
- Immediate execution (default)
- Delayed execution (WithDelayedStart)
- Queued execution (WithQueue)
- Sequential execution (WithRunAfter)

**Interceptors** (`interceptor.go`):
- Retry logic with exponential backoff
- Error handling and classification
- Metrics collection (Prometheus)
- Distributed tracing (OpenTelemetry)

### 2. Container Image Management (Implementation)

**Core Packages:**

```
flyio/
├── database/              # SQLite database layer
│   ├── database.go        # Connection management, WAL mode
│   ├── migrations.go      # Schema migrations
│   ├── schema.go          # DDL for tables and indexes
│   ├── models.go          # Data models
│   ├── images.go          # Image CRUD operations
│   ├── unpacked.go        # Unpacked image operations
│   └── snapshots.go       # Snapshot operations
│
├── s3/                    # S3 client wrapper
│   └── client.go          # Streaming downloads, validation
│
├── devicemapper/          # DeviceMapper utilities
│   ├── dm.go              # Thin devices, snapshots, lifecycle
│   └── pool.go            # Pool Manager (auto-create, health, recovery)
│
├── safeguards/            # System stability safeguards
│   └── safeguards.go      # OperationGuard, health checks, D-state detection
│
├── extraction/            # Tarball extraction
│   └── extract.go         # Secure extraction, validation
│
├── download/              # Download FSM
│   └── fsm.go             # check-exists → download → validate → store-metadata
│
├── unpack/                # Unpack FSM
│   └── fsm.go             # check-unpacked → create-device → extract → verify → update-db
│
├── activate/              # Activation FSM
│   └── fsm.go             # check-snapshot → create-snapshot → register
│
├── tui/                   # Interactive TUI (Bubble Tea)
│   ├── dashboard.go       # Main dashboard model and views
│   ├── s3browser.go       # S3 image browser with selection
│   ├── fetcher.go         # Background data fetching (pool, db, logs)
│   ├── progress.go        # Progress bar for image processing
│   ├── cli.go             # CLI-mode output (non-TUI)
│   ├── callback.go        # FSM progress callbacks
│   ├── admin_client.go    # FSM admin socket client
│   ├── styles.go          # Lipgloss styling
│   ├── table.go           # Table rendering utilities
│   └── types.go           # Shared types
│
├── scripts/               # Automation scripts
│   ├── setup.sh           # Environment setup (deps, pool, build)
│   └── cleanup.sh         # Safe teardown (correct order)
│
└── cmd/flyio-image-manager/
    ├── main.go            # CLI entry point, commands
    └── gc.go              # Garbage collection command
```

**Key FSMs:**

1. **Download FSM**: Handles S3 retrieval with idempotency
   - Queue: `downloads` (max 5 concurrent)
   - Timeout: 5 minutes for download transition
   - Idempotency: Check database before downloading

2. **Unpack FSM**: Extracts and validates container image layers
   - Queue: `unpacking` (max 1 concurrent, serialized)
   - Timeout: 30 minutes for extraction
   - Idempotency: Check database and devicemapper

3. **Activation FSM**: Creates devicemapper snapshots
   - No queue (fast operation)
   - Idempotency: Check database and devicemapper

### 3. CLI Commands

The `flyio-image-manager` binary provides these commands:

| Command | Description |
|---------|-------------|
| `process-image` | Process a container image (download → unpack → activate) |
| `list-images` | List downloaded images from database |
| `list-snapshots` | List active snapshots |
| `monitor` | Interactive TUI dashboard for live FSM tracking |
| `gc` | Garbage collect orphaned devices |
| `setup-pool` | Setup or recreate the devicemapper thin-pool |
| `daemon` | Run as a daemon (future: API server) |

### 4. TUI Dashboard (`tui/` package)

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea), the TUI provides:

**Process Image TUI** (default for `process-image`):
- Animated progress bars for Download → Unpack → Activate phases
- Real-time speed and percentage display
- Color-coded status indicators
- Quiet mode (`--quiet`) for scripting

**Monitor Dashboard** (`monitor` command):
- **Monitor View** (Press `1`): FSM state tracking, pool utilization, snapshots, logs
- **S3 Browser** (Press `2`): Browse S3 bucket, select images, process with Enter
- **Inline Mode** (`--inline`): Disables alt-screen for SSH compatibility
- Real-time updates via background fetcher

**Key Components**:
- `dashboard.go`: Main Bubble Tea model with view switching
- `s3browser.go`: S3 image listing with cursor navigation
- `fetcher.go`: Background data fetching (pool status, database, logs)
- `progress.go`: Progress bar model for image processing
- `styles.go`: Lipgloss styling for consistent look

### 5. Environment Automation (`scripts/`)

**`scripts/setup.sh`**: Complete environment setup
- Installs dependencies (Go, lvm2, thin-provisioning-tools, sqlite3)
- Creates DeviceMapper pool with optimal 128KB block size
- Builds the Go application
- Validates AWS credentials

**`scripts/cleanup.sh`**: Safe environment teardown
- Unmounts devices, removes snapshots, then thin devices
- Removes pool only after devices are gone
- Detaches loop devices, then removes backing files
- Prevents kernel panics from out-of-order cleanup

### 6. Concurrency Control

**Four-Layer Defense-in-Depth Approach**:

The system implements multi-layered concurrency control to prevent kernel panics and devicemapper pool hangs caused by concurrent operations.

**Layer 1: Process-Level Locking**
- **Manager Lock File**: `<FSMDBPath>/flyio-manager.lock`
- **Purpose**: Prevents multiple flyio-image-manager processes from running simultaneously
- **Mechanism**: Atomic file creation with PID, timestamp, and command metadata
- **Enforcement**:
  - `process-image` and `daemon` commands acquire lock at startup
  - `gc` command checks for lock file and requires `--ignore-lock` flag to override
- **Rationale**: DeviceMapper operations are not safe across multiple processes; single-process execution eliminates inter-process race conditions

**Layer 2: Image-Level Locking**
- **Database Table**: `image_locks` (image_id PRIMARY KEY, locked_at, locked_by)
- **Purpose**: Prevents concurrent unpack of the same image across FSMs
- **Mechanism**: UNIQUE constraint on `image_id` provides atomic lock acquisition
- **Lifecycle**:
  1. Acquired at start of Unpack FSM (`checkUnpacked` transition)
  2. Released on Handoff (image already unpacked)
  3. Released before `fsm.Abort` (validation failure, pool exhaustion)
  4. Released after successful unpack (`updateDB` transition)
- **Rationale**: Multiple FSMs attempting to unpack the same image concurrently cause devicemapper pool hangs; per-image locks serialize unpack operations

**Layer 3: DeviceMapper Mutex Serialization**
- **Implementation**: `sync.Mutex` in `devicemapper.Client` struct
- **Purpose**: Serializes all devicemapper operations within a single process
- **Protected Operations**: `CreateThinDevice`, `CreateSnapshot`, `ActivateDevice`, `DeactivateDevice`, `DeleteDevice`
- **Rationale**: Concurrent dm operations within a process cause pool instability; mutex ensures sequential execution

**Layer 4: System Safeguards** (added 2025-11-26)
- **Pool Manager** (`devicemapper/pool.go`): Auto-creates pool if missing, validates health
- **Health Checker** (`safeguards/safeguards.go`): D-state detection, kernel log scanning, memory checks
- **Operation Guard** (`safeguards/safeguards.go`): Semaphore-based serialization with pre-op health checks
- **Auto-Recovery**: Pool automatically recreated after reboot/panic via `poolManager.EnsurePoolExists()`

**Concurrency Configuration**:
- `DownloadQueueSize`: 5 (network-bound, safe to parallelize)
- `UnpackQueueSize`: 1 (devicemapper-heavy, must serialize)
- `ActivateQueueSize`: Unlimited (fast snapshot creation)
- `OperationGuard.MaxConcurrent`: 1 (all dm-heavy ops serialized)

**Trade-offs**:
- **Positive**: Eliminates kernel panics, prevents pool hangs, maintains system stability, auto-recovery
- **Negative**: Reduced unpack concurrency (1 at a time), increased lock management complexity
- **Backward Compatibility**: Database migration automatically adds `image_locks` table

**See Also**:
- [Kernel Panic Root Cause Analysis](../note/KERNEL_PANIC_ROOT_CAUSE.md)
- [ADR-001 - Kernel Panic Mitigation](ADR-001-KERNEL-PANIC-MITIGATION.md)
- [Database API - Image Lock Operations](../api/DATABASE.md#image-lock-operations)
- [Operations Guide - System Safeguards](../guide/OPERATIONS.md#system-safeguards)

---

## Data Flow

### Download FSM Flow

```
User Request
    │
    ▼
┌─────────────────┐
│  check-exists   │ ──► Query SQLite by s3_key
└────────┬────────┘
         │ (not found or invalid)
         ▼
┌─────────────────┐
│    download     │ ──► Stream from S3, compute checksum
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│    validate     │ ──► Verify checksum, tar structure, security
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ store-metadata  │ ──► Insert into SQLite images table
└────────┬────────┘
         │
         ▼
    Complete
```

### Unpack FSM Flow

```
Download Complete
    │
    ▼
┌─────────────────┐
│ check-unpacked  │ ──► Query SQLite + verify devicemapper
└────────┬────────┘
         │ (not found or invalid)
         ▼
┌─────────────────┐
│ create-device   │ ──► dmsetup create thin device + mkfs.ext4
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ extract-layers  │ ──► Mount device, extract tar, unmount
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ verify-layout   │ ──► Check rootfs/, permissions, symlinks
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│   update-db     │ ──► Insert into unpacked_images table
└────────┬────────┘
         │
         ▼
    Complete
```

### Activate FSM Flow

```
Unpack Complete
    │
    ▼
┌─────────────────┐
│ check-snapshot  │ ──► Query SQLite + verify devicemapper
└────────┬────────┘
         │ (not found or invalid)
         ▼
┌─────────────────┐
│ create-snapshot │ ──► dmsetup create snapshot + activate
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│    register     │ ──► Insert into snapshots table, update images
└────────┬────────┘
         │
         ▼
    Complete
```

---

## Component Interactions

### Database Layer

**Purpose**: Persistent state tracking and idempotency checks

**Tables**:
- `images`: Downloaded images (s3_key, local_path, checksum, size)
- `unpacked_images`: Extracted images (device_id, device_name, device_path)
- `snapshots`: Active snapshots (snapshot_id, snapshot_name, device_path)

**Operations**:
- Idempotency checks (CheckImageDownloaded, CheckImageUnpacked, CheckSnapshotExists)
- CRUD operations (Store, Get, List, Delete)
- Transaction support for atomic updates

**Configuration**:
- WAL mode for concurrency
- Foreign keys for referential integrity
- Indexes for fast lookups

### S3 Client

**Purpose**: Download container images from S3

**Features**:
- Streaming downloads (avoid memory exhaustion)
- SHA256 checksum computation during download
- Size limit enforcement (10GB max)
- Temporary file handling with atomic moves
- Error classification (access denied, size limit, network)

**Security**:
- S3 key validation (no path traversal, length limits)
- Size checks before download
- Cleanup on failure

### DeviceMapper Integration

**Purpose**: Manage thin devices and snapshots

**Operations**:
- CreateThinDevice: Create thin device + ext4 filesystem
- CreateSnapshot: Create copy-on-write snapshot
- ActivateDevice/DeactivateDevice: Lifecycle management
- DeleteDevice: Cleanup
- MountDevice/UnmountDevice: Filesystem operations

**Error Handling**:
- DeviceExistsError → Retry with new ID
- PoolFullError → Abort (unrecoverable)
- DeviceNotFoundError → Abort (data missing)

**Security**:
- Input validation (device IDs, names, pool names)
- Size limits (100GB max per device)
- Pool capacity checks

### Tarball Extraction

**Purpose**: Securely extract container images

**Features**:
- Path traversal protection
- Symlink validation
- Size limits (1GB per file, 10GB total, 100k files)
- Timeout support (30 minutes)
- Layout verification (rootfs/, etc/, usr/, var/)
- Permission checks (setuid/setgid, world-writable)

**Security**:
- Reject absolute paths
- Reject `..` in paths
- Validate symlink targets stay within rootfs
- Enforce resource limits
- Log security violations

---

## Concurrency & Queuing

### Queue Configuration

| FSM | Queue Name | Max Concurrent | Rationale |
|-----|------------|----------------|-----------|
| Download | `downloads` | 5 | Network bandwidth management |
| Unpack | `unpacking` | 2 | I/O contention management |
| Activate | (none) | Unlimited | Fast operation, no bottleneck |

### FSM Chaining

FSMs are chained using `WithRunAfter`:

```go
// Download FSM
downloadRunID, _ := startDownload(ctx, imageID, req, WithQueue("downloads"))

// Unpack FSM (waits for download)
unpackRunID, _ := startUnpack(ctx, imageID, req, 
    WithQueue("unpacking"), 
    WithRunAfter(downloadRunID))

// Activate FSM (waits for unpack)
activateRunID, _ := startActivate(ctx, imageID, req, 
    WithRunAfter(unpackRunID))
```

---

## Error Handling Strategy

### Error Types

| Error Type | Behavior | Use Cases |
|------------|----------|-----------|
| Standard Error | Auto-retry with exponential backoff | Network errors, database locks, transient I/O |
| fsm.Abort | Stop immediately, no retries | Validation failures, security violations, resource exhaustion |
| fsm.Unrecoverable | Stop immediately, mark as failed | Permanent errors, data corruption |
| fsm.Handoff | Skip remaining transitions | Idempotency (already processed) |

### Retry Strategies

See [FSM Flow Design](FSM_FLOWS.md) for detailed retry strategies per transition.

---

## Observability

### Logging

- **Structured logging** with logrus (JSON or text format)
- **Contextual fields**: image_id, s3_key, device_id, transition, phase
- **Progress tracking**: Download speed, bytes transferred, percentages
- **Security violation logging**: Path traversal attempts, symlink attacks
- **Log suppression**: TUI mode suppresses logs to `io.Discard` to prevent mixing

### TUI Dashboard Metrics

The `monitor` command provides real-time visibility:

- **Pool Status Panel**: Data/metadata usage, percentage full, error states
- **FSM Activity Panel**: Active runs, current states, progress
- **Snapshots Panel**: Active snapshot count, names
- **Logs Panel**: Recent activity with level indicators

### FSM Library Observability

The FSM library provides built-in observability:

- **Prometheus metrics**: Action/transition counts and durations
- **OpenTelemetry tracing**: Spans for actions and transitions
- **Event logging**: All state transitions persisted to BoltDB

### Health Monitoring

- **Pool health checks**: Via `dmsetup status pool`
- **D-state detection**: Scan `/proc` for blocked processes
- **Database connectivity**: WAL mode, connection pooling
- **Safeguards logging**: Pre-operation health check results

---

## References

- [FSM Flow Design](FSM_FLOWS.md) - Detailed state machine flows
- [Security Design](SECURITY.md) - Security strategy and validations
- [Database Schema](../api/DATABASE.md) - Data model and queries
- [Requirements](../spec/REQUIREMENTS.md) - Functional and non-functional requirements
- [Quick Start Guide](../guide/QUICKSTART.md) - Setup and usage instructions


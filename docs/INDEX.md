# Container Image Manager — Documentation Index

**Version**: 1.2
**Status**: Production Ready (All Phases Complete)
**Last Updated**: 2025-11-26

---

## Quick Navigation

### 🎯 For New Developers
Start here to get up and running quickly:
1. [Requirements Specification](spec/REQUIREMENTS.md) - What this system does
2. [Quick Start Guide](guide/QUICKSTART.md) - Setup and first run
3. [System Architecture](design/SYSTEM_ARCH.md) - High-level overview

### 🔧 For Contributors
Working on the codebase? Start here:
1. [Development Guide](guide/DEVELOPMENT.md) - Build, test, contribute
2. [FSM Flow Design](design/FSM_FLOWS.md) - State machine logic
3. [API Interfaces](api/INTERFACES.md) - Request/Response types
4. [Troubleshooting](guide/TROUBLESHOOTING.md) - Common issues

### 🏗️ For Architects
Understanding design decisions:
1. [Architecture Decisions](design/DECISIONS.md) - ADRs and rationale
2. [Security Design](design/SECURITY.md) - Security strategy
3. [FSM Library Reference](api/FSM_LIBRARY.md) - FSM framework details
4. [Database Schema](api/DATABASE.md) - Data model and queries

### 📝 For Operators
Running and maintaining the system:
1. [Quick Start Guide](guide/QUICKSTART.md) - Deployment instructions
2. [Operations Guide](guide/OPERATIONS.md) - Maintenance and emergency procedures
3. [Troubleshooting](guide/TROUBLESHOOTING.md) - Issue resolution
4. [Database Reference](api/DATABASE.md) - Schema and queries

---

## Documentation Categories

### 📋 Specification (`spec/`)
Formal requirements and constraints.

- **[REQUIREMENTS.md](spec/REQUIREMENTS.md)** - Functional and non-functional requirements, system constraints, resource limits, acceptance criteria

### 🔌 API Reference (`api/`)
Technical reference for interfaces, schemas, and libraries.

- **[FSM_LIBRARY.md](api/FSM_LIBRARY.md)** - FSM framework API, registration patterns, error handling
- **[DATABASE.md](api/DATABASE.md)** - SQLite schema, indexes, queries, transactions, migrations
- **[INTERFACES.md](api/INTERFACES.md)** - Request/Response types, CLI flags, configuration structs

### 🎨 Design (`design/`)
Architecture, patterns, and design decisions.

- **[SYSTEM_ARCH.md](design/SYSTEM_ARCH.md)** - High-level architecture, component interactions, data flow
- **[FSM_FLOWS.md](design/FSM_FLOWS.md)** - State machine flows, transitions, error handling, retry strategies
- **[DURABLE_STATE_CONTRACTS.md](design/DURABLE_STATE_CONTRACTS.md)** - State contracts, crash recovery, database-FSM alignment
- **[SECURITY.md](design/SECURITY.md)** - Security strategy, validation layers, threat model, protections
- **[DECISIONS.md](design/DECISIONS.md)** - Architecture Decision Records (ADRs) with rationale and trade-offs
- **[ADR-001-KERNEL-PANIC-MITIGATION.md](design/ADR-001-KERNEL-PANIC-MITIGATION.md)** - Kernel panic mitigation strategy and fail-dumb pattern

### 📖 Guides (`guide/`)
Practical how-to documentation.

- **[QUICKSTART.md](guide/QUICKSTART.md)** - Zero-to-running setup, devicemapper pool creation, first image processing
- **[OPERATIONS.md](guide/OPERATIONS.md)** - Operational procedures, orphaned device cleanup, emergency recovery, maintenance
- **[USAGE.md](guide/USAGE.md)** - Complete usage guide with CLI commands, workflows, database queries, troubleshooting
- **[OBSERVABILITY.md](guide/OBSERVABILITY.md)** - Logging, tracing, metrics, and request correlation guide
- **[INTEGRATION_TESTING.md](guide/INTEGRATION_TESTING.md)** - Integration test suite and procedures
- **[DEVELOPMENT.md](guide/DEVELOPMENT.md)** - Developer workflow, build commands, testing, debugging, contributing
- **[TROUBLESHOOTING.md](guide/TROUBLESHOOTING.md)** - Common issues, error messages, solutions, debugging techniques

### 📓 Notes (`note/`)
Optional engineering context for maintainers.

- **[FLY_CONTEXT.md](note/FLY_CONTEXT.md)** - Engineering philosophy, FSM patterns, hot deploy strategy
- **[DB_PERSISTENCE_DIAGNOSTIC.md](note/DB_PERSISTENCE_DIAGNOSTIC.md)** - SQLite durability and persistence notes
- **[DEVICEMAPPER_OPTIMIZATION.md](note/DEVICEMAPPER_OPTIMIZATION.md)** - Devicemapper performance and tuning notes
- **[KERNEL_PANIC_ROOT_CAUSE.md](note/KERNEL_PANIC_ROOT_CAUSE.md)** - Root cause summary for kernel panic behavior

---

## Implementation Status

### ✅ Phase 1: Architecture & Design (COMPLETE)
- FSM state machine design
- Database schema design
- Request/Response type definitions
- Architecture documentation

### ✅ Phase 2: Foundation & Infrastructure (COMPLETE)
- SQLite database layer with migrations and CRUD operations
- S3 client wrapper with streaming downloads and validation
- DeviceMapper utilities for thin devices and snapshots
- Secure tarball extraction with security checks

### ✅ Phase 3: Download FSM Implementation (COMPLETE)
- check-exists transition (idempotency)
- download transition (S3 streaming)
- validate transition (checksum, tar structure, security)
- store-metadata transition (database update)
- FSM registration with queue configuration

### ✅ Phase 4: Unpack FSM Implementation (COMPLETE)
- check-unpacked transition
- create-device transition
- extract-layers transition
- verify-layout transition
- update-db transition

### ✅ Phase 5: Activation FSM Implementation (COMPLETE)
- check-snapshot transition
- create-snapshot transition
- register transition

### ✅ Phase 6: Orchestration & Main Application (COMPLETE)
- FSM Manager initialization
- FSM chaining logic
- CLI interface
- Configuration management

### ✅ Phase 7: Error Handling & Resilience (COMPLETE)
- Cleanup on failure (fail-dumb pattern)
- Retry strategies
- Security validations
- Crash recovery
- GC command for safe cleanup

### ✅ Phase 8: Integration & Validation (COMPLETE)
- Devicemapper pool setup
- Real S3 image testing
- Error scenario testing
- Concurrent operation testing
- Kernel panic mitigation (Priority 1 and Priority 2)

### ✅ Phase 9: Documentation & Polish (COMPLETE)
- Code documentation
- Usage documentation
- Logging and observability
- Final code review
- Comprehensive documentation updates

### ✅ Phase 10: TUI & Production Hardening (COMPLETE - 2025-11-26)
- Interactive TUI monitor dashboard with Bubble Tea
- S3 image browser with runtime/version/size display
- Real-time pool status monitoring
- Enhanced system health checks:
  - D-state process detection
  - Kernel dmesg error scanning
  - Memory pressure monitoring
  - I/O wait monitoring
  - Pool existence verification
- Post-kernel-panic recovery procedures

### ✅ Phase 11: System Safeguards (COMPLETE - 2025-11-26)
- Pool Manager with auto-creation and health validation
- Health Checker (D-state, kernel logs, memory, I/O)
- Operation Guard for serializing dm operations
- `setup-pool` CLI command for manual pool management
- See [Operations Guide - System Safeguards](guide/OPERATIONS.md#system-safeguards) for details

---

## Project Structure

```
flyio/
├── docs/                    # Documentation (you are here)
│   ├── INDEX.md            # This file
│   ├── spec/               # Requirements
│   ├── api/                # API reference
│   ├── design/             # Architecture & design
│   ├── guide/              # How-to guides
│   └── note/               # Context & logs
├── database/               # ✅ SQLite database layer
├── s3/                     # ✅ S3 client wrapper
├── devicemapper/           # ✅ DeviceMapper utilities + Pool Manager
├── safeguards/             # ✅ System stability safeguards
├── extraction/             # ✅ Tarball extraction
├── download/               # ✅ Download FSM
├── unpack/                 # ✅ Unpack FSM
├── activate/               # ✅ Activation FSM
├── tui/                    # ✅ Interactive TUI dashboard
│   ├── dashboard.go        # Main dashboard with views
│   ├── s3browser.go        # S3 image browser
│   └── fetcher.go          # Data fetching
└── cmd/                    # ✅ CLI entry point
    └── flyio-image-manager/
        ├── main.go         # Main application
        └── gc.go           # Garbage collection command
```

---

## Related Files

- **[README.md](../README.md)** - Project overview and quick links
- **[CLAUDE.md](../CLAUDE.md)** - AI assistant context and FSM library guide
- **[go.mod](../go.mod)** - Go module dependencies

---

## Contributing

See [Development Guide](guide/DEVELOPMENT.md) for build instructions, testing guidelines, and contribution workflow.


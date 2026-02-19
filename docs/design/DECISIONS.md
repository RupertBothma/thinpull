# Architecture Decision Records

**Document Type**: Design  
**Status**: Living Document  
**Version**: 1.0  
**Last Updated**: 2025-11-21

---

## Overview

This document captures Architecture Decision Records (ADRs) for the Fly.io Container Image Management System. Each ADR follows the format: Context, Decision, Rationale, Consequences, Alternatives Considered.

---

## ADR-001: SQLite Driver Selection

**Status**: Accepted  
**Date**: 2025-11-21  
**Deciders**: Implementation Team

### Context

The system requires a SQLite database for tracking image state. Two main Go SQLite drivers exist:
1. `mattn/go-sqlite3` - CGo-based, mature, widely used
2. `modernc.org/sqlite` - Pure Go, newer, no CGo

### Decision

Use `modernc.org/sqlite` as the SQLite driver.

### Rationale

- **Pure Go**: No CGo required, simpler build process
- **Cross-compilation**: Easier to build for different platforms
- **No C Dependencies**: No C compiler required for builds
- **Comparable Performance**: Sufficient for our use case
- **Simpler Deployment**: Single binary with no external dependencies

### Consequences

**Positive**:
- Simpler build and deployment process
- Easier cross-compilation
- No C compiler dependencies
- Single static binary

**Negative**:
- Slightly slower than CGo version for some operations (~10-20%)
- Less battle-tested than mattn/go-sqlite3
- Smaller community and ecosystem

**Mitigation**:
- Performance difference is acceptable for our use case
- Pure Go implementation is more maintainable long-term

### Alternatives Considered

1. **mattn/go-sqlite3** (CGo-based):
   - Pros: Faster, more mature, larger community
   - Cons: Requires CGo, C compiler, harder cross-compilation
   - Rejected: Build complexity outweighs performance benefits

2. **Embedded database (BoltDB, BadgerDB)**:
   - Pros: Pure Go, optimized for Go
   - Cons: No SQL, different query model, less familiar
   - Rejected: SQL provides better query flexibility

---

## ADR-002: Database Configuration (WAL Mode)

**Status**: Accepted  
**Date**: 2025-11-21  
**Deciders**: Implementation Team

### Context

SQLite supports multiple journal modes and synchronous settings. The choice affects concurrency, durability, and performance.

### Decision

Use WAL (Write-Ahead Logging) mode with NORMAL synchronous setting.

**Configuration**:
```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA cache_size = -10000;  -- 10MB cache
PRAGMA busy_timeout = 5000;   -- 5 second timeout
```

### Rationale

- **WAL Mode**: Enables better concurrency (readers don't block writers)
- **NORMAL Synchronous**: Balances durability and performance
- **Crash Recovery**: FSM library provides crash recovery through event replay
- **Acceptable Risk**: Some data loss on crash is acceptable given FSM recovery

### Consequences

**Positive**:
- Better concurrency for read-heavy workloads
- Improved performance for write operations
- Reduced lock contention

**Negative**:
- Potential data loss on crash (last few transactions)
- Additional WAL file to manage
- Slightly more complex backup process

**Mitigation**:
- FSM event replay provides crash recovery
- WAL checkpoint on clean shutdown
- Regular backups of both DB and WAL files

### Alternatives Considered

1. **DELETE mode with FULL synchronous**:
   - Pros: Maximum durability, simpler
   - Cons: Poor concurrency, slower writes
   - Rejected: Concurrency is important for our use case

2. **WAL mode with FULL synchronous**:
   - Pros: Better concurrency, maximum durability
   - Cons: Slower writes, unnecessary given FSM recovery
   - Rejected: Performance cost not justified

---

## ADR-003: Custom Error Types for DeviceMapper

**Status**: Accepted  
**Date**: 2025-11-21  
**Deciders**: Implementation Team

### Context

DeviceMapper operations can fail in different ways. FSMs need to distinguish between recoverable and unrecoverable errors to make intelligent retry decisions.

### Decision

Create custom error types for devicemapper operations:
- `DeviceExistsError`: Device already exists
- `PoolFullError`: Pool is full
- `DeviceNotFoundError`: Device not found

### Rationale

- **Intelligent Retry**: FSM can decide whether to retry or abort
- **Type Safety**: Compile-time checking with helper functions
- **Clear Semantics**: Error type indicates recovery strategy
- **Better Debugging**: Specific error types aid troubleshooting

### Consequences

**Positive**:
- FSMs can make intelligent retry decisions
- Clear error handling patterns
- Better error messages and logging
- Type-safe error checking

**Negative**:
- More code to maintain
- Need to parse dmsetup output to detect error types

**Mitigation**:
- Helper functions (`IsDeviceExistsError`, etc.) simplify checking
- Comprehensive error parsing tests

### Alternatives Considered

1. **String matching on error messages**:
   - Pros: Simpler implementation
   - Cons: Fragile, not type-safe, hard to maintain
   - Rejected: Type safety is important

2. **Error codes (int constants)**:
   - Pros: Simple, efficient
   - Cons: Not idiomatic Go, less type-safe
   - Rejected: Custom error types are more idiomatic

---

## ADR-004: Defense-in-Depth Security Strategy

**Status**: Accepted  
**Date**: 2025-11-21  
**Deciders**: Implementation Team

### Context

The system must handle potentially malicious container images in a hostile environment. Security is a critical requirement.

### Decision

Implement defense-in-depth with multiple validation layers:
1. S3 key validation
2. Tar header validation
3. Path sanitization
4. Symlink target validation
5. Filesystem layout verification

### Rationale

- **Hostile Environment**: Assume all input is malicious
- **Multiple Barriers**: If one layer fails, others provide protection
- **Fail Closed**: Reject suspicious content rather than allow
- **Comprehensive Coverage**: Validate at every boundary

### Consequences

**Positive**:
- Strong security posture
- Protection against multiple attack vectors
- Clear security audit trail
- Confidence in handling untrusted input

**Negative**:
- More code complexity
- Potential false positives
- Performance overhead for validation

**Mitigation**:
- Comprehensive testing with malicious inputs
- Clear logging of security violations
- Performance optimization for validation code

### Alternatives Considered

1. **Single validation layer**:
   - Pros: Simpler, faster
   - Cons: Single point of failure, less secure
   - Rejected: Security is critical requirement

2. **Sandboxing (containers, VMs)**:
   - Pros: Strong isolation
   - Cons: Complex, resource-intensive, out of scope
   - Rejected: Not required for this implementation

---

## ADR-005: Strict Resource Limits

**Status**: Accepted  
**Date**: 2025-11-21  
**Deciders**: Implementation Team

### Context

Malicious or corrupted images could attempt resource exhaustion attacks. The system must protect against these.

### Decision

Enforce strict resource limits:
- Max file size: 1GB per file
- Max total size: 10GB per image
- Max file count: 100,000 files per image
- Max device size: 100GB per device
- Download timeout: 5 minutes
- Extraction timeout: 30 minutes
- Max concurrent downloads: 5
- Max concurrent unpacking: 2

### Rationale

- **Prevent Exhaustion**: Protect against resource exhaustion attacks
- **Predictable Usage**: Ensure predictable resource consumption
- **Align with Reality**: Limits align with typical container image sizes
- **Fail Fast**: Detect and reject malicious content early

### Consequences

**Positive**:
- Protection against resource exhaustion
- Predictable resource usage
- Clear error messages when limits exceeded
- System stability

**Negative**:
- May reject legitimate large images
- Limits may need adjustment over time

**Mitigation**:
- Limits are configurable
- Clear error messages explain limit violations
- Monitoring to detect if limits are too restrictive

### Alternatives Considered

1. **No limits**:
   - Pros: Maximum flexibility
   - Cons: Vulnerable to resource exhaustion
   - Rejected: Security requirement

2. **Dynamic limits based on available resources**:
   - Pros: More flexible
   - Cons: Complex, unpredictable behavior
   - Rejected: Simplicity and predictability preferred

---

## ADR-006: Idempotency via Database Checks and fsm.Handoff

**Status**: Accepted  
**Date**: 2025-11-21  
**Deciders**: Implementation Team

### Context

FSMs may be restarted or run multiple times for the same image. The system must avoid redundant work while ensuring correctness.

### Decision

Implement idempotency by:
1. Checking database before performing work
2. Verifying resources still exist and are valid
3. Using `fsm.Handoff` to skip remaining transitions if work is done

### Rationale

- **Efficiency**: Avoid redundant downloads, unpacking, activation
- **Safety**: Verify resources are still valid before skipping
- **Consistency**: Same pattern across all FSMs
- **FSM Integration**: Leverages FSM library's Handoff mechanism

### Consequences

**Positive**:
- Efficient handling of retries and restarts
- Safe idempotency with validation
- Consistent pattern across FSMs
- Clear audit trail in database

**Negative**:
- Additional database queries
- Need to verify resource validity (file exists, checksum matches, device active)

**Mitigation**:
- Database queries are fast (indexed lookups)
- Validation is necessary for correctness anyway

### Alternatives Considered

1. **Deterministic IDs only (no database check)**:
   - Pros: Simpler, no database dependency
   - Cons: Can't verify resource validity, may skip broken resources
   - Rejected: Safety is important

2. **Always redo work**:
   - Pros: Simplest implementation
   - Cons: Wasteful, slow, unnecessary S3 costs
   - Rejected: Efficiency is important

---

## ADR-007: Streaming Downloads with Single-Pass Checksum

**Status**: Accepted  
**Date**: 2025-11-21  
**Deciders**: Implementation Team

### Context

Container images can be large (up to 10GB). Loading entire files into memory is not feasible.

### Decision

Stream downloads directly to disk while computing checksum in a single pass using `io.MultiWriter`.

**Implementation**:
```go
hash := sha256.New()
multiWriter := io.MultiWriter(tmpFile, hash)
io.Copy(multiWriter, s3Response.Body)
checksum := hex.EncodeToString(hash.Sum(nil))
```

### Rationale

- **Memory Efficiency**: Avoid loading entire file into memory
- **Single Pass**: Compute checksum during download (no second read)
- **Performance**: Streaming is faster than buffering
- **Simplicity**: Standard library provides all needed tools

### Consequences

**Positive**:
- Low memory footprint (constant memory usage)
- Fast downloads (no buffering overhead)
- Efficient checksum computation (single pass)
- Simple implementation

**Negative**:
- Cannot retry partial downloads (must restart from beginning)
- Temporary file required

**Mitigation**:
- Atomic move from temporary to final location
- Cleanup temporary files on failure
- Future enhancement: support resumable downloads

### Alternatives Considered

1. **Buffer entire file in memory**:
   - Pros: Simpler error handling
   - Cons: Memory exhaustion for large files
   - Rejected: Not feasible for 10GB files

2. **Two-pass (download, then checksum)**:
   - Pros: Simpler code
   - Cons: Slower (read file twice), more I/O
   - Rejected: Performance cost not justified

---

## ADR-008: Queue-Based Concurrency Control

**Status**: Accepted  
**Date**: 2025-11-21  
**Deciders**: Implementation Team

### Context

Unlimited concurrent operations could exhaust network bandwidth, I/O capacity, or devicemapper pool.

### Decision

Use FSM library's queue mechanism to limit concurrency:
- Downloads: max 5 concurrent (queue: `"downloads"`)
- Unpacking: max 2 concurrent (queue: `"unpacking"`)
- Activation: unlimited (no queue)

### Rationale

- **Network Management**: Limit concurrent S3 downloads to avoid bandwidth exhaustion
- **I/O Management**: Limit concurrent unpacking to avoid I/O contention
- **Fast Operations**: Activation is fast, no need to limit
- **FSM Integration**: Leverages FSM library's built-in queue support

### Consequences

**Positive**:
- Controlled resource usage
- Predictable performance
- No resource exhaustion
- Simple configuration

**Negative**:
- Queued operations wait for available slots
- May need tuning based on hardware

**Mitigation**:
- Limits are configurable
- Monitoring to detect if limits are too restrictive
- Can adjust based on observed performance

### Alternatives Considered

1. **No limits**:
   - Pros: Maximum throughput
   - Cons: Resource exhaustion, unpredictable performance
   - Rejected: Stability is important

2. **Semaphore-based limiting**:
   - Pros: More flexible
   - Cons: More complex, FSM library provides queues
   - Rejected: FSM queues are simpler and sufficient

---

## Summary

These ADRs capture the key architectural decisions made during implementation. They provide context for future maintainers and document the rationale behind design choices.

For security-specific decisions, see [Security Design](SECURITY.md).


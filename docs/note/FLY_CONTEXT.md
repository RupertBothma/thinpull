# Engineering Context

**Document Type**: Note  
**Audience**: Engineers, Architects  
**Last Updated**: 2025-11-21

---

## Overview

This document captures insights into Fly.io's engineering philosophy and patterns, particularly around FSM-based orchestration, as demonstrated in this container image management system.

**Context**: This project demonstrates production-ready FSM-based orchestration patterns for resilient distributed systems, inspired by Fly.io's approach to host-level autonomy.

---

## Fly.io Engineering Philosophy

### Single-Host Autonomy

**Principle**: Each host should be able to operate independently without relying on external coordination.

**Application in This Project**:
- SQLite for local state (no external database dependency)
- FSM library for local orchestration (no external workflow engine)
- S3 for shared storage (simple, reliable, no complex coordination)
- DeviceMapper for local storage (kernel-level, no external storage service)

**Benefits**:
- **Resilience**: Host failures don't cascade
- **Simplicity**: No distributed consensus protocols
- **Performance**: No network round-trips for coordination
- **Debuggability**: All state is local and inspectable

**Trade-offs**:
- No global view of system state
- Coordination must be explicit (via S3, database, etc.)
- Each host must be self-sufficient

---

## FSM-Based Orchestration

### Why FSMs?

**Problem**: Traditional imperative code is hard to reason about when failures occur mid-execution.

**Solution**: Model workflows as explicit state machines with well-defined transitions.

**Benefits**:
1. **Crash Recovery**: FSM state is persisted, can resume after crash
2. **Observability**: Current state is always known and queryable
3. **Retry Logic**: Built-in retry with exponential backoff
4. **Idempotency**: Easy to implement via state checks
5. **Debugging**: Event log provides complete execution history

**Example**:
```
Traditional Code (Imperative):
  download()
  unpack()
  activate()
  // What if crash happens between unpack() and activate()?
  // How do we know what was completed?

FSM Code (Declarative):
  START → download → unpack → activate → COMPLETE
  // State is persisted after each transition
  // Can resume from any state after crash
  // Event log shows exactly what happened
```

---

## FSM Linearity and Idempotency

### Linear State Machines

**Principle**: FSMs should be linear (no branching) with idempotent transitions.

**Rationale**:
- **Simplicity**: Linear flow is easy to understand and debug
- **Predictability**: Always know the next state
- **Testability**: Easy to test each transition in isolation

**Pattern**:
```
START → check → work → validate → persist → COMPLETE
         ↓ (already done)
         └────────────────────────────────────→ COMPLETE (Handoff)
```

**Anti-Pattern** (Avoid):
```
START → check → work
         ↓ (condition A)
         ├─→ path1 → end1
         └─→ path2 → end2
// Branching makes state harder to reason about
```

### Idempotency via Check-Then-Work

**Pattern**: Every FSM starts with a check transition that verifies if work is already done.

**Implementation**:
1. Query database/filesystem for existing state
2. Verify state is still valid (file exists, checksum matches, device active)
3. If valid, return `fsm.Handoff` to skip remaining transitions
4. If invalid or missing, proceed with work

**Benefits**:
- **Efficiency**: Avoid redundant work
- **Safety**: Verify resources before skipping
- **Consistency**: Same pattern across all FSMs

**Example**:
```go
func checkExists(ctx context.Context, req *fsm.Request[R, W]) (*fsm.Response[W], error) {
    // Query database
    existing, err := db.CheckImageDownloaded(ctx, req.Msg.S3Key)
    if err != nil {
        return nil, err // Retry on database error
    }
    
    // If not found, proceed with work
    if existing == nil {
        return nil, nil
    }
    
    // Verify resource is still valid
    if !fileValid(existing.LocalPath, existing.Checksum) {
        return nil, nil // Re-download
    }
    
    // Work already done, skip remaining transitions
    return fsm.NewResponse(&W{...}), fsm.Handoff(ulid.ULID{})
}
```

---

## Hot Deploy Strategy

### Zero-Downtime Deployments

**Challenge**: Deploy new code without interrupting in-flight operations.

**FSM Solution**:
1. **Graceful Shutdown**: Stop accepting new work, wait for active FSMs to complete
2. **State Persistence**: All FSM state is persisted in database
3. **Resume on Startup**: New version resumes active FSMs from persisted state
4. **Version Compatibility**: FSM event log is forward-compatible

**Deployment Flow**:
```
1. Deploy new version
2. Old version: Stop accepting new FSMs
3. Old version: Wait for active FSMs to complete (or timeout)
4. Old version: Shutdown
5. New version: Start
6. New version: Resume active FSMs from database
7. New version: Accept new work
```

**Benefits**:
- No lost work during deployment
- No manual intervention required
- Automatic recovery from crashes

---

## Recovery and Resume Patterns

### Crash Recovery

**FSM Library Behavior**:
1. On startup, query database for active FSMs
2. For each active FSM:
   - Load persisted state (Request, partial Response)
   - Determine last completed transition
   - Resume from next transition
3. Continue execution as if no crash occurred

**Implementation Requirements**:
- All state must be in Request/Response types (no local variables)
- Transitions must be idempotent (safe to retry)
- Side effects must be tracked in database

**Example**:
```go
// On startup
if err := manager.Start(ctx); err != nil {
    log.Fatal(err)
}

// Resume all active FSMs
if err := resumeDownloads(ctx); err != nil {
    log.Fatal(err)
}
if err := resumeUnpacks(ctx); err != nil {
    log.Fatal(err)
}
if err := resumeActivates(ctx); err != nil {
    log.Fatal(err)
}
```

### Partial Work Cleanup

**Challenge**: What if crash happens mid-transition (e.g., file partially downloaded)?

**Solution**: Use temporary files and atomic operations.

**Pattern**:
```go
func download(ctx context.Context, req *fsm.Request[R, W]) (*fsm.Response[W], error) {
    // Download to temporary file
    tmpPath := "/tmp/download-" + uuid.New().String()
    defer os.Remove(tmpPath) // Cleanup on error
    
    // Download
    if err := downloadToFile(ctx, tmpPath); err != nil {
        return nil, err // Temporary file cleaned up by defer
    }
    
    // Atomic move to final location
    finalPath := "/var/lib/flyio/images/" + req.Msg.ImageID + ".tar"
    if err := os.Rename(tmpPath, finalPath); err != nil {
        return nil, err
    }
    
    // Success: file is complete or doesn't exist (atomic)
    return fsm.NewResponse(&W{LocalPath: finalPath}), nil
}
```

---

## Observability Patterns

### Structured Logging

**Principle**: Every log entry should have contextual fields for filtering and correlation.

**FSM Integration**:
```go
func transition(ctx context.Context, req *fsm.Request[R, W]) (*fsm.Response[W], error) {
    logger := req.Log().WithFields(logrus.Fields{
        "image_id": req.Msg.ImageID,
        "s3_key":   req.Msg.S3Key,
    })
    
    logger.Info("starting download")
    // ...
    logger.WithField("size", size).Info("download complete")
}
```

**Benefits**:
- Easy to filter logs by image_id, s3_key, etc.
- Correlation across transitions
- FSM library automatically adds run_id, version, transition fields

### Metrics and Tracing

**FSM Library Built-in**:
- Prometheus metrics for action/transition counts and durations
- OpenTelemetry spans for distributed tracing
- Automatic instrumentation (no manual code required)

**Usage**:
```go
// Metrics automatically exposed at /metrics
// Traces automatically sent to configured backend
// No manual instrumentation needed
```

---

## Production Readiness Checklist

Based on Fly.io patterns, a production-ready system should have:

- [x] **FSM-based orchestration** for crash recovery
- [x] **Idempotent transitions** for safe retries
- [x] **Local state persistence** (SQLite, BoltDB)
- [x] **Structured logging** with contextual fields
- [x] **Metrics and tracing** (Prometheus, OpenTelemetry)
- [x] **Error classification** (abort vs retry)
- [x] **Resource limits** (timeouts, concurrency, sizes)
- [x] **Security validations** (defense-in-depth)
- [x] **Health checks** for monitoring (Pool Manager, Health Checker, Operation Guard)
- [ ] **Graceful shutdown** for deployments
- [ ] **Admin interface** for debugging (FSM library provides this)

---

## Key Takeaways

### 1. FSMs Enable Resilience

State machines with persisted state enable crash recovery and hot deploys without manual intervention.

### 2. Idempotency is Essential

Every operation should be safe to retry. Check-then-work pattern ensures efficiency and correctness.

### 3. Local State is Powerful

SQLite and BoltDB provide reliable local state without distributed coordination complexity.

### 4. Observability is Built-in

Structured logging, metrics, and tracing should be automatic, not manual.

### 5. Security is Layered

Defense-in-depth with multiple validation layers protects against diverse attack vectors.

---

## References

- [System Architecture](../design/SYSTEM_ARCH.md) - FSM-based architecture
- [FSM Flow Design](../design/FSM_FLOWS.md) - State machine patterns
- [FSM Library API](../api/FSM_LIBRARY.md) - FSM framework reference
- [Architecture Decisions](../design/DECISIONS.md) - Design rationale
- [Fly.io Blog](https://fly.io/blog/) - Engineering insights from Fly.io team


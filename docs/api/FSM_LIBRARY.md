# FSM Library API Reference

**Document Type**: API Reference  
**Status**: Approved  
**Version**: 1.0  
**Last Updated**: 2025-11-21

---

## Overview

This document provides the complete API reference for the FSMv2 (Finite State Machine) library, which provides the foundation for the container image management system.

**Library Location**: Root package (`github.com/superfly/fsm`)

---

## Core Types

### Manager

The central coordinator for FSM lifecycle, runs, and persistence.

```go
type Manager struct {
    // Internal fields (not exported)
}

type Config struct {
    Logger  *logrus.Logger
    DBPath  string              // Path to FSM state database
    Queues  map[string]int      // Queue name -> concurrency limit
}

// New creates a new FSM manager
func New(cfg Config) (*Manager, error)

// Start begins the manager and admin server
func (m *Manager) Start(ctx context.Context) error

// Stop gracefully shuts down the manager
func (m *Manager) Stop(ctx context.Context) error

// Children returns FSMs associated with a parent
func (m *Manager) Children(ctx context.Context, parent ulid.ULID) ([]ulid.ULID, error)

// ActiveChildren returns active FSMs started from a parent
func (m *Manager) ActiveChildren(ctx context.Context, parent ulid.ULID) ([]Run, error)
```

**Example**:
```go
manager, err := fsm.New(fsm.Config{
    DBPath: "/var/lib/flyio/fsm",
    Queues: map[string]int{
        "downloads": 5,  // Max 5 concurrent downloads
        "unpacking": 2,  // Max 2 concurrent unpacking
    },
})
if err != nil {
    log.Fatal(err)
}

if err := manager.Start(ctx); err != nil {
    log.Fatal(err)
}
defer manager.Stop(ctx)
```

---

## FSM Registration

### Register

Creates a new FSM builder for defining state machines.

```go
func Register[R, W any](m *Manager, action string) *fsmStart[R, W]
```

**Type Parameters**:
- `R`: Request type (input data)
- `W`: Response type (accumulated output data)

**Parameters**:
- `m`: Manager instance
- `action`: Action name (used for metrics and logging)

**Returns**: FSM builder for defining transitions

### Builder Methods

#### Start

Defines the initial state and transition.

```go
func (b *fsmStart[R, W]) Start(
    state string,
    fn Transition[R, W],
    opts ...StartOption[R, W],
) *fsmTransition[R, W]
```

**Parameters**:
- `state`: State name
- `fn`: Transition function
- `opts`: Optional configuration (initializers, interceptors)

#### To

Defines the next state transition.

```go
func (t *fsmTransition[R, W]) To(
    state string,
    fn Transition[R, W],
    opts ...Option[R, W],
) *fsmTransition[R, W]
```

**Parameters**:
- `state`: State name
- `fn`: Transition function
- `opts`: Optional configuration (timeout, interceptors)

#### End

Marks the final state.

```go
func (t *fsmTransition[R, W]) End(
    state string,
    opts ...EndOption[R, W],
) *fsmEnd[R, W]
```

#### Build

Returns start and resume functions.

```go
func (e *fsmEnd[R, W]) Build(ctx context.Context) (Start[R, W], Resume, error)
```

**Returns**:
- `Start[R, W]`: Function to start new FSM runs
- `Resume`: Function to resume active FSMs after restart
- `error`: Error if registration fails

**Example**:
```go
startDownload, resumeDownloads, err := fsm.Register[ImageDownloadRequest, ImageDownloadResponse](manager, "download-image").
    Start("check-exists", checkExists).
    To("download", downloadFromS3, fsm.WithTimeout(5*time.Minute)).
    To("validate", validateBlob).
    To("store-metadata", storeMetadata).
    End("complete").
    Build(ctx)
```

---

## Request and Response

### Request

Strongly-typed request passed through FSM transitions.

```go
type Request[R, W any] struct {
    Msg R           // Input data (read-only)
    W   Response[W] // Accumulated response data
}

// Log returns the FSM's logger with contextual fields
func (r *Request[R, W]) Log() logrus.FieldLogger

// Run returns the current run information
func (r *Request[R, W]) Run() Run
```

**Fields**:
- `Msg`: Input data of type `R` (read-only, set at FSM start)
- `W`: Accumulated response data of type `W` (updated by each transition)

**Methods**:
- `Log()`: Returns logger with contextual fields (transition, version, run ID)
- `Run()`: Returns run information (ID, version, state, error)

### Response

Strongly-typed response returned from transitions.

```go
type Response[W any] struct {
    Msg *W
}

// NewResponse creates a response with data
func NewResponse[W any](w *W) *Response[W]
```

**Example**:
```go
func downloadFromS3(ctx context.Context, req *fsm.Request[ImageDownloadRequest, ImageDownloadResponse]) (*fsm.Response[ImageDownloadResponse], error) {
    logger := req.Log().WithField("transition", "download")
    s3Key := req.Msg.S3Key
    
    // Download from S3
    result, err := s3Client.Download(ctx, s3Key)
    if err != nil {
        logger.WithError(err).Error("download failed")
        return nil, err
    }
    
    // Return response
    resp := &ImageDownloadResponse{
        LocalPath: result.Path,
        Checksum:  result.Checksum,
        SizeBytes: result.Size,
    }
    
    return fsm.NewResponse(resp), nil
}
```

---

## Transition Function

### Transition

The core function type for state transitions.

```go
type Transition[R, W any] func(
    ctx context.Context,
    req *Request[R, W],
) (*Response[W], error)
```

**Parameters**:
- `ctx`: Context for cancellation and timeouts
- `req`: Request with input data and accumulated response

**Returns**:
- `*Response[W]`: Response data (can be nil to pass through previous response)
- `error`: Error if transition fails (triggers retry or abort)

**Error Handling**:
- Standard error: Auto-retry with exponential backoff
- `fsm.Abort(err)`: Stop immediately, no retries, mark as aborted
- `fsm.Handoff(ulid.ULID{})`: Skip remaining transitions, mark as complete
- `fsm.Unrecoverable(err)`: Stop immediately, mark as failed

---

## Start and Resume Functions

### Start

Function to start a new FSM run.

```go
type Start[R, W any] func(
    ctx context.Context,
    id string,
    req *Request[R, W],
    opts ...StartOptionsFn,
) (ulid.ULID, error)
```

**Parameters**:
- `ctx`: Context for cancellation
- `id`: Unique identifier for this run
- `req`: Request with input data
- `opts`: Start options (queue, delay, parent, etc.)

**Returns**:
- `ulid.ULID`: Run version (unique ID for this FSM run)
- `error`: Error if start fails

**Example**:
```go
req := fsm.NewRequest(&ImageDownloadRequest{
    S3Key:   "images/alpine.tar",
    ImageID: "alpine-001",
}, &ImageDownloadResponse{})

runID, err := startDownload(ctx, "alpine-001", req, 
    fsm.WithQueue("downloads"))
if err != nil {
    log.Fatal(err)
}

fmt.Printf("Started download FSM: %s\n", runID)
```

### Resume

Function to resume active FSMs after restart.

```go
type Resume func(context.Context) error
```

**Usage**:
```go
// Resume all active downloads after restart
if err := resumeDownloads(ctx); err != nil {
    log.Fatal(err)
}
```

---

## Start Options

### WithQueue

Adds FSM to a queue with concurrency limit.

```go
func WithQueue(name string) StartOptionsFn
```

**Example**:
```go
runID, err := startDownload(ctx, id, req, fsm.WithQueue("downloads"))
```

### WithDelayedStart

Delays FSM start until a specific time.

```go
func WithDelayedStart(t time.Time) StartOptionsFn
```

**Example**:
```go
runID, err := startDownload(ctx, id, req, 
    fsm.WithDelayedStart(time.Now().Add(5*time.Minute)))
```

### WithRunAfter

Waits for another FSM to complete before starting.

```go
func WithRunAfter(version ulid.ULID) StartOptionsFn
```

**Example**:
```go
// Start unpack after download completes
unpackRunID, err := startUnpack(ctx, id, req, 
    fsm.WithRunAfter(downloadRunID))
```

### WithParent

Links FSM as a child of a parent FSM.

```go
func WithParent(parent ulid.ULID) StartOptionsFn
```

**Example**:
```go
runID, err := startDownload(ctx, id, req, 
    fsm.WithParent(parentRunID))
```

---

## Transition Options

### WithTimeout

Sets a timeout for a specific transition.

```go
func WithTimeout(d time.Duration) Option[R, W]
```

**Example**:
```go
fsm.Register[Req, Resp](manager, "action").
    Start("state1", fn1).
    To("state2", fn2, fsm.WithTimeout(5*time.Minute)).
    End("complete").
    Build(ctx)
```

### WithInitializers

Adds initializers to run before the first transition.

```go
func WithInitializers[R, W any](i ...Initializer[R, W]) StartOption[R, W]

type Initializer[R, W any] func(context.Context, *Request[R, W]) context.Context
```

**Example**:
```go
func addTracing(ctx context.Context, req *fsm.Request[Req, Resp]) context.Context {
    span := trace.StartSpan(ctx, "fsm-run")
    return span.Context()
}

fsm.Register[Req, Resp](manager, "action").
    Start("state1", fn1, fsm.WithInitializers(addTracing)).
    // ...
```

---

## Error Types

### Abort

Stops FSM without retries, marks as aborted.

```go
func Abort(err error) error
```

**Use Cases**:
- Validation failures
- Security violations
- Resource exhaustion

**Example**:
```go
if size > maxSize {
    return nil, fsm.Abort(fmt.Errorf("file too large: %d bytes", size))
}
```

### Handoff

Skips remaining transitions, marks as complete.

```go
func Handoff(version ulid.ULID) error
```

**Use Cases**:
- Idempotency (work already done)
- Delegation to another FSM

**Example**:
```go
if imageExists && checksumValid {
    logger.Info("image already downloaded, skipping")
    return fsm.NewResponse(resp), fsm.Handoff(ulid.ULID{})
}
```

### Unrecoverable

Stops FSM permanently with error state.

```go
func NewUnrecoverableSystemError(err error) *UnrecoverableError
func NewUnrecoverableUserError(err error) *UnrecoverableError
```

**Use Cases**:
- Permanent errors
- Data corruption

**Example**:
```go
if corruptedData {
    return nil, fsm.NewUnrecoverableSystemError(
        fmt.Errorf("data corruption detected"))
}
```

---

## Run Information

### Run

Information about an FSM run.

```go
type Run struct {
    ID                string
    Version           ulid.ULID
    CurrentState      string
    TransitionVersion ulid.ULID
    // ... other fields
}
```

**Access**:
```go
func myTransition(ctx context.Context, req *fsm.Request[R, W]) (*fsm.Response[W], error) {
    run := req.Run()
    logger := req.Log().WithFields(logrus.Fields{
        "run_id":  run.ID,
        "version": run.Version,
        "state":   run.CurrentState,
    })
    // ...
}
```

---

## References

- [FSM Flow Design](../design/FSM_FLOWS.md) - State machine flows and patterns
- [System Architecture](../design/SYSTEM_ARCH.md) - FSM role in system
- [API Interfaces](INTERFACES.md) - Request/Response types
- [CLAUDE.md](../../CLAUDE.md) - FSM library deep dive


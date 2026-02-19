# Development Guide

**Document Type**: Guide  
**Audience**: Contributors, Developers  
**Prerequisites**: Go 1.21+, Linux system  
**Last Updated**: 2025-11-21

---

## Overview

This guide covers the development workflow for the Fly.io Container Image Management System, including project structure, build commands, testing strategies, and contribution guidelines.

---

## Project Structure

```
flyio/
├── cmd/
│   └── flyio-image-manager/    # ✅ CLI entry point
│       └── main.go              # 4 commands: process-image, list-images, list-snapshots, daemon
│
├── database/                    # ✅ SQLite database layer
│   ├── database.go              # Connection management, migrations
│   ├── schema.go                # DDL for tables and indexes
│   ├── models.go                # Data models
│   ├── images.go                # Image CRUD operations
│   ├── unpacked.go              # Unpacked image operations
│   └── snapshots.go             # Snapshot operations
│
├── s3/                          # ✅ S3 client wrapper
│   └── client.go                # Streaming downloads, validation
│
├── devicemapper/                # ✅ DeviceMapper utilities
│   └── dm.go                    # Thin devices, snapshots, lifecycle
│
├── extraction/                  # ✅ Tarball extraction
│   └── extract.go               # Secure extraction, validation
│
├── download/                    # ✅ Download FSM
│   └── fsm.go                   # check-exists → download → validate → store-metadata
│
├── unpack/                      # ✅ Unpack FSM
│   └── fsm.go                   # check-unpacked → create-device → extract → verify → update-db
│
├── activate/                    # ✅ Activation FSM
│   └── fsm.go                   # check-snapshot → create-snapshot → register
│
├── docs/                        # Documentation
│   ├── INDEX.md                 # Master navigation
│   ├── spec/                    # Requirements
│   ├── api/                     # API reference
│   ├── design/                  # Architecture & design
│   ├── guide/                   # How-to guides
│   └── note/                    # Context & logs
│
├── proto/                       # Protobuf definitions (FSM library)
│   └── fsm/v1/
│
├── go.mod                       # Go module dependencies
├── go.sum                       # Dependency checksums
├── README.md                    # Project overview
└── CLAUDE.md                    # AI assistant context
```

---

## Development Setup

### Prerequisites

1. **Go 1.21+**: [Download](https://go.dev/dl/)
2. **Git**: Version control
3. **Linux**: DeviceMapper requires Linux kernel
4. **Root access**: For devicemapper operations
5. **Tools**: `dmsetup`, `losetup`, `mkfs.ext4`

### Clone Repository

```bash
git clone https://github.com/RupertBothma/flyio.git
cd flyio
```

### Install Dependencies

```bash
# Download Go dependencies
go mod download

# Verify dependencies
go mod verify

# Tidy up (remove unused dependencies)
go mod tidy
```

### Install Development Tools

```bash
# Install protobuf compiler (for code generation)
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest

# Install buf (protobuf build tool)
go install github.com/bufbuild/buf/cmd/buf@latest

# Install golangci-lint (linter)
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

---

## Build Commands

### Build Application

```bash
# Build for current platform
go build -o flyio-image-manager ./cmd/flyio-image-manager

# Build with version information
VERSION=$(git describe --tags --always --dirty)
go build -ldflags "-X main.Version=$VERSION" -o flyio-image-manager ./cmd/flyio-image-manager

# Build for Linux (cross-compile from macOS)
GOOS=linux GOARCH=amd64 go build -o flyio-image-manager-linux ./cmd/flyio-image-manager
```

### Code Generation

```bash
# Generate protobuf code
go generate ./...

# Or manually with buf
buf generate
```

### Clean Build

```bash
# Remove build artifacts
rm -f flyio-image-manager flyio-image-manager-linux

# Clean Go cache
go clean -cache -modcache -testcache
```

---

## Testing Strategy

### Unit Tests

Test individual functions and components in isolation.

**Location**: `*_test.go` files alongside source code

**Run Tests**:
```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -cover ./...

# Run tests with detailed coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Run tests for specific package
go test ./database

# Run specific test
go test ./database -run TestCheckImageDownloaded

# Run tests with verbose output
go test -v ./...
```

**Example Test**:
```go
func TestCheckImageDownloaded(t *testing.T) {
    db := setupTestDB(t)
    defer db.Close()
    
    // Test case: image not found
    img, err := db.CheckImageDownloaded(context.Background(), "nonexistent")
    assert.NoError(t, err)
    assert.Nil(t, img)
    
    // Test case: image exists
    err = db.StoreImageMetadata(context.Background(), "img-1", "key-1", "/path", "checksum", 1000)
    assert.NoError(t, err)
    
    img, err = db.CheckImageDownloaded(context.Background(), "key-1")
    assert.NoError(t, err)
    assert.NotNil(t, img)
    assert.Equal(t, "img-1", img.ImageID)
}
```

### Integration Tests

Test component interactions (database + S3, database + devicemapper, etc.).

**Run Integration Tests**:
```bash
# Run with integration tag
go test -tags=integration ./...

# Run specific integration test
go test -tags=integration ./database -run TestDatabaseIntegration
```

### Security Tests

Test security validations with malicious inputs.

**Test Cases**:
- Path traversal attempts
- Symlink attacks
- Oversized files
- Excessive file counts
- Corrupted archives

**Example**:
```bash
# Create malicious tar with path traversal
mkdir -p test/malicious
echo "test" > test/malicious/../../../etc/passwd
tar -czf malicious.tar test/

# Test extraction (should reject)
go test ./extraction -run TestPathTraversal
```

### Performance Tests

Benchmark critical operations.

```bash
# Run benchmarks
go test -bench=. ./...

# Run specific benchmark
go test -bench=BenchmarkDownload ./download

# Run with memory profiling
go test -bench=. -benchmem ./...
```

---

## Code Quality

### Linting

```bash
# Run golangci-lint
golangci-lint run

# Run with auto-fix
golangci-lint run --fix

# Run specific linters
golangci-lint run --enable-only=errcheck,govet,staticcheck
```

### Formatting

```bash
# Format all Go files
go fmt ./...

# Check formatting (CI)
test -z $(gofmt -l .)

# Use goimports (organizes imports)
goimports -w .
```

### Vet

```bash
# Run go vet (static analysis)
go vet ./...
```

---

## Debugging

### Logging

The application uses structured logging with logrus.

**Enable Debug Logging**:
```bash
# Set log level
export LOG_LEVEL=debug

# Run with debug logging
./flyio-image-manager process-image --s3-key "images/test.tar" --image-id "test-001"
```

**Log Fields**:
- `image_id`: Image identifier
- `s3_key`: S3 object key
- `device_id`: DeviceMapper device ID
- `transition`: Current FSM transition
- `run_id`: FSM run ID

### Debugging FSM State

```bash
# Check FSM database
sqlite3 /var/lib/flyio/fsm/fsm-state.db

# List active FSMs
SELECT * FROM active;

# List events for a run
SELECT * FROM events WHERE run_id = '<run-id>';

# Check FSM status via CLI
./flyio-image-manager status --run-id <run-id>
```

### Debugging DeviceMapper

```bash
# List all devices
sudo dmsetup ls

# Check device status
sudo dmsetup status pool
sudo dmsetup status thin-12345

# Check device info
sudo dmsetup info thin-12345

# Check pool usage
sudo dmsetup status pool | awk '{print $6, $7}'
```

### Debugging Database

```bash
# Open database
sqlite3 /var/lib/flyio/images.db

# List tables
.tables

# Check images
SELECT image_id, s3_key, download_status FROM images;

# Check unpacked images
SELECT image_id, device_name, layout_verified FROM unpacked_images;

# Check snapshots
SELECT image_id, snapshot_name, active FROM snapshots;
```

---

## Contributing

### Workflow

1. **Create Feature Branch**:
   ```bash
   git checkout -b feature/my-feature
   ```

2. **Make Changes**:
   - Write code following Go idioms
   - Add tests for new functionality
   - Update documentation

3. **Test Changes**:
   ```bash
   go test ./...
   golangci-lint run
   ```

4. **Commit Changes**:
   ```bash
   git add .
   git commit -m "feat: add new feature"
   ```

5. **Push and Create PR**:
   ```bash
   git push origin feature/my-feature
   # Create pull request on GitHub
   ```

### Commit Message Format

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>

[optional body]

[optional footer]
```

**Types**:
- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation changes
- `test`: Test changes
- `refactor`: Code refactoring
- `perf`: Performance improvements
- `chore`: Build/tooling changes

**Examples**:
```
feat(download): add checksum validation
fix(database): handle concurrent inserts
docs(api): update FSM library reference
test(extraction): add path traversal tests
```

### Code Style

Follow [Effective Go](https://go.dev/doc/effective_go) and [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments).

**Key Guidelines**:
- Use `gofmt` for formatting
- Use meaningful variable names
- Keep functions small and focused
- Write tests for all public APIs
- Document exported functions and types
- Handle errors explicitly
- Use context for cancellation
- Avoid global state

---

## References

- [Quick Start Guide](QUICKSTART.md) - Setup and first run
- [Troubleshooting](TROUBLESHOOTING.md) - Common issues
- [System Architecture](../design/SYSTEM_ARCH.md) - Architecture overview
- [FSM Flow Design](../design/FSM_FLOWS.md) - State machine patterns
- [API Reference](../api/) - API documentation


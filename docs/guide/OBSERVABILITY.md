# Observability Guide

**Document Type**: Guide  
**Status**: Complete  
**Version**: 1.0  
**Last Updated**: 2025-11-21

---

## Overview

This guide covers the observability features of the Fly.io Container Image Management System, including structured logging, distributed tracing with OpenTelemetry, Prometheus metrics, and end-to-end request correlation.

**Goal**: Enable operators to monitor, debug, and optimize the system in production.

---

## Observability Stack

### Built-in Features

| Feature | Technology | Status | Purpose |
|---------|-----------|--------|---------|
| **Structured Logging** | Logrus (JSON) | ✅ Enabled | Request/FSM context tracking |
| **Distributed Tracing** | OpenTelemetry | ✅ Built-in (FSM library) | End-to-end request flow |
| **Metrics** | Prometheus | ✅ Built-in (FSM library) | Performance and health monitoring |
| **Trace Correlation** | SQLite trace_id fields | 📋 Schema ready | Database-to-trace linking |

---

## Structured Logging

### Log Format

All logs use **JSON format** with structured fields for easy parsing and filtering.

**Example Log Entry**:
```json
{
  "level": "info",
  "msg": "download FSM completed",
  "image_id": "img_abc123def456...",
  "s3_key": "images/alpine-3.18.tar",
  "local_path": "/var/lib/flyio/images/img_abc123...tar",
  "checksum": "sha256:def456...",
  "size_bytes": 5242880,
  "downloaded": true,
  "time": "2025-11-21T20:00:35Z"
}
```

### Contextual Fields

Each log entry includes contextual fields based on the operation:

**Download FSM**:
- `image_id` - Unique image identifier
- `s3_key` - S3 object key
- `local_path` - Downloaded file path
- `checksum` - SHA256 hash
- `size_bytes` - File size
- `transition` - Current FSM transition

**Unpack FSM**:
- `image_id` - Image identifier
- `device_id` - DeviceMapper device ID
- `device_name` - Device name (thin-xxx)
- `device_path` - Device path (/dev/mapper/xxx)
- `mount_point` - Temporary mount location
- `transition` - Current FSM transition

**Activate FSM**:
- `image_id` - Image identifier
- `snapshot_id` - Snapshot device ID
- `snapshot_name` - Human-readable snapshot name
- `device_path` - Snapshot device path
- `origin_device` - Source device for snapshot
- `transition` - Current FSM transition

### Log Levels

```go
// Debug: Detailed diagnostic information
log.WithFields(logrus.Fields{
    "image_id": imageID,
    "s3_key":   s3Key,
}).Debug("starting download")

// Info: General informational messages (default)
log.Info("download FSM completed")

// Warn: Warning messages (non-critical issues)
log.Warn("file size mismatch, will re-download")

// Error: Error messages (operation failures)
log.WithError(err).Error("failed to download image")
```

### Viewing Logs

**JSON logs** (default):
```bash
# View all logs
journalctl -u flyio-image-manager -f

# Pretty-print JSON
journalctl -u flyio-image-manager -f | jq .

# Filter by level
journalctl -u flyio-image-manager | jq 'select(.level=="error")'

# Filter by image_id
journalctl -u flyio-image-manager | jq 'select(.image_id=="img_abc123...")'

# Follow specific FSM run
journalctl -u flyio-image-manager | jq 'select(.image_id=="img_abc123..." and .time > "2025-11-21T20:00:00Z")'
```

---

## Distributed Tracing (OpenTelemetry)

### Overview

The FSM library has **built-in OpenTelemetry tracing** that automatically creates spans for:
- FSM runs (entire execution)
- Transitions (individual steps)
- State changes
- Database operations
- External calls (S3, devicemapper)

### Trace Architecture

```
Request Entry Point (CLI/API)
└── FSM Run Span (download-image)
    ├── Transition Span (check-exists)
    │   └── Database Query Span
    ├── Transition Span (download)
    │   ├── S3 Download Span
    │   └── Checksum Computation Span
    ├── Transition Span (validate)
    │   └── Security Check Span
    └── Transition Span (store-metadata)
        └── Database Insert Span
```

### Trace Context Propagation

The system propagates trace context through:

1. **Go context.Context** - Carries trace IDs through the call stack
2. **FSM Request objects** - Trace context embedded in FSM requests
3. **Logs** - Trace IDs included in structured log fields (when available)
4. **Database** (future) - Trace IDs stored with domain records

### Enabling Tracing

The FSM library's tracing is **always enabled** internally. To export traces to a collector:

**Option 1: Jaeger**
```bash
# Start Jaeger all-in-one
docker run -d --name jaeger \
  -p 6831:6831/udp \
  -p 16686:16686 \
  jaegertracing/jaeger:latest

# Configure application (environment variables)
export OTEL_EXPORTER_JAEGER_ENDPOINT=http://localhost:14268/api/traces
export OTEL_SERVICE_NAME=flyio-image-manager

# Run application
sudo ./flyio-image-manager daemon
```

**Option 2: OTLP Collector**
```bash
# Configure OTLP endpoint
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_SERVICE_NAME=flyio-image-manager

# Run application
sudo ./flyio-image-manager daemon
```

**Note**: Full trace export configuration requires adding initialization code to `main.go`. The FSM library handles span creation automatically.

### Trace Example Workflow

**Scenario**: Process an image end-to-end

```bash
# 1. Process image
sudo ./flyio-image-manager process-image --s3-key "images/test.tar"

# 2. In Jaeger UI (http://localhost:16686):
#    - Service: flyio-image-manager
#    - Operation: download-image
#    - Find the trace

# 3. Trace structure shows:
#    Root Span: download-image (5min)
#    ├─ check-exists (10ms)
#    ├─ download (4min 30s)
#    │  └─ s3.GetObject (4min 25s)
#    ├─ validate (20s)
#    │  ├─ checksum (15s)
#    │  └─ security-checks (5s)
#    └─ store-metadata (5ms)
```

### Trace Correlation with Database

**Future Enhancement**: Add trace_id fields to database tables for correlation.

**Schema Addition**:
```sql
-- Add to images table
ALTER TABLE images ADD COLUMN trace_id TEXT;
ALTER TABLE images ADD COLUMN span_id TEXT;

-- Add to unpacked_images table
ALTER TABLE unpacked_images ADD COLUMN trace_id TEXT;

-- Add to snapshots table
ALTER TABLE snapshots ADD COLUMN trace_id TEXT;

-- Query by trace
SELECT * FROM images WHERE trace_id = 'abc123...';
```

**Usage**:
```bash
# Find trace ID from logs
LOG_TRACE=$(journalctl -u flyio-image-manager | jq -r 'select(.image_id=="img_abc123...") | .trace_id' | head -1)

# Query database
sqlite3 /var/lib/flyio/images.db "SELECT * FROM images WHERE trace_id = '$LOG_TRACE';"

# View in Jaeger
open "http://localhost:16686/trace/$LOG_TRACE"
```

---

## Prometheus Metrics

### Overview

The FSM library exposes **built-in Prometheus metrics** for:
- FSM run counts (by status: success, failure, handoff)
- Transition durations (histogram)
- Transition counts (by transition name)
- Active runs (gauge)
- Queue depths (gauge)

### Metrics Endpoint

**Option 1: HTTP Server** (requires implementation in main.go)

```go
// Add to main.go
import (
    "net/http"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

func startMetricsServer() {
    http.Handle("/metrics", promhttp.Handler())
    go http.ListenAndServe(":9090", nil)
}
```

**Option 2: Unix Socket** (using FSM admin socket)

The FSM library's admin socket can expose metrics. See FSM library documentation.

### Available Metrics

#### FSM Run Metrics

```prometheus
# Total FSM runs by action and status
fsm_runs_total{action="download-image", status="success"} 142
fsm_runs_total{action="download-image", status="failure"} 3
fsm_runs_total{action="download-image", status="handoff"} 58

fsm_runs_total{action="unpack-image", status="success"} 140
fsm_runs_total{action="activate-image", status="success"} 138

# Active FSM runs (gauge)
fsm_active_runs{action="download-image"} 2
fsm_active_runs{action="unpack-image"} 1
```

#### Transition Metrics

```prometheus
# Transition durations (histogram)
fsm_transition_duration_seconds{action="download-image", transition="check-exists", quantile="0.5"} 0.008
fsm_transition_duration_seconds{action="download-image", transition="check-exists", quantile="0.99"} 0.025

fsm_transition_duration_seconds{action="download-image", transition="download", quantile="0.5"} 35.5
fsm_transition_duration_seconds{action="download-image", transition="download", quantile="0.99"} 120.0

# Transition counts
fsm_transitions_total{action="download-image", transition="check-exists"} 200
fsm_transitions_total{action="download-image", transition="download"} 145
```

#### Queue Metrics (if implemented)

```prometheus
# Queue depth (gauge)
fsm_queue_depth{queue="downloads"} 3
fsm_queue_depth{queue="unpacks"} 1

# Queue capacity
fsm_queue_capacity{queue="downloads"} 5
fsm_queue_capacity{queue="unpacks"} 2
```

### Prometheus Queries

**Dashboard Queries**:

```promql
# Success rate (last 1h)
rate(fsm_runs_total{status="success"}[1h]) / rate(fsm_runs_total[1h])

# P95 download time
histogram_quantile(0.95, rate(fsm_transition_duration_seconds_bucket{transition="download"}[5m]))

# Active downloads
fsm_active_runs{action="download-image"}

# Failure rate
rate(fsm_runs_total{status="failure"}[5m])

# Handoff rate (idempotency hit rate)
rate(fsm_runs_total{status="handoff"}[5m]) / rate(fsm_runs_total[5m])

# Average transition times
rate(fsm_transition_duration_seconds_sum[5m]) / rate(fsm_transition_duration_seconds_count[5m])
```

### Grafana Dashboard

**Example Panel Configurations**:

**Panel 1: Success Rate**
```json
{
  "title": "FSM Success Rate",
  "expr": "rate(fsm_runs_total{status=\"success\"}[5m]) / rate(fsm_runs_total[5m])",
  "legend": "{{action}}",
  "unit": "percentunit"
}
```

**Panel 2: Transition Durations**
```json
{
  "title": "P95 Transition Duration",
  "expr": "histogram_quantile(0.95, rate(fsm_transition_duration_seconds_bucket[5m]))",
  "legend": "{{action}}.{{transition}}",
  "unit": "s"
}
```

**Panel 3: Active Runs**
```json
{
  "title": "Active FSM Runs",
  "expr": "fsm_active_runs",
  "legend": "{{action}}",
  "type": "graph"
}
```

---

## End-to-End Request Correlation

### Correlation Strategy

Link requests across logs, traces, and database using common identifiers:

1. **image_id** - Primary correlation key (deterministic from S3 key)
2. **trace_id** - OpenTelemetry trace identifier
3. **run_id** - FSM run identifier
4. **timestamp** - Temporal correlation

### Correlation Workflow

**Scenario**: Debug a failed image processing request

**Step 1: Find the request in logs**
```bash
# Search by S3 key or time range
journalctl -u flyio-image-manager --since "2025-11-21 20:00:00" | \
  jq 'select(.s3_key=="images/problem.tar")'

# Extract image_id
IMAGE_ID=$(journalctl -u flyio-image-manager | \
  jq -r 'select(.s3_key=="images/problem.tar") | .image_id' | head -1)

echo "Image ID: $IMAGE_ID"
```

**Step 2: Query database for image state**
```bash
sqlite3 /var/lib/flyio/images.db <<EOF
-- Get image info
SELECT image_id, download_status, activation_status, downloaded_at
FROM images
WHERE image_id = '$IMAGE_ID';

-- Check if unpacked
SELECT device_id, device_name, layout_verified
FROM unpacked_images
WHERE image_id = '$IMAGE_ID';

-- Check snapshots
SELECT snapshot_id, active
FROM snapshots
WHERE image_id = '$IMAGE_ID';
EOF
```

**Step 3: Find all logs for this image**
```bash
# Get complete timeline
journalctl -u flyio-image-manager | \
  jq -c "select(.image_id==\"$IMAGE_ID\")" | \
  jq -s 'sort_by(.time)' > /tmp/image-timeline.json

# View timeline
jq -r '.[] | "\(.time) [\(.level)] \(.msg)"' /tmp/image-timeline.json
```

**Step 4: Identify failure point**
```bash
# Find error logs
jq -r '.[] | select(.level=="error") | "\(.time) \(.msg) \(.error)"' /tmp/image-timeline.json
```

**Step 5: Check trace (if trace export enabled)**
```bash
# Get trace_id from logs
TRACE_ID=$(jq -r '.[0].trace_id // empty' /tmp/image-timeline.json)

if [ -n "$TRACE_ID" ]; then
  echo "View trace: http://localhost:16686/trace/$TRACE_ID"
fi
```

### Example: Full Request Flow

**Request**: Process `images/alpine-3.18.tar`

**1. Entry Point (CLI)**
```bash
sudo ./flyio-image-manager process-image --s3-key "images/alpine-3.18.tar"
```

**2. Image ID Derived**
```
image_id = img_f3e4d5c6b7a89012...
```

**3. Download FSM**
```json
{"level":"info","image_id":"img_f3e4d5c6...","msg":"starting download FSM","transition":"check-exists","time":"2025-11-21T20:00:00Z"}
{"level":"info","image_id":"img_f3e4d5c6...","msg":"image not found, proceeding to download","time":"2025-11-21T20:00:01Z"}
{"level":"info","image_id":"img_f3e4d5c6...","msg":"download complete","size_bytes":5242880,"time":"2025-11-21T20:04:31Z"}
{"level":"info","image_id":"img_f3e4d5c6...","msg":"validation successful","time":"2025-11-21T20:04:51Z"}
```

**4. Database Record**
```sql
INSERT INTO images (image_id, s3_key, download_status, ...) VALUES ('img_f3e4d5c6...', 'images/alpine-3.18.tar', 'completed', ...);
```

**5. Unpack FSM**
```json
{"level":"info","image_id":"img_f3e4d5c6...","msg":"starting unpack FSM","device_id":"a1b2c3d4","time":"2025-11-21T20:04:52Z"}
{"level":"info","image_id":"img_f3e4d5c6...","msg":"device created","device_name":"thin-a1b2c3d4","time":"2025-11-21T20:04:54Z"}
```

**6. Activate FSM**
```json
{"level":"info","image_id":"img_f3e4d5c6...","msg":"snapshot created","snapshot_id":"a1b2c3d4-snap","time":"2025-11-21T20:05:15Z"}
```

**7. Query Full State**
```bash
# Timeline from logs
journalctl | jq 'select(.image_id=="img_f3e4d5c6...")' | jq -s 'sort_by(.time)'

# State from database
sqlite3 /var/lib/flyio/images.db "SELECT * FROM images WHERE image_id='img_f3e4d5c6...'"
```

---

## Monitoring Best Practices

### 1. Alert Configuration

**Critical Alerts**:
```yaml
# High failure rate
- alert: HighFSMFailureRate
  expr: rate(fsm_runs_total{status="failure"}[5m]) > 0.1
  for: 5m
  annotations:
    summary: "FSM failure rate above 10%"

# Long-running transitions
- alert: SlowTransitions
  expr: histogram_quantile(0.95, fsm_transition_duration_seconds_bucket) > 600
  for: 10m
  annotations:
    summary: "P95 transition time above 10 minutes"

# Queue saturation
- alert: QueueFull
  expr: fsm_queue_depth / fsm_queue_capacity > 0.9
  for: 5m
  annotations:
    summary: "FSM queue nearly full"
```

### 2. Log Retention

```bash
# Configure systemd journal retention
sudo mkdir -p /etc/systemd/journald.conf.d
sudo tee /etc/systemd/journald.conf.d/flyio.conf <<EOF
[Journal]
SystemMaxUse=1G
SystemMaxFileSize=100M
MaxRetentionSec=7day
EOF

sudo systemctl restart systemd-journald
```

### 3. Metrics Retention

Configure Prometheus retention:
```yaml
# prometheus.yml
global:
  scrape_interval: 15s

storage:
  tsdb:
    retention.time: 30d
    retention.size: 50GB
```

---

## Troubleshooting with Observability

### Problem: Slow image processing

**Investigation**:
```bash
# 1. Check Prometheus for slow transitions
# Query: histogram_quantile(0.95, fsm_transition_duration_seconds_bucket)

# 2. Identify slowest transition
journalctl | jq 'select(.transition) | {transition, duration_ms: (.duration // 0)}'

# 3. Check if it's network (S3) or I/O (unpacking)
# S3: Check network logs
# I/O: Check system I/O wait (iostat -x 1)
```

### Problem: High failure rate

**Investigation**:
```bash
# 1. Get failure count
journalctl | jq 'select(.level=="error")' | jq -s 'length'

# 2. Group by error type
journalctl | jq -r 'select(.level=="error") | .error' | sort | uniq -c

# 3. Check database for failed images
sqlite3 /var/lib/flyio/images.db "SELECT COUNT(*), download_status FROM images GROUP BY download_status;"
```

---

## References

- [FSM Library Documentation](../api/FSM_LIBRARY.md) - Built-in observability features
- [Usage Guide](USAGE.md) - CLI commands and configuration
- [Troubleshooting](TROUBLESHOOTING.md) - Common issues
- [OpenTelemetry Docs](https://opentelemetry.io/docs/) - Tracing standards
- [Prometheus Docs](https://prometheus.io/docs/) - Metrics collection

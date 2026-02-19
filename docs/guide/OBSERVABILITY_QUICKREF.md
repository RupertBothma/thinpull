# Observability Quick Reference

**Quick command reference for monitoring and debugging the Fly.io Container Image Management System.**

---

## 📊 Logs

```bash
# View all logs (JSON)
journalctl -u flyio-image-manager -f

# Pretty-print
journalctl -u flyio-image-manager -f | jq .

# Filter by level
journalctl -u flyio-image-manager | jq 'select(.level=="error")'

# Filter by image_id
journalctl -u flyio-image-manager | jq 'select(.image_id=="img_abc...")'

# Extract image_id from S3 key
IMAGE_ID=$(journalctl -u flyio-image-manager | \
  jq -r 'select(.s3_key=="images/test.tar") | .image_id' | head -1)
```

---

## 🔍 Tracing

```bash
# Start Jaeger
docker run -d --name jaeger -p 16686:16686 jaegertracing/jaeger:latest

# Configure app
export OTEL_SERVICE_NAME=flyio-image-manager
export OTEL_EXPORTER_JAEGER_ENDPOINT=http://localhost:14268/api/traces

# View traces
open http://localhost:16686
```

---

## 📈 Metrics

```bash
# Example Prometheus queries

# Success rate
rate(fsm_runs_total{status="success"}[5m]) / rate(fsm_runs_total[5m])

# P95 download time
histogram_quantile(0.95, rate(fsm_transition_duration_seconds_bucket{transition="download"}[5m]))

# Active downloads
fsm_active_runs{action="download-image"}

# Failure rate
rate(fsm_runs_total{status="failure"}[5m])
```

---

## 🔗 Correlation

```bash
# Find image by S3 key
IMAGE_ID=$(journalctl | jq -r 'select(.s3_key=="images/test.tar") | .image_id' | head -1)

# Query database
sqlite3 /var/lib/flyio/images.db "SELECT * FROM images WHERE image_id='$IMAGE_ID';"

# Get timeline
journalctl | jq "select(.image_id==\"$IMAGE_ID\")" | jq -s 'sort_by(.time)'

# Find errors
journalctl | jq "select(.image_id==\"$IMAGE_ID\" and .level==\"error\")"
```

---

## 🚨 Alerts

```promql
# High failure rate (>10%)
rate(fsm_runs_total{status="failure"}[5m]) > 0.1

# Slow transitions (>10min)
histogram_quantile(0.95, fsm_transition_duration_seconds_bucket) > 600

# Queue full (>90%)
fsm_queue_depth / fsm_queue_capacity > 0.9
```

---

## 🛠️ Troubleshooting

```bash
# Get error summary
journalctl -u flyio-image-manager --since today | \
  jq -r 'select(.level=="error") | .error' | \
  sort | uniq -c | sort -rn

# Check database status
sqlite3 /var/lib/flyio/images.db <<EOF
SELECT download_status, COUNT(*) FROM images GROUP BY download_status;
SELECT active, COUNT(*) FROM snapshots GROUP BY active;
EOF

# Check devicemapper
sudo dmsetup status pool
sudo dmsetup ls | grep thin
```

---

See [OBSERVABILITY.md](OBSERVABILITY.md) for complete guide.

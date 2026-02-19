#!/bin/bash
# Environment check for integration testing

echo "=== Fly.io Image Manager: Environment Check ==="
echo ""

# Check Linux
if [[ "$OSTYPE" != "linux-gnu"* ]]; then
    echo "❌ ERROR: Linux required (current: $OSTYPE)"
    echo "   macOS users: Use EC2, Docker, or Multipass VM"
    exit 1
fi
echo "✓ Linux detected"

# Check root
if [[ $EUID -ne 0 ]]; then
    echo "❌ ERROR: Root access required"
    echo "   Run with: sudo $0"
    exit 1
fi
echo "✓ Root access confirmed"

# Check Go
if ! command -v go &> /dev/null; then
    echo "❌ ERROR: Go not installed"
    echo "   Install: https://go.dev/dl/"
    exit 1
fi
GO_VERSION=$(go version | awk '{print $3}')
echo "✓ Go installed: $GO_VERSION"

# Check DeviceMapper tools
for cmd in dmsetup losetup mkfs.ext4; do
    if ! command -v $cmd &> /dev/null; then
        echo "❌ ERROR: $cmd not found"
        echo "   Install: apt-get install lvm2 (Debian/Ubuntu)"
        exit 1
    fi
    echo "✓ $cmd found"
done

# Check SQLite CLI (for debugging/verification)
if ! command -v sqlite3 &> /dev/null; then
    echo "❌ ERROR: sqlite3 not found"
    echo "   Install: apt-get install sqlite3 (Debian/Ubuntu)"
    exit 1
fi
echo "✓ sqlite3 found: $(sqlite3 --version | awk '{print $1}')"

# Check disk space
AVAILABLE=$(df /var/lib 2>/dev/null | awk 'NR==2 {print $4}' || echo "0")
if [[ $AVAILABLE -lt 10485760 ]]; then
    echo "❌ ERROR: Insufficient disk space"
    echo "   Required: 10GB+, Available: $(($AVAILABLE / 1024 / 1024))GB"
    exit 1
fi
echo "✓ Disk space: $(($AVAILABLE / 1024 / 1024))GB available"

# Check AWS credentials
if [[ -z "$AWS_ACCESS_KEY_ID" && ! -f ~/.aws/credentials ]]; then
    echo "⚠️  WARNING: AWS credentials not configured"
    echo "   Set: export AWS_ACCESS_KEY_ID=..."
    echo "   Or: aws configure"
else
    echo "✓ AWS credentials configured"
fi

echo ""
echo "✓ Environment check PASSED!"
echo "Ready to run integration tests."

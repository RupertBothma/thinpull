#!/bin/bash
# Deploy project to remote test server for integration testing

set -e

SERVER="rupert@192.168.1.21"
REMOTE_DIR="/home/rupert/flyio-test"

echo "=== Deploying to Test Server ==="
echo ""
echo "Server: $SERVER"
echo "Remote directory: $REMOTE_DIR"
echo ""

# Check SSH connectivity
echo "Testing SSH connection..."
if ssh -o ConnectTimeout=5 "$SERVER" "echo 'Connection successful'" &>/dev/null; then
    echo "✓ SSH connection successful"
else
    echo "❌ ERROR: Cannot connect to $SERVER"
    echo "   Check: ssh $SERVER"
    exit 1
fi

# Create remote directory
echo ""
echo "Creating remote directory..."
ssh "$SERVER" "mkdir -p $REMOTE_DIR"
echo "✓ Directory created: $REMOTE_DIR"

# Transfer project files
echo ""
echo "Transferring project files..."
rsync -avz --progress \
    --exclude='.git' \
    --exclude='*.db' \
    --exclude='flyio-image-manager' \
    --exclude='*.log' \
    --exclude='.DS_Store' \
    ./ "$SERVER:$REMOTE_DIR/"

echo "✓ Files transferred"

# Make scripts executable
echo ""
echo "Making scripts executable..."
ssh "$SERVER" "cd $REMOTE_DIR && chmod +x test-scripts/*.sh"
echo "✓ Scripts executable"

# Check Go installation
echo ""
echo "Checking Go installation on remote server..."
if ssh "$SERVER" "command -v go &>/dev/null"; then
    GO_VERSION=$(ssh "$SERVER" "go version")
    echo "✓ Go installed: $GO_VERSION"
else
    echo "⚠️  WARNING: Go not installed on remote server"
    echo "   Install with: sudo apt-get install golang-go"
fi

# Check AWS credentials
echo ""
echo "Checking AWS credentials on remote server..."
if ssh "$SERVER" "test -f ~/.aws/credentials || test -n \"\$AWS_ACCESS_KEY_ID\""; then
    echo "✓ AWS credentials configured"
else
    echo "⚠️  WARNING: AWS credentials not configured on remote server"
    echo "   Configure with: aws configure"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✓ Deployment complete!"
echo ""
echo "Next steps:"
echo ""
echo "  1. Connect to server:"
echo "     ssh $SERVER"
echo ""
echo "  2. Navigate to project:"
echo "     cd $REMOTE_DIR"
echo ""
echo "  3. Setup and run:"
echo "     sudo ./test-scripts/check-environment.sh"
echo "     sudo ./test-scripts/setup-test-environment.sh"
echo "     sudo ./flyio-image-manager process-image --s3-key images/node/5.tar"
echo ""
echo "  4. Cleanup (when done):"
echo "     sudo ./test-scripts/cleanup-test-environment.sh"
echo ""

#!/bin/bash
# Setup Ubuntu test server with required dependencies

set -e

echo "=== Setting up Ubuntu Test Server ==="
echo ""

# Check if running as root
if [ "$EUID" -ne 0 ]; then 
    echo "Please run with sudo: sudo ./setup-ubuntu-server.sh"
    exit 1
fi

# Update package list
echo "Updating package list..."
apt-get update -qq

# Install Go
echo ""
echo "Installing Go..."
if command -v go &>/dev/null; then
    echo "✓ Go already installed: $(go version)"
else
    apt-get install -y golang-go
    GO_VERSION=$(go version)
    echo "✓ Go installed: $GO_VERSION"
fi

# Install AWS CLI if not present
echo ""
echo "Checking AWS CLI..."
if command -v aws &>/dev/null; then
    echo "✓ AWS CLI already installed: $(aws --version)"
else
    echo "Installing AWS CLI..."
    apt-get install -y awscli
    echo "✓ AWS CLI installed: $(aws --version)"
fi

# Install devicemapper tools
echo ""
echo "Checking devicemapper tools..."
if command -v dmsetup &>/dev/null; then
    echo "✓ devicemapper tools already installed"
else
    echo "Installing lvm2 (devicemapper tools)..."
    apt-get install -y lvm2
    echo "✓ lvm2 installed"
fi

# Install other useful tools
echo ""
echo "Installing additional tools..."
apt-get install -y \
    rsync \
    curl \
    jq \
    tree

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✓ System setup complete!"
echo ""
echo "Next steps:"
echo ""
echo "  1. Configure AWS credentials (as regular user, not sudo):"
echo "     exit  # exit sudo if needed"
echo "     aws configure"
echo ""
echo "  2. Enter your AWS credentials:"
echo "     AWS Access Key ID: [your key]"
echo "     AWS Secret Access Key: [your secret]"
echo "     Default region: us-west-2"
echo "     Default output format: json"
echo ""
echo "  3. Verify AWS access:"
echo "     aws s3 ls s3://flyio-image-manager/images/"
echo ""

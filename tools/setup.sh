#!/usr/bin/env bash
set -euo pipefail

echo "🔧 Installing gotools..."
mkdir -p "$(go env GOPATH)/bin"
curl -fsSL https://raw.githubusercontent.com/piusalfred/gotools/v0.6.7/gotools.sh \
  -o "$(go env GOPATH)/bin/gotools"
chmod +x "$(go env GOPATH)/bin/gotools"

echo ""
echo "🔄 Syncing tools..."
gotools sync

echo ""
echo "✅ Done. Run 'make hooks' to install pre-commit hooks."

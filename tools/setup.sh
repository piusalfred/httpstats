#!/usr/bin/env bash
set -euo pipefail

echo "🔧 Installing gotools..."
go install github.com/piusalfred/gotools/cmd/gotools@v0.7.1

echo ""
echo "🔄 Syncing tools..."
gotools sync

echo ""
echo "✅ Done. Run 'make hooks' to install pre-commit hooks."

#!/usr/bin/env bash
set -euo pipefail

echo "🔧 Installing gotools..."
go install github.com/piusalfred/gotools/cmd/gotools@latest

echo ""
echo "🔄 Syncing tools..."
gotools sync

echo ""
echo "✅ Done. Run 'make hooks' to install pre-commit hooks."

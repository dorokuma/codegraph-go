#!/bin/bash
# codegraph-go 一键部署：编译 → 杀进程 → 替换二进制 → 重启 daemon
set -e

ROOT="$(cd "$(dirname "$0")" && pwd)"
BINARY="/usr/local/bin/codegraph-go"

echo "=== 编译 ==="
cd "$ROOT"
go build -o ./bin/codegraph-go . 2>&1
echo "BUILD OK ($(du -h ./bin/codegraph-go | cut -f1))"

echo "=== 停止旧进程 ==="
PID=$(cat /root/.codegraph/daemon.pid 2>/dev/null | grep -o '"pid":[0-9]*' | grep -o '[0-9]*')
if [ -n "$PID" ]; then
  kill "$PID" 2>/dev/null && echo "killed daemon pid $PID" || echo "daemon already stopped"
  sleep 1
fi
rm -f /root/.codegraph/daemon.pid /root/.codegraph/daemon.sock 2>/dev/null

echo "=== 替换二进制 ==="
install -m 755 ./bin/codegraph-go "$BINARY"
echo "DEPLOYED → $BINARY"

echo "=== 验证 ==="
"$BINARY" version 2>&1 | head -1

echo "=== 提交推送 ==="
git add -A
if git diff --cached --quiet; then
  echo "无改动，跳过提交"
else
  VERSION=$(grep 'PackageVersion' daemon/paths.go | grep -o '"[^"]*"' | tr -d '"')
  git commit -m "v${VERSION}" 2>&1
  git push 2>&1
  echo "PUSHED v${VERSION}"
fi

echo "=== 完成 ==="

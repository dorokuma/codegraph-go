#!/bin/bash
# codegraph-go 一键部署：编译 → 杀进程 → 替换二进制 → 重启 daemon
set -e

ROOT="$(cd "$(dirname "$0")" && pwd)"
BINARY="${BINARY:-$HOME/.local/bin/codegraph-go}"
CODEGRAPH_HOME="${CODEGRAPH_HOME:-$HOME/.codegraph}"

echo "=== 编译 ==="
cd "$ROOT"
go build -o ./bin/codegraph-go . 2>&1
echo "BUILD OK ($(du -h ./bin/codegraph-go | cut -f1))"

echo "=== 停止旧进程 ==="
PID=$(cat "$CODEGRAPH_HOME/daemon.pid" 2>/dev/null | grep -o '"pid":[0-9]*' | grep -o '[0-9]*')
if [ -n "$PID" ]; then
  if [ -r /proc/$PID/cmdline ] && tr '\0' ' ' </proc/$PID/cmdline | grep -q codegraph; then
    kill "$PID" 2>/dev/null && echo "killed daemon pid $PID" || echo "daemon already stopped"
  else
    echo "pid $PID does not belong to codegraph, skipping kill"
  fi
  sleep 1
fi
rm -f "$CODEGRAPH_HOME/daemon.pid" "$CODEGRAPH_HOME/daemon.sock" 2>/dev/null

echo "=== 替换二进制 ==="
if [ -f "$BINARY" ]; then
  install -m 755 ./bin/codegraph-go "$BINARY.new" && \
  mv "$BINARY" "$BINARY.old" && \
  mv "$BINARY.new" "$BINARY" || { mv "$BINARY.old" "$BINARY" 2>/dev/null; echo "DEPLOY FAILED: rolled back"; exit 1; }
  rm -f "$BINARY.old"
else
  install -m 755 ./bin/codegraph-go "$BINARY"
fi
echo "DEPLOYED → $BINARY"
rm -rf ./bin
echo "cleaned build output ./bin"

echo "=== 验证 ==="
"$BINARY" version 2>&1 | head -1

echo "=== 提交 ==="
git add deploy.sh daemon/paths.go
if git diff --cached --quiet; then
  echo "无改动，跳过提交"
else
  VERSION=$(grep 'PackageVersion' daemon/paths.go | grep -o '"[^"]*"' | tr -d '"')
  git commit -m "v${VERSION}" 2>&1
  echo "COMMITTED v${VERSION} — push manually with: git push"
fi

echo "=== 完成 ==="

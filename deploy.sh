#!/bin/bash
# codegraph-go 一键部署：编译 → 杀进程 → 替换二进制 → 重启 daemon
set -e

ROOT="$(cd "$(dirname "$0")" && pwd)"
BINARY="${BINARY:-$HOME/.local/bin/codegraph-go}"
CODEGRAPH_HOME="${CODEGRAPH_HOME:-$HOME/.codegraph}"

echo "=== 编译 ==="
cd "$ROOT"
go build -o ./bin/codegraph-go ./cmd/codegraph-go 2>&1
echo "BUILD OK ($(du -h ./bin/codegraph-go | cut -f1))"

echo "=== 停止旧进程 ==="
PID=$(cat "$CODEGRAPH_HOME/daemon.pid" 2>/dev/null | grep -oE '"pid"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+' | head -1)
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

echo "=== 升级后强制重启 daemon ==="
# The daemon writes its pidfile at <root>/.codegraph/daemon.pid (per project
# root), not $CODEGRAPH_HOME. Probe the repo-root pidfile first, then the
# legacy location; take the first that exists. No pidfile → nothing to do
# (idempotent).
DAEMON_PIDFILE=""
for cand in "$ROOT/.codegraph/daemon.pid" "$CODEGRAPH_HOME/daemon.pid"; do
  if [ -f "$cand" ]; then DAEMON_PIDFILE="$cand"; break; fi
done
# Workdir for the restart: the pidfile always sits at
# <workdir>/.codegraph/daemon.pid, so the workdir is exactly two levels up
# from the pidfile — including $HOME/.codegraph/daemon.pid, which is the
# regular location for workdir=$HOME (not a legacy layout, so no special
# case). No pidfile → fall back to the repo root (the deployment target).
WORKDIR=""
if [ -n "$DAEMON_PIDFILE" ]; then
  WORKDIR="$(cd "$(dirname "$(dirname "$DAEMON_PIDFILE")")" && pwd)"
else
  WORKDIR="$ROOT"
fi
KILLED_PID=""
if [ -n "$DAEMON_PIDFILE" ]; then
  PID=$(grep -oE '"pid"[[:space:]]*:[[:space:]]*[0-9]+' "$DAEMON_PIDFILE" | grep -oE '[0-9]+' | head -1)
  if [ -n "$PID" ]; then
    if [ -r /proc/$PID/cmdline ] && tr '\0' ' ' </proc/$PID/cmdline | grep -q codegraph; then
      echo "restarting daemon pid $PID (SIGTERM)"
      kill "$PID" 2>/dev/null || true
      # Graceful exit window (daemon Stop drains sessions, checkpoints WAL):
      # wait up to 5s, then SIGKILL so a half-dead daemon (stuck Stop) cannot
      # survive the upgrade and keep holding the DB.
      waited=0
      while kill -0 "$PID" 2>/dev/null && [ "$waited" -lt 25 ]; do
        sleep 0.2
        waited=$((waited + 1))
      done
      if kill -0 "$PID" 2>/dev/null; then
        echo "daemon pid $PID still alive after 5s — SIGKILL"
        kill -9 "$PID" 2>/dev/null || true
      else
        echo "daemon pid $PID exited"
      fi
      KILLED_PID="$PID"
    else
      echo "pid $PID does not belong to codegraph, skipping kill"
    fi
  fi
fi
# Clean residual artifacts so the fresh daemon binds cleanly.
rm -f "$ROOT/.codegraph/daemon.pid" "$ROOT/.codegraph/daemon.sock" \
      "$CODEGRAPH_HOME/daemon.pid" "$CODEGRAPH_HOME/daemon.sock" 2>/dev/null || true
# Note: this glob is not project-scoped — it also removes the tmp-fallback
# sockets of OTHER projects on this machine. Their daemons are unaffected
# (the pidfiles still hold the locks); the only cost is one extra spawn
# probe on their next dial. Accepted for a deploy script.
rm -f /tmp/codegraph-go-*.sock 2>/dev/null || true
# Drop daemons/ registry records for this project root or the killed daemon
# (leave other projects' records intact).
REG_DIR="${CODEGRAPH_HOME:-$HOME/.codegraph}/daemons"
if [ -d "$REG_DIR" ]; then
  for f in "$REG_DIR"/*.json; do
    [ -e "$f" ] || continue
    if [ -n "$KILLED_PID" ] && grep -Fq "\"pid\": $KILLED_PID" "$f" 2>/dev/null; then
      rm -f "$f"
    elif [ -n "$WORKDIR" ] && grep -Fq "\"root\": \"$WORKDIR\"" "$f" 2>/dev/null; then
      rm -f "$f"
    fi
  done
fi
# Pre-warm: spawn the new daemon detached (SpawnDetached equivalent:
# -workdir + CODEGRAPH_DAEMON_INTERNAL=1, output to .codegraph/daemon.log) so
# the next client connects immediately instead of paying spawn + index time.
mkdir -p "$WORKDIR/.codegraph"
CODEGRAPH_DAEMON_INTERNAL=1 nohup "$BINARY" -workdir "$WORKDIR" >>"$WORKDIR/.codegraph/daemon.log" 2>&1 &
SPAWN_PID=$!
# Verify the warm-up actually took the lock; a half-dead old daemon still
# holding the DB would make the new daemon exit immediately.
ready=0
for i in $(seq 1 15); do
  if [ -f "$WORKDIR/.codegraph/daemon.pid" ]; then
    ready=1
    break
  fi
  if ! kill -0 "$SPAWN_PID" 2>/dev/null; then
    break
  fi
  sleep 0.2
done
if [ "$ready" = "1" ]; then
  NEWPID=$(grep -oE '"pid"[[:space:]]*:[[:space:]]*[0-9]+' "$WORKDIR/.codegraph/daemon.pid" | grep -oE '[0-9]+' | head -1)
  echo "new daemon warmed up for $WORKDIR (pid $NEWPID)"
else
  echo "WARN: daemon warm-up did not take the lock — the next client will spawn it automatically"
fi

echo "=== 验证 ==="
test -x "$BINARY" && echo "binary deployed: $BINARY ($(stat -c %s "$BINARY") bytes)" || { echo "DEPLOY FAILED: binary not executable"; exit 1; }

echo "=== 提交 ==="
git add deploy.sh internal/daemon/paths.go
if git diff --cached --quiet; then
  echo "无改动，跳过提交"
else
  VERSION=$(grep 'PackageVersion' internal/daemon/paths.go | grep -o '"[^"]*"' | tr -d '"')
  if [ -z "$VERSION" ]; then
    VERSION="unknown"
    echo "warning: VERSION is empty, using 'unknown'"
  fi
  git commit -m "v${VERSION}" 2>&1 || { echo "commit failed (non-fatal)"; }
  echo "COMMITTED v${VERSION} — push manually with: git push"
fi

echo "=== 完成 ==="

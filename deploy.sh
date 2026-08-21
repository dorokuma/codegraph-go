#!/bin/bash
# codegraph-go 一键部署：编译 → 杀进程 → 替换二进制 → 重启 daemon
# M4: -u（未定义变量直接报错）+ pipefail（管道中段失败即退出）。
# 所有本来可容忍缺失的管道（cat/grep 找不到 pidfile/pid）都显式 `|| true`。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
BINARY="${BINARY:-$HOME/.local/bin/codegraph-go}"
CODEGRAPH_HOME="${CODEGRAPH_HOME:-$HOME/.codegraph}"

echo "=== 编译 ==="
cd "$ROOT"
go build -o ./bin/codegraph-go ./cmd/codegraph-go 2>&1
echo "BUILD OK ($(du -h ./bin/codegraph-go | cut -f1))"

echo "=== 停止旧进程 ==="
# Resolve the daemon workdir BEFORE any artifact handling. The pidfile
# always sits at <workdir>/.codegraph/daemon.pid, so the workdir is exactly
# two levels up from the pidfile — including $CODEGRAPH_HOME/daemon.pid,
# which is the regular location for workdir=$HOME (the HOME-workdir
# scenario). Candidates in priority order: repo-root pidfile, then legacy
# CODEGRAPH_HOME pidfile; neither → the repo root (the deployment target).
# Doing this early matters even when the stop section below gets no pidfile:
# the /proc scan later must know WORKDIR to find daemons whose pidfile/
# socket were already removed by an earlier race ("invisible flock holder"),
# so scan_daemon_pids also covers a HOME-workdir daemon on the "only HOME
# pidfile" path.
WORKDIR=""
for cand in "$ROOT/.codegraph/daemon.pid" "$CODEGRAPH_HOME/daemon.pid"; do
  if [ -f "$cand" ]; then
    WORKDIR="$(cd "$(dirname "$(dirname "$cand")")" && pwd)"
    break
  fi
done
[ -n "$WORKDIR" ] || WORKDIR="$ROOT"

# Project-scoped tmp socket for the resolved WORKDIR: mirrors the Go-side
# projectHash (sha256(filepath.Clean(root))[:16]) so only THIS project's
# tmp-fallback socket is removed — never other projects' sockets. WORKDIR is
# normalized by cd&&pwd, which matches filepath.Clean for the paths that
# occur here (absolute, no symlinks).
TMP_SOCK="/tmp/codegraph-go-$(printf '%s' "$WORKDIR" | sha256sum | cut -c1-16).sock"

# daemon_matches <pid> <wd>: FULL daemon predicate — argv0 basename ∈
# {codegraph, codegraph-go} AND cmdline carries "-workdir <wd>" (both sides
# normalized) AND environ has CODEGRAPH_DAEMON_INTERNAL=1 (daemon mode only
# — never a foreground client or direct-mode session). All three must hold;
# any unreadable /proc file is a non-match. Mirrors the Go-side
# liveInvisibleHolderMatch, which re-verifies identity immediately before
# every signal — this is the pre-signal recheck that closes the PID-reuse
# kill window.
daemon_matches() {
  local pid="$1" wd="$2" argv0 base prev="" arg wdval=""
  [ -r "/proc/$pid/cmdline" ] || return 1
  argv0=$(tr '\0' '\n' <"/proc/$pid/cmdline" 2>/dev/null | head -1)
  [ -n "$argv0" ] || return 1
  base=${argv0##*/}
  case "$base" in
    codegraph|codegraph-go) ;;
    *) return 1 ;;
  esac
  # Normalize wd (cd && pwd strips a trailing slash and resolves ".") so
  # it compares equal to the daemon's own normalized -workdir value — the
  # shell mirror of the Go-side filepath.Clean in isInvisibleHolder.
  wd="$(cd "$wd" 2>/dev/null && pwd || printf '%s' "$wd")"
  while IFS= read -r arg; do
    if [ "$prev" = "-workdir" ]; then wdval="$arg"; break; fi
    prev="$arg"
  done < <(tr '\0' '\n' <"/proc/$pid/cmdline" 2>/dev/null)
  [ -n "$wdval" ] || return 1
  wdval="$(cd "$wdval" 2>/dev/null && pwd || printf '%s' "$wdval")"
  [ "$wdval" = "$wd" ] || return 1
  # Daemon mode only: CODEGRAPH_DAEMON_INTERNAL=1 must be in environ.
  tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | grep -qx 'CODEGRAPH_DAEMON_INTERNAL=1' || return 1
  return 0
}

# Kill-state recorded at kill time, consumed by the artifact cleanup and
# the daemons-registry cleanup below:
#   KILLED_PID — last daemon pid this deploy killed (registry record match);
#   KILLED_WDS  — every workdir whose daemon this deploy actually killed.
# Artifact cleanup is scoped to exactly KILLED_WDS: never an unconditional
# rm across both $ROOT/.codegraph and $CODEGRAPH_HOME — that deleted the
# pidfile/socket of a LIVE daemon of an unrelated workdir when a client
# respawned it concurrently during the deploy (the invisible-holder wedge,
# see internal/daemon/recovery.go).
KILLED_PID=""
KILLED_WDS=""

# Legacy known-pid stop: SIGTERM the pid recorded in the CODEGRAPH_HOME
# pidfile, then a short window for it to exit. The pid is verified against
# the FULL daemon predicate (daemon_matches) with the workdir implied by
# that pidfile's location before the signal — same PID-reuse guard as the
# main kill loop below. NO rm here — deleting the pidfile/socket of a
# daemon that is still running is exactly what creates the invisible flock
# holder (the daemon keeps the DB flock while becoming unreachable through
# every pidfile and socket). Artifacts are only removed below, after the
# flock is confirmed released and no daemon-mode process for WORKDIR
# remains.
PID=$(cat "$CODEGRAPH_HOME/daemon.pid" 2>/dev/null | grep -oE '"pid"[[:space:]]*:[[:space:]]*[0-9]+' | grep -oE '[0-9]+' | head -1 || true)
if [ -n "$PID" ]; then
  LEGACY_WD="$(cd "$(dirname "$(dirname "$CODEGRAPH_HOME/daemon.pid")")" && pwd 2>/dev/null)"
  if daemon_matches "$PID" "$LEGACY_WD"; then
    if kill "$PID" 2>/dev/null; then
      echo "killed daemon pid $PID"
      # Record the workdir of the daemon this deploy killed; the artifact
      # cleanup below removes pidfile/socket for exactly these workdirs.
      KILLED_WDS="$KILLED_WDS $LEGACY_WD"
    else
      echo "daemon already stopped"
    fi
  else
    echo "pid $PID does not match daemon predicate, skipping kill"
  fi
  sleep 1
fi

# scan_daemon_pids <workdir>: print pids of daemon-mode codegraph processes
# whose -workdir value equals the argument (exact match). Reuses
# daemon_matches — no duplicated predicate logic.
scan_daemon_pids() {
  local wd="$1" p pid
  for p in /proc/[0-9]*; do
    pid=${p#/proc/}
    if daemon_matches "$pid" "$wd"; then
      echo "$pid"
    fi
  done
  true
}

echo "=== 替换二进制 ==="
# M6: install 目标父目录（~/.local/bin）在新机器上可能不存在，先建好。
mkdir -p "$(dirname "$BINARY")"
if [ -f "$BINARY" ]; then
  if install -m 755 ./bin/codegraph-go "$BINARY.new" && \
     mv "$BINARY" "$BINARY.old" && \
     mv "$BINARY.new" "$BINARY"; then
    rm -f "$BINARY.old"
  else
    # 失败回滚：只在确实已经把旧二进制挪走、且新二进制没装上时恢复 .old；
    # 上次崩溃残留的旧 .old 绝不能被当成回滚源覆盖新二进制。
    if [ -f "$BINARY.old" ] && [ ! -f "$BINARY" ]; then
      mv "$BINARY.old" "$BINARY" 2>/dev/null || true
    fi
    rm -f "$BINARY.new" 2>/dev/null || true
    echo "DEPLOY FAILED: rolled back" >&2
    exit 1
  fi
else
  if ! install -m 755 ./bin/codegraph-go "$BINARY"; then
    echo "DEPLOY FAILED: could not install $BINARY" >&2
    exit 1
  fi
fi
echo "DEPLOYED → $BINARY"
# rm -rf 保护：只允许删除本次构建产物目录。仓库若被检到 / 下，`rm -rf ./bin`
# 会变成 `rm -rf /bin`；符号链接的 bin 也绝不能跟随删除。
if [ "$ROOT" = "/" ] || [ -L "$ROOT/bin" ] || [ ! -d "$ROOT/bin" ]; then
  echo "WARN: skipping ./bin cleanup ($ROOT/bin is not a plain build dir)"
else
  rm -rf -- "$ROOT/bin"
  echo "cleaned build output $ROOT/bin"
fi

echo "=== 升级后强制重启 daemon ==="
# WORKDIR was resolved in the stop section above (first existing pidfile
# among the two candidate locations, else the repo root). It drives the
# kill scan, the flock wait, the artifact cleanup, and the pre-warm spawn.
# Collect every daemon-mode codegraph process targeting WORKDIR, not just
# the pidfile-recorded one: a deploy race can leave a live daemon whose
# pidfile and socket dentry were already removed ("invisible flock holder")
# — the pidfile alone would miss it, and an unconditional rm would then
# delete the artifacts of a process that still owns the DB flock.
# (a) every pidfile's recorded pid (both candidate locations), each paired
#     with the workdir implied by ITS OWN pidfile location — a HOME-workdir
#     daemon recorded in $CODEGRAPH_HOME/daemon.pid must be verified against
#     $HOME, not the repo root;
# (b) /proc scan: codegraph binary + -workdir $WORKDIR +
#     CODEGRAPH_DAEMON_INTERNAL=1 (daemon mode only — never a foreground
#     client or direct-mode session). A running daemon keeps its ORIGINAL
#     argv[0] even after deploy renames its binary on disk to .old: the
#     basename is still "codegraph-go", so the scan matches it by name —
#     not by any ".old" suffix in the path.
# Each entry is "<pid>:<expected-workdir>"; daemon_matches verifies against
# that workdir before ANY signal.
PIDS=""
for pf in "$ROOT/.codegraph/daemon.pid" "$CODEGRAPH_HOME/daemon.pid"; do
  [ -f "$pf" ] || continue
  PID=$(grep -oE '"pid"[[:space:]]*:[[:space:]]*[0-9]+' "$pf" | grep -oE '[0-9]+' | head -1 || true)
  if [ -n "$PID" ]; then
    PFWD="$(cd "$(dirname "$(dirname "$pf")")" && pwd)"
    if ! echo "$PIDS" | grep -qw "$PID"; then PIDS="$PIDS $PID:$PFWD"; fi
  fi
done

for PID in $(scan_daemon_pids "$WORKDIR"); do
  if ! echo "$PIDS" | grep -qw "$PID"; then PIDS="$PIDS $PID:$WORKDIR"; fi
done

for entry in $PIDS; do
  PID="${entry%%:*}"
  PIDWD="${entry#*:}"
  if [ ! -r "/proc/$PID/cmdline" ]; then
    echo "pid $PID already gone, skipping"
    continue
  fi
  # Full predicate recheck before SIGTERM (never signal a pid that is not
  # provably our daemon for this workdir — PID reuse guard).
  if daemon_matches "$PID" "$PIDWD"; then
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
      # Full predicate recheck again before the SIGKILL escalation: the pid
      # may have exited and been reused by an unrelated process during the
      # wait window. Never signal a process that no longer matches.
      if daemon_matches "$PID" "$PIDWD"; then
        echo "daemon pid $PID still alive after 5s — SIGKILL"
        kill -9 "$PID" 2>/dev/null || true
      else
        echo "pid $PID no longer matches daemon predicate before SIGKILL — not killing"
      fi
    else
      echo "daemon pid $PID exited"
    fi
    KILLED_PID="$PID"
    # Record the workdir of this killed daemon — the artifact cleanup below
    # is scoped to exactly these workdirs, never an unconditional rm of
    # other locations (see cleanup block).
    KILLED_WDS="$KILLED_WDS $PIDWD"
  else
    echo "pid $PID does not match daemon predicate (expected workdir $PIDWD), skipping kill"
  fi
done

# Only now, after every target is dead, wait for the DB flock to be
# released and confirm no daemon-mode process for WORKDIR remains BEFORE
# removing artifacts. The rm here used to be the trigger of the
# invisible-holder wedge: deleting the pidfile/socket of a daemon that is
# still running (mid-spawn, or not yet dead) makes it unreachable forever
# while it keeps holding the flock.
mkdir -p "$WORKDIR/.codegraph"
[ -f "$WORKDIR/.codegraph/codegraph.lock" ] || : >"$WORKDIR/.codegraph/codegraph.lock"
flock_free=0
for i in $(seq 1 50); do
  if flock -n "$WORKDIR/.codegraph/codegraph.lock" true 2>/dev/null; then
    flock_free=1
    break
  fi
  sleep 0.2
done
REMAINING=$(scan_daemon_pids "$WORKDIR")
if [ -z "$REMAINING" ]; then
  if [ "$flock_free" = "1" ]; then
    echo "daemon flock released"
  else
    echo "WARN: flock still held after 10s (by a non-daemon process); removing killed daemons' artifacts anyway — no daemon processes remain"
  fi
  # Artifact cleanup is scoped to the workdirs of the daemons THIS deploy
  # actually killed (KILLED_WDS, recorded at kill time) — never the old
  # unconditional rm of both $ROOT/.codegraph and $CODEGRAPH_HOME, which
  # deleted the pidfile/socket of a LIVE daemon of an unrelated workdir
  # when a client respawned it concurrently during the deploy (the
  # invisible-holder wedge). A workdir whose daemon was not killed keeps
  # its files. A dead daemon's leftover files are stale and self-heal on
  # the next spawn (RunAsDaemon: ClearStaleLock + socket rebind), and the
  # pre-warm below writes fresh ones for WORKDIR.
  for wd in $KILLED_WDS; do
    rm -f "$wd/.codegraph/daemon.pid" "$wd/.codegraph/daemon.sock" 2>/dev/null || true
  done
  # Project-scoped tmp socket cleanup: only THIS project's tmp-fallback
  # socket is removed (TMP_SOCK mirrors the Go-side projectHash for WORKDIR,
  # sha256(Clean(root))[:16]). Other projects' tmp sockets are left intact.
  rm -f "$TMP_SOCK" 2>/dev/null || true
else
  echo "WARN: daemon processes still present for $WORKDIR ($REMAINING) — keeping pidfile/socket so client-side stale-daemon cleanup handles them"
fi
# Drop daemons/ registry records for this project root or the killed daemon
# (leave other projects' records intact).
REG_DIR="${CODEGRAPH_HOME:-$HOME/.codegraph}/daemons"
if [ -d "$REG_DIR" ]; then
  if ! command -v python3 >/dev/null 2>&1; then
    echo "DEPLOY FAILED: python3 is required for registry cleanup" >&2
    exit 1
  fi
  python3 "$ROOT/scripts/cleanup_daemon_registry.py" "$REG_DIR" "${KILLED_PID:-}" "${WORKDIR:-}"
fi
# Pre-warm: spawn the new daemon detached — the shell equivalent of Go-side
# SpawnDetached (internal/daemon/spawn.go): setsid puts the daemon in its own
# session (SysProcAttr{Setsid:true}), stdin is /dev/null, stdout/stderr go to
# .codegraph/daemon.log, and CODEGRAPH_DAEMON_INTERNAL=1 is the daemon-mode
# entry SpawnDetached uses. nohup is deliberately NOT used: it only ignores
# SIGHUP without leaving the session (no setsid), which is a remedial
# workaround, not proper daemonization. This way the next client connects
# immediately instead of paying spawn + index time.
mkdir -p "$WORKDIR/.codegraph"
# M5: 预热参数与 Go 侧 SpawnDetached（internal/daemon/spawn.go）对齐：
# -workdir <root> + 可选 -config。config 文件按与 Go ConfigPath 相同的优先级
# 解析（$CODEGRAPH_CONFIG > ./codegraph-config.yaml > ~/.config/codegraph/
# config.yaml），显式传入后 daemon 的完整 workdirs 列表不再依赖其 cwd（Go
# spawn 的 daemon cwd 是 workdir 本身，裸 ./codegraph-config.yaml 查找会静默
# 指向别的文件）。CODEGRAPH_CONFIG 本身也会经环境变量继承给 daemon。
# $CODEGRAPH_CONFIG 指向不存在的文件时跳过（与 Go 端 ConfigPath 的 L1 行为
# 对齐），继续默认候选查找——绝不把死路径传进 -config（否则预热 daemon 的
# workdirs 列表与普通 client 的解析结果不一致）。
# 故意不传 -no-sync：部署要的是默认同步 daemon，与普通 client 的 SpawnOpts 一致。
DEPLOY_CFG=""
if [ -n "${CODEGRAPH_CONFIG:-}" ] && [ -f "$CODEGRAPH_CONFIG" ]; then
  DEPLOY_CFG="$CODEGRAPH_CONFIG"
elif [ -f "$ROOT/codegraph-config.yaml" ]; then
  DEPLOY_CFG="$ROOT/codegraph-config.yaml"
elif [ -f "$HOME/.config/codegraph/config.yaml" ]; then
  DEPLOY_CFG="$HOME/.config/codegraph/config.yaml"
fi
if [ -n "$DEPLOY_CFG" ]; then
  CODEGRAPH_DAEMON_INTERNAL=1 setsid "$BINARY" -workdir "$WORKDIR" -config "$DEPLOY_CFG" </dev/null >>"$WORKDIR/.codegraph/daemon.log" 2>&1 &
else
  CODEGRAPH_DAEMON_INTERNAL=1 setsid "$BINARY" -workdir "$WORKDIR" </dev/null >>"$WORKDIR/.codegraph/daemon.log" 2>&1 &
fi
SPAWN_PID=$!
# Verify the warm-up actually took the lock AND bound the socket AND is
# alive: the pidfile is written before the socket bind, so a pidfile alone
# does not mean the daemon is listening yet. Four conditions together mean
# ready — pidfile + socket + DB flock held by the new daemon (flock -n
# probe fails = held) + the spawned process alive. A half-dead old daemon
# still holding the DB would make the new daemon exit immediately; the
# missing flock would then be caught here instead of falsely reporting
# ready. Not ready is idempotent: the next client spawns automatically.
ready=0
for i in $(seq 1 15); do
  if [ -f "$WORKDIR/.codegraph/daemon.pid" ] && [ -S "$WORKDIR/.codegraph/daemon.sock" ] \
     && ! flock -n "$WORKDIR/.codegraph/codegraph.lock" true 2>/dev/null \
     && kill -0 "$SPAWN_PID" 2>/dev/null; then
    ready=1
    break
  fi
  if ! kill -0 "$SPAWN_PID" 2>/dev/null; then
    break
  fi
  sleep 0.2
done
if [ "$ready" = "1" ]; then
  NEWPID=$(grep -oE '"pid"[[:space:]]*:[[:space:]]*[0-9]+' "$WORKDIR/.codegraph/daemon.pid" | grep -oE '[0-9]+' | head -1 || true)
  echo "new daemon warmed up for $WORKDIR (pid $NEWPID)"
else
  echo "WARN: daemon warm-up did not take the lock — the next client will spawn it automatically"
fi

echo "=== 验证 ==="
test -x "$BINARY" && echo "binary deployed: $BINARY ($(stat -c %s "$BINARY") bytes)" || { echo "DEPLOY FAILED: binary not executable"; exit 1; }

echo "=== 提交 ==="
# Automatic commit is OPT-IN (DEPLOY_COMMIT=1) — a deploy must never
# surprise-commit the working tree. Default: report that changes are
# uncommitted and let the operator commit/push explicitly.
if [ "${DEPLOY_COMMIT:-0}" = "1" ]; then
  git add deploy.sh internal/daemon/paths.go
  if git diff --cached --quiet; then
    echo "无改动，跳过提交"
  else
    VERSION=$(grep 'PackageVersion' internal/daemon/paths.go | grep -o '"[^"]*"' | tr -d '"' || true)
    if [ -z "$VERSION" ]; then
      VERSION="unknown"
      echo "warning: VERSION is empty, using 'unknown'"
    fi
    git commit -m "v${VERSION}" 2>&1 || { echo "commit failed (non-fatal)"; }
    echo "COMMITTED v${VERSION} — push manually with: git push"
  fi
else
  echo "改动未提交，如需自动提交设 DEPLOY_COMMIT=1"
fi

echo "=== 完成 ==="

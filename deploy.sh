#!/bin/bash
# codegraph-go 一键部署：编译 → 替换二进制 → 杀进程 → 重启 daemon
# M4: -u（未定义变量直接报错）+ pipefail（管道中段失败即退出）。
# 所有本来可容忍缺失的管道（cat/grep 找不到 pidfile/pid）都显式 `|| true`。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
BINARY="${BINARY:-$HOME/.local/bin/codegraph-go}"
CODEGRAPH_HOME="${CODEGRAPH_HOME:-$HOME/.codegraph}"

cd "$ROOT"

# 前置门禁：默认仅允许在干净的 main 分支执行部署，避免把未合入分支或未审改动直接盖进生产二进制与 Pi 扩展目录。
CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"
if [ "$CURRENT_BRANCH" != "main" ] && [ "${DEPLOY_ALLOW_DIRTY:-0}" != "1" ]; then
  echo "FAILED: deploy.sh requires branch 'main' (current: $CURRENT_BRANCH); set DEPLOY_ALLOW_DIRTY=1 to override" >&2
  exit 1
fi
DIRTY_STATUS="$(git status --porcelain)"
if [ -n "$DIRTY_STATUS" ] && [ "${DEPLOY_ALLOW_DIRTY:-0}" != "1" ] && [ "${DEPLOY_COMMIT:-0}" != "1" ]; then
  echo "FAILED: working tree is dirty; commit/stash/clean changes before deploying, or set DEPLOY_ALLOW_DIRTY=1 (or DEPLOY_COMMIT=1) to override" >&2
  exit 1
fi

echo "=== 编译 ==="
go build -o ./bin/codegraph-go ./cmd/codegraph-go 2>&1
echo "BUILD OK ($(du -h ./bin/codegraph-go | cut -f1))"

# Install the new binary BEFORE any kill: a client that respawns a daemon in
# the kill window must exec the NEW version. Installing first also means an
# install failure aborts before any daemon is disturbed.
echo "=== 替换二进制 ==="
# M6: install 目标父目录（~/.local/bin）在新机器上可能不存在，先建好。
mkdir -p "$(dirname "$BINARY")"
# 不可变锁（对齐 dispatch-agent / sbx 的部署规范）：正式安装的二进制是 0555
# 只读可执行 + chattr +i 不可变位，运行期任何一条 `cat > ~/.local/bin/...`
# 之类的误写都会在写入那一刻就被 EPERM 拒掉，而不是悄悄污染生产二进制。
#   - staged 新二进制一律 install -m 0555；
#   - CODEGRAPH_SKIP_IMMUTABLE_LOCK=1：测试环境（二进制在 /tmp 下）跳过 chattr，
#     否则只读目录里的清理会 EPERM；
#   - chattr +i 只在 root 且有 chattr 时执行（非 root 或无 e2fsprogs 时只保留
#     0555）；文件系统不支持不可变位（非 ext*，如 btrfs/overlayfs/tmpfs）时
#     chattr 会失败，这里同样只降级为 0555 并记一行，不算部署失败。
# 替换过程本身要求「先备、后解锁、再单次原子换、立刻上锁」：旧版实现先
# `mv $BINARY $BINARY.old` 再 `mv $BINARY.new $BINARY`——两次 rename 之间存在
# 正式路径完全缺失的空窗（此时任何 client 拉起的 daemon 都会 ENOENT），且中途
# 异常退出会留下一份无锁甚至不存在的二进制。现在旧文件经 `cp -pf` 备份到
# $BINARY.old、新文件经 `install -m 0555` 预置到 $BINARY.new，解锁 $BINARY 后只
# 需一次 `mv -f`（POSIX rename(2) 单步覆盖目录项，正式路径零毫秒空窗）；中途
# 异常退出由 EXIT trap 恢复「存在 + 0555 + 不可变」。
binary_locked=0
protect_binary=0
# relock_binary — EXIT trap 处理：部署中途异常退出（set -e / pipefail 触发的
# 非零退出同样走 EXIT）时兜底：清掉未换入的 staged 文件；若正式路径缺失而
# .old 备份尚在则先恢复，再对任何在位的 $BINARY 强制「0555 + 不可变」，
# 不允许留下一个可写、无锁或干脆缺失的生产二进制。
relock_binary() {
  rm -f "$BINARY.new" 2>/dev/null || true
  [ "$protect_binary" = "1" ] || return 0
  if [ ! -e "$BINARY" ] && [ -f "$BINARY.old" ]; then
    mv -f "$BINARY.old" "$BINARY" 2>/dev/null || true
  fi
  # $BINARY 仍在原位而 .old 备份尚未清理（例如 cp -pf + unlock_binary 之后、
  # mv -f "$BINARY.new" "$BINARY" 之前异常退出）：先解锁再删除，绝不遗留
  # 一个可能带不可变位的 .old 文件。
  if [ -e "$BINARY.old" ]; then
    unlock_binary "$BINARY.old" 2>/dev/null || true
    rm -f "$BINARY.old" 2>/dev/null || true
  fi
  [ -e "$BINARY" ] || return 0
  chmod 0555 "$BINARY" 2>/dev/null || true
  if [ "${CODEGRAPH_SKIP_IMMUTABLE_LOCK:-0}" != "1" ] && [ "$(id -u)" = "0" ] && command -v chattr >/dev/null 2>&1; then
    chattr +i "$BINARY" 2>/dev/null || true
  fi
}
trap relock_binary EXIT
# unlock_binary — mv 只能替换目录项，不可变位不阻碍换名；但目标已存在时显式
# 解锁一次是更安全的做法，也顺带满足「原子 mv 前先 chattr -i」的规范。
unlock_binary() {
  [ "${CODEGRAPH_SKIP_IMMUTABLE_LOCK:-0}" = "1" ] && return 0
  [ "$(id -u)" = "0" ] || return 0
  command -v chattr >/dev/null 2>&1 || return 0
  [ -e "$1" ] || return 0
  chattr -i "$1" 2>/dev/null || true
  return 0
}
# lock_binary — 上不可变位；非 root / 无 chattr / 文件系统不支持（btrfs、
# overlayfs、tmpfs 等）时降级为纯 0555 并记一行，不算部署失败。
lock_binary() {
  if [ "${CODEGRAPH_SKIP_IMMUTABLE_LOCK:-0}" = "1" ]; then
    echo "immutable lock: skipped (CODEGRAPH_SKIP_IMMUTABLE_LOCK=1)"
    return 0
  fi
  if [ "$(id -u)" != "0" ]; then
    echo "immutable lock: skipped (not root; 0555 only)"
    return 0
  fi
  if ! command -v chattr >/dev/null 2>&1; then
    echo "immutable lock: skipped (chattr not installed; 0555 only)"
    return 0
  fi
  if chattr +i "$1" 2>/dev/null; then
    binary_locked=1
    echo "immutable lock: chattr +i applied"
  else
    echo "immutable lock: chattr +i unsupported on this filesystem (0555 only)"
  fi
  return 0
}
if [ -f "$BINARY" ]; then
  # 替换分支：先备份旧文件、预置新文件（此时 $BINARY 尚未解锁、全程在位且
  # 不可变），再解锁 → 单次原子 mv → 立即上锁。任何一步失败都不会让正式
  # 路径空出来：$BINARY 直到 mv 那一刻都还是旧二进制。
  # protect_binary 在本分支最顶部就置位——连下面的预清理（解锁残留 .old）
  # 与后续 cp/install 中途收到 SIGINT/SIGTERM 时，relock_binary 同样能兜
  # 底：清掉半截 .old/.new 并确保 $BINARY 维持「0555 + 不可变」。
  # then/else 内部不再重复置位。
  protect_binary=1
  # 上一次崩溃可能留下带不可变位的 $BINARY.old——不可变文件不能 unlink，
  # 不先解锁会让下面的 rm 失败，把一次正常部署带进 set -e。
  unlock_binary "$BINARY.old"
  rm -f "$BINARY.old" 2>/dev/null || true
  if cp -pf "$BINARY" "$BINARY.old" && \
     install -m 0555 ./bin/codegraph-go "$BINARY.new"; then
    # 解锁 $BINARY：从这一刻起 $BINARY 处于「已解锁、将被换入」窗口，窗口只
    # 跨下面这一条 mv。
    unlock_binary "$BINARY"
    if mv -f "$BINARY.new" "$BINARY"; then
      lock_binary "$BINARY"
      unlock_binary "$BINARY.old"
      rm -f "$BINARY.old"
    else
      # mv 失败：$BINARY 仍在位但已解锁。新 staged 文件由 EXIT trap 清理，
      # 这里立即重上锁并删除本次备份后退出。
      lock_binary "$BINARY"
      rm -f "$BINARY.new" "$BINARY.old" 2>/dev/null || true
      echo "DEPLOY FAILED: atomic replace failed" >&2
      exit 1
    fi
  else
    # 备份或 staged install 就失败（如目标分区满）：$BINARY 从未解锁、一直
    # 在位。protect_binary 已在外层置位让 trap 重锁兜底；清理本次产生的
    # .new/.old 再退出。
    rm -f "$BINARY.new" "$BINARY.old" 2>/dev/null || true
    lock_binary "$BINARY"
    echo "DEPLOY FAILED: could not stage new binary" >&2
    exit 1
  fi
else
  # 新装分支：正式路径不存在，先把新文件以 0555 预置到 $BINARY.new，置位
  # protect_binary 后单次 mv 到位再上锁——与替换分支同一节奏，trap 语义一致。
  if install -m 0555 ./bin/codegraph-go "$BINARY.new"; then
    protect_binary=1
    if mv -f "$BINARY.new" "$BINARY"; then
      lock_binary "$BINARY"
    else
      rm -f "$BINARY.new" 2>/dev/null || true
      echo "DEPLOY FAILED: could not install $BINARY" >&2
      exit 1
    fi
  else
    rm -f "$BINARY.new" 2>/dev/null || true
    echo "DEPLOY FAILED: could not stage new binary" >&2
    exit 1
  fi
fi
if [ "$binary_locked" = "1" ]; then
  echo "DEPLOYED → $BINARY (0555, immutable)"
else
  echo "DEPLOYED → $BINARY (0555)"
fi
# rm -rf 保护：只允许删除本次构建产物目录。仓库若被检到 / 下，`rm -rf ./bin`
# 会变成 `rm -rf /bin`；符号链接的 bin 也绝不能跟随删除。
if [ "$ROOT" = "/" ] || [ -L "$ROOT/bin" ] || [ ! -d "$ROOT/bin" ]; then
  echo "WARN: skipping ./bin cleanup ($ROOT/bin is not a plain build dir)"
else
  rm -rf -- "$ROOT/bin"
  echo "cleaned build output $ROOT/bin"
fi

echo "=== 同步 Pi 扩展 ==="
# Pi 扩展（integrations/pi/codegraph-go.ts）此前只靠 README 手工 install，
# deploy.sh 只换二进制，部署副本落后仓库一个提交的事故就是这么来的（旧版
# 缺 wantsKeepCode 逃生口，把 action=node / includeCode=true 的源码正文二
# 次剥掉）。同步收进独立小脚本，与二进制/daemon 流程解耦，也可单独执行：
#   integrations/pi/install.sh
# 幂等：目标 md5 与源一致即跳过、不重写。失败不阻断部署主体（二进制与
# daemon 照常完成），仅记账——结尾 WARN 汇总段会重打，确保「部署看似成功
# 但扩展没同步」在最后一行输出里可见。需要硬失败请单独运行该脚本。
PI_EXT_SYNC_FAILED=0
if bash "$ROOT/integrations/pi/install.sh"; then
  :
else
  PI_EXT_SYNC_FAILED=1
  echo "WARN: Pi 扩展同步失败（非阻断）——可单独运行 integrations/pi/install.sh 排查" >&2
fi

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
[ -L "$WORKDIR/.codegraph" ] && { echo "refusing: $WORKDIR/.codegraph is a symlink"; exit 1; }
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
  # WORKDIR-scoped record cleanup only when no live daemon remains for
  # this workdir. If a daemon survived (client respawn race), deleting
  # its record by workdir match would leave a live daemon undiscoverable
  # until its next restart; killed-PID cleanup always runs because that
  # daemon is gone either way.
  if [ -z "$REMAINING" ]; then
    python3 "$ROOT/scripts/cleanup_daemon_registry.py" "$REG_DIR" "${KILLED_PID:-}" "${WORKDIR:-}"
  else
    python3 "$ROOT/scripts/cleanup_daemon_registry.py" "$REG_DIR" "${KILLED_PID:-}" ""
  fi
fi
# Pre-warm: bring the new daemon up so the next client connects immediately
# instead of paying spawn + index time. The spawn itself is NOT done here any
# more — deploy.sh used to be the shell twin of Go-side SpawnDetached (setsid
# + CODEGRAPH_DAEMON_INTERNAL=1 + daemon.log), and that second entry point
# quietly bypassed everything the Go spawn path layers on top (the systemd-run
# CPUQuota scope first of all). The warm-up now asks the binary itself to do
# it through the ordinary client path, see the -warm-daemon call below.
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
# M5 后续收敛：预热不再自己 `CODEGRAPH_DAEMON_INTERNAL=1 setsid "$BINARY"`。
# 那是与 Go 端 SpawnDetached 平行的第二套 daemon 启动入口，绕过了
# systemd-run --scope -p CPUQuota=<N>% 第四层硬配额（以及它的两级回退）、
# 配置解析后的 workdir 收窄与权限校验；结果是「经客户端拉起的 daemon 有
# CPUQuota，部属时预热出来的 daemon 没有」，上限形同虚设。
# 现在走 `"$BINARY" -workdir <wd> [-config <cfg>] -warm-daemon`：客户端跑完整
# 启动链（LoadConfig → NarrowExcludedWorkdirs → ValidateWorkdirs）后调
# EnsureAndDial，由 SpawnDetached 带配额地拉起 daemon，连上后挂断连接退 0。
# 失败（非零退出）不阻断部属：下面仍然做就绪探测，下一个客户端也会自己重拉。
WARM_ARGS=(-workdir "$WORKDIR")
if [ -n "$DEPLOY_CFG" ]; then
  WARM_ARGS+=(-config "$DEPLOY_CFG")
fi
WARM_ARGS+=(-warm-daemon)
if "$BINARY" "${WARM_ARGS[@]}" </dev/null >>"$WORKDIR/.codegraph/daemon.log" 2>&1; then
  echo "warm-daemon: daemon reachable for $WORKDIR"
else
  echo "WARN: warm-daemon exited non-zero — daemon not reachable yet, falling through to the readiness probe"
fi
# Verify the warm-up actually took the lock AND bound the socket AND that the
# daemon process is alive: the pidfile is written before the socket bind, so a
# pidfile alone does not mean the daemon is listening yet. All conditions
# together mean ready — pidfile + socket + DB flock held by the new daemon
# (flock -n probe fails = held) + the recorded daemon pid alive and matching
# the FULL daemon predicate (daemon_matches: our binary + -workdir $WORKDIR +
# CODEGRAPH_DAEMON_INTERNAL=1). A half-dead old daemon still holding the DB
# would make the new daemon exit immediately; the missing flock would then be
# caught here instead of falsely reporting ready. Not ready is idempotent: the
# next client spawns automatically.
# 注意：预热现在由客户端间接拉起 daemon，PID 属于那个 daemon 而不是任何子
# shell 的 $!；经 systemd-run --scope 包装时更是如此。就绪与否一律以 pidfile
# 里记录的真实 daemon PID 为准，不再与客户端命令自身的 PID 比对。
ready=0
for i in $(seq 1 15); do
  NEWPID=$(grep -oE '"pid"[[:space:]]*:[[:space:]]*[0-9]+' "$WORKDIR/.codegraph/daemon.pid" 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)
  if [ -f "$WORKDIR/.codegraph/daemon.pid" ] && [ -S "$WORKDIR/.codegraph/daemon.sock" ] \
     && ! flock -n "$WORKDIR/.codegraph/codegraph.lock" true 2>/dev/null \
     && [ -n "$NEWPID" ] && kill -0 "$NEWPID" 2>/dev/null \
     && daemon_matches "$NEWPID" "$WORKDIR"; then
    ready=1
    break
  fi
  sleep 0.2
done
if [ "$ready" = "1" ]; then
  echo "new daemon warmed up for $WORKDIR (pid $NEWPID)"
else
  # Distinguish "another live daemon is already serving" (fine — keep it,
  # its discovery record was preserved above) from a genuinely failed
  # warm-up (next client spawns automatically).
  SERVING=$(grep -oE '"pid"[[:space:]]*:[[:space:]]*[0-9]+' "$WORKDIR/.codegraph/daemon.pid" 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)
  if [ -n "$SERVING" ] && kill -0 "$SERVING" 2>/dev/null; then
    echo "INFO: live daemon already serving $WORKDIR (pid $SERVING) — warm-up spawn exited without taking over"
  else
    echo "WARN: daemon warm-up did not take the lock — the next client will spawn it automatically"
  fi
fi

echo "=== 验证 ==="
test -x "$BINARY" && echo "binary deployed: $BINARY ($(stat -c %s "$BINARY") bytes)" || { echo "DEPLOY FAILED: binary not executable"; exit 1; }

echo "=== 提交 ==="
# Automatic commit is OPT-IN (DEPLOY_COMMIT=1) — a deploy must never
# surprise-commit the working tree. Default: report that changes are
# uncommitted and let the operator commit/push explicitly.
if [ "${DEPLOY_COMMIT:-0}" = "1" ]; then
  # Release commits also touch CHANGELOG.md / README.md — include them so
  # DEPLOY_COMMIT does not produce an incomplete version commit.
  git add deploy.sh integrations/pi/install.sh integrations/pi/codegraph-go.ts internal/daemon/paths.go CHANGELOG.md README.md
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

# 扩展同步 WARN 汇总：失败不改退出码（二进制/daemon 成功路径仍为 0），但必须在
# 结尾重打一次——原事故的本质是「漂移发生时没有任何信号」，只在中段打一行
# WARN 会被后续输出冲掉、运维看完 `=== 完成 ===` 就认为无事发生。
if [ "$PI_EXT_SYNC_FAILED" = "1" ]; then
  echo "WARN: Pi 扩展未同步（见上方 WARN）——部署副本可能仍是旧版，action=node / includeCode=true 的源码正文会被旧扩展二次剥掉。请单独运行 integrations/pi/install.sh 修复，Pi 侧 /reload 或新会话生效" >&2
fi

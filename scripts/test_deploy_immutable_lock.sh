#!/bin/bash
# 用途: 验证 deploy.sh「替换二进制」段的不可变锁流程——staged 新二进制 install
#       -m 0555、替换前解锁、替换后 chattr +i，以及中途异常退出时 EXIT trap
#       把二进制恢复成「存在 + 0555 + 不可变」；并静态断言预热改走 -warm-daemon。
# 运行方式: bash scripts/test_deploy_immutable_lock.sh
# 依赖环境: bash, install, mv, chmod, stat, mktemp；chattr/lsattr 存在且文件系统
#           支持不可变位时额外验证 chattr +i，否则该条记 SKIP。
#
# 实现方式: 从 deploy.sh 里原样抽出「=== 替换二进制 === 到 # rm -rf 保护」之间的
# 那一段来跑，避免把部署逻辑在测试里重抄一遍（重抄的副本会漂移）。功能测试一律
# 传 CODEGRAPH_SKIP_IMMUTABLE_LOCK=1（二进制在 /tmp 下；正式环境才上不可变锁，
# 否则只读位会让测试清理 EPERM）；trap 的解锁/上锁行为再用真实模式跑一次，并且
# 只在文件系统确实支持不可变位时才断言结果。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DEPLOY_SCRIPT="$SCRIPT_DIR/deploy.sh"

TOTAL_TESTS=0
PASSED_TESTS=0
CLEANUP_DIRS=""

cleanup_all() {
  chmod -R u+rwX $CLEANUP_DIRS 2>/dev/null || true
  # 不可变位会挡住 rm -rf，先全部解掉
  if command -v chattr >/dev/null 2>&1; then
    for d in $CLEANUP_DIRS; do
      find "$d" -exec chattr -i {} + 2>/dev/null || true
    done
  fi
  [ -n "$CLEANUP_DIRS" ] && rm -rf $CLEANUP_DIRS 2>/dev/null || true
  rm -f "$SECTION_FILE" 2>/dev/null || true
}
trap cleanup_all EXIT

run_test() {
  TOTAL_TESTS=$((TOTAL_TESTS + 1))
  echo -n "Test $TOTAL_TESTS: $1 ... "
}
pass() { echo "OK"; PASSED_TESTS=$((PASSED_TESTS + 1)); }
fail() { echo "FAILED ($1)"; exit 1; }

# --- 抽出被测段落 ---------------------------------------------------------
SECTION_FILE="$(mktemp)"
awk '/^echo "=== 替换二进制 ==="$/{f=1} /^# rm -rf 保护/{f=0} f' "$DEPLOY_SCRIPT" >"$SECTION_FILE"
[ -s "$SECTION_FILE" ] || { echo "FAILED: could not extract the 替换二进制 section from $DEPLOY_SCRIPT"; exit 1; }

# immutable_supported <dir>: root + chattr + 文件系统接受 chattr +i
# （tmpfs / btrfs / overlayfs 会拒绝，此时相关断言记 SKIP 而不是假绿）。
immutable_supported() {
  local dir="$1"
  local probe="$dir/.probe.$$"
  [ "$(id -u)" = "0" ] || return 1
  command -v chattr >/dev/null 2>&1 || return 1
  : >"$probe" 2>/dev/null || return 1
  if chattr +i "$probe" 2>/dev/null; then
    chattr -i "$probe" 2>/dev/null || true
    rm -f "$probe" 2>/dev/null || true
    return 0
  fi
  rm -f "$probe" 2>/dev/null || true
  return 1
}

mode_of() { stat -c '%a' "$1"; }

# --- 场景 0: deploy.sh 静态检查 ------------------------------------------
echo "=== 场景 0: deploy.sh 静态检查 ==="

run_test "deploy.sh bash syntax check (bash -n)"
bash -n "$DEPLOY_SCRIPT" && pass || fail "syntax error"

run_test "staged binary is installed with mode 0555"
grep -Eq 'install -m 0555 \./bin/codegraph-go' "$DEPLOY_SCRIPT" \
  && pass || fail "no real 'install -m 0555 ./bin/codegraph-go' call in deploy.sh"

run_test "no world/group-writable 0755 install left"
grep -q 'install -m 755' "$DEPLOY_SCRIPT" && fail "'install -m 755' still present" || pass

run_test "the section registers an EXIT trap that restores the lock"
grep -q 'trap relock_binary EXIT' "$SECTION_FILE" && pass || fail "no EXIT trap inside the section"

run_test "warm-up goes through the binary's -warm-daemon entry"
grep -q -- '-warm-daemon' "$DEPLOY_SCRIPT" && pass || fail "deploy.sh never calls -warm-daemon"

run_test "the divergent setsid daemon spawn is gone"
# 只匹配真正的调用行（行首可有缩进），不匹配提到它的注释。
if grep -Eq '^[[:space:]]*CODEGRAPH_DAEMON_INTERNAL=1[[:space:]]+setsid' "$DEPLOY_SCRIPT"; then
  fail "deploy.sh still spawns the daemon itself, bypassing SpawnDetached/CPUQuota"
else
  pass
fi

# --- 场景 1: 首次安装（目标不存在）---------------------------------------
echo "=== 场景 1: 首次安装 ==="
D1="$(mktemp -d)"; CLEANUP_DIRS="$CLEANUP_DIRS $D1"
mkdir -p "$D1/bin"
printf 'v1\n' >"$D1/bin/codegraph-go"; chmod 755 "$D1/bin/codegraph-go"
B1="$D1/dest/bin/codegraph-go"

run_test "fresh install exits 0 into a not-yet-existing bindir"
if ( set -euo pipefail; cd "$D1"; BINARY="$B1" CODEGRAPH_SKIP_IMMUTABLE_LOCK=1 bash "$SECTION_FILE" >"$D1/out" 2>&1 ); then
  pass
else
  fail "section exited non-zero: $(tail -3 "$D1/out" 2>/dev/null)"
fi

run_test "fresh binary exists with mode 0555"
[ -f "$B1" ] || fail "binary missing"
[ "$(mode_of "$B1")" = "555" ] || fail "mode is $(mode_of "$B1"), want 555"
pass

# --- 场景 2: 替换已存在的只读二进制 ---------------------------------------
echo "=== 场景 2: 替换既有二进制 ==="
D2="$(mktemp -d)"; CLEANUP_DIRS="$CLEANUP_DIRS $D2"
mkdir -p "$D2/bin" "$D2/dest/bin"
printf 'v2\n' >"$D2/bin/codegraph-go"; chmod 755 "$D2/bin/codegraph-go"
B2="$D2/dest/bin/codegraph-go"
printf 'old\n' >"$B2"; chmod 555 "$B2"

run_test "replacing an existing 0555 binary exits 0"
if ( set -euo pipefail; cd "$D2"; BINARY="$B2" CODEGRAPH_SKIP_IMMUTABLE_LOCK=1 bash "$SECTION_FILE" >"$D2/out" 2>&1 ); then
  pass
else
  fail "section exited non-zero: $(tail -3 "$D2/out" 2>/dev/null)"
fi

run_test "the new content replaced the old binary"
[ "$(cat "$B2")" = "v2" ] || fail "content is $(cat "$B2"), want v2"
pass

run_test "replaced binary has mode 0555"
[ "$(mode_of "$B2")" = "555" ] || fail "mode is $(mode_of "$B2"), want 555"
pass

run_test "no .old / .new leftovers"
[ ! -e "$B2.old" ] && [ ! -e "$B2.new" ] && pass || fail "leftover swap files remain"

# --- 场景 3: 中途异常退出，EXIT trap 必须重新上锁 -------------------------
echo "=== 场景 3: 异常退出后的锁恢复（EXIT trap） ==="
D3="$(mktemp -d)"; CLEANUP_DIRS="$CLEANUP_DIRS $D3"
mkdir -p "$D3/bin" "$D3/dest/bin"
printf 'v3\n' >"$D3/bin/codegraph-go"; chmod 755 "$D3/bin/codegraph-go"
B3="$D3/dest/bin/codegraph-go"
printf 'old\n' >"$B3"; chmod 555 "$B3"
SUPPORTED=0
if immutable_supported "$D3/dest/bin"; then SUPPORTED=1; fi

# 把「替换成功」之后的第一件事换成 exit 7，模拟 mv 完成后、收尾之前的崩溃；
# EXIT trap 必须仍然把二进制恢复成 0555（+ 不可变）。注入点取 protect_binary=1
# 之后的首个 lock_binary "$BINARY" 行（替换分支与新装分支各有一次，取首个即
# 替换分支——$B3 已存在）。
CRASH_FILE="$D3/section-crash.sh"
awk '{ print } index($0, "lock_binary \"$BINARY\"") > 0 && index($0, "unlock_binary") == 0 && !done { print "exit 7"; done=1 }' "$SECTION_FILE" >"$CRASH_FILE"
grep -q 'exit 7' "$CRASH_FILE" || fail "could not inject the crash into the section"

run_test "aborted deploy keeps the injected failure exit code"
rc3=0
( set -euo pipefail; cd "$D3"; BINARY="$B3" CODEGRAPH_SKIP_IMMUTABLE_LOCK=1 bash "$CRASH_FILE" >"$D3/out" 2>&1 ) || rc3=$?
[ "$rc3" = "7" ] || fail "exit code is $rc3, want 7"
pass

run_test "EXIT trap left the new binary in place"
[ -f "$B3" ] && [ "$(cat "$B3")" = "v3" ] || fail "binary missing or not the new version"
pass

run_test "EXIT trap restored mode 0555"
[ "$(mode_of "$B3")" = "555" ] || fail "mode is $(mode_of "$B3"), want 555"
pass

run_test "no .new / .old leftovers after an aborted deploy"
[ ! -e "$B3.new" ] && [ ! -e "$B3.old" ] && pass || fail "leftover staged/backup files remain"

if [ "$SUPPORTED" = "1" ]; then
  run_test "EXIT trap restored chattr +i"
  # 真实模式（不跳过锁）再跑一次：先把锁摘掉并放开权限，验证 trap 会重新上锁。
  chattr -i "$B3" 2>/dev/null || true
  chmod 644 "$B3"
  ( set -euo pipefail; cd "$D3"; BINARY="$B3" bash "$CRASH_FILE" >"$D3/out-real" 2>&1 ) || true
  flags=$(lsattr -d "$B3" 2>/dev/null | cut -d' ' -f1 || true)
  case "$flags" in
    *i*) pass ;;
    *) fail "flags are '$flags', want the immutable bit set" ;;
  esac
else
  echo "SKIP: chattr +i restore (no chattr or no immutable-bit filesystem here)"
fi

# 解锁后、上锁前的崩溃是最关键的窗口：unlock_binary "$BINARY" 已执行、
# mv -f 尚未运行，此时 EXIT trap 必须把仍处在「已解锁、未替换」状态的
# $BINARY 恢复成「存在 + 0555 + 不可变」，且不留 .new 残留。本场景用独立
# 夹具 D3U/B3U——不能复用上方已被 crash 测试改写为 v3 的 D3/B3。
D3U="$(mktemp -d)"; CLEANUP_DIRS="$CLEANUP_DIRS $D3U"
mkdir -p "$D3U/bin" "$D3U/dest/bin"
printf 'v3\n' >"$D3U/bin/codegraph-go"; chmod 755 "$D3U/bin/codegraph-go"
B3U="$D3U/dest/bin/codegraph-go"
printf 'old\n' >"$B3U"; chmod 555 "$B3U"
CRASH_UNLOCKED="$D3U/section-crash-unlocked.sh"
awk '{ print } index($0, "unlock_binary \"$BINARY\"") > 0 && !done { print "exit 9"; done=1 }' "$SECTION_FILE" >"$CRASH_UNLOCKED"
grep -q 'exit 9' "$CRASH_UNLOCKED" || fail "could not inject the post-unlock crash into the section"

run_test "crash between unlock_binary and mv exits with the injected code"
rc9=0
( set -euo pipefail; cd "$D3U"; BINARY="$B3U" CODEGRAPH_SKIP_IMMUTABLE_LOCK=1 bash "$CRASH_UNLOCKED" >"$D3U/out-u" 2>&1 ) || rc9=$?
[ "$rc9" = "9" ] || fail "exit code is $rc9, want 9"
pass

run_test "post-unlock crash leaves the OLD binary in place"
[ -f "$B3U" ] && [ "$(cat "$B3U")" = "old" ] || fail "binary missing or not the old version"
pass

run_test "post-unlock crash restored mode 0555"
[ "$(mode_of "$B3U")" = "555" ] || fail "mode is $(mode_of "$B3U"), want 555"
pass

run_test "post-unlock crash leaves no .new / .old leftovers"
[ ! -e "$B3U.new" ] && [ ! -e "$B3U.old" ] && pass || fail "leftover staged/backup files remain"

if [ "$SUPPORTED" = "1" ]; then
  run_test "post-unlock crash keeps the pre-locked binary locked and old"
  # 模拟生产上已上不可变锁的旧二进制：pre-lock 后跑真实模式（不跳过锁），
  # unlock_binary 会摘掉 i 位，崩溃后 EXIT trap 必须把锁挂回去且内容不动。
  chattr +i "$B3U" 2>/dev/null || true
  ( set -euo pipefail; cd "$D3U"; BINARY="$B3U" bash "$CRASH_UNLOCKED" >"$D3U/out-u-real" 2>&1 ) || true
  flags=$(lsattr -d "$B3U" 2>/dev/null | cut -d' ' -f1 || true)
  case "$flags" in
    *i*) ;;
    *) fail "flags are '$flags', want the immutable bit set" ;;
  esac
  [ "$(cat "$B3U")" = "old" ] || fail "content changed: $(cat "$B3U"), want old"
  [ ! -e "$B3U.new" ] && [ ! -e "$B3U.old" ] || fail "leftover staged/backup files remain"
  pass
else
  echo "SKIP: chattr +i restore after post-unlock crash (no immutable-bit fs)"
fi

# --- 场景 4: staged install 阶段失败，原二进制重上锁 -----------------------
# install 在解锁前就失败（如目标分区满、staged 源缺失）时，原 $BINARY 仍在
# 原位且从未解锁——失败分支仍走 protect_binary=1 → trap 重锁兜底。
echo "=== 场景 4: install 失败但原二进制保持 0555(+i) ==="
D4="$(mktemp -d)"; CLEANUP_DIRS="$CLEANUP_DIRS $D4"
mkdir -p "$D4/dest/bin"            # 故意不建 $D4/bin —— install 找不到 staged 源
B4="$D4/dest/bin/codegraph-go"
printf 'old\n' >"$B4"; chmod 555 "$B4"

run_test "install-failure path exits non-zero but original binary stays 0555"
rc4=0
( set -euo pipefail; cd "$D4"; BINARY="$B4" CODEGRAPH_SKIP_IMMUTABLE_LOCK=1 bash "$SECTION_FILE" >"$D4/out" 2>&1 ) || rc4=$?
[ "$rc4" != "0" ] || fail "section unexpectedly succeeded: $(cat "$D4/out")"
[ -f "$B4" ] || fail "original binary lost"
[ "$(mode_of "$B4")" = "555" ] || fail "mode is $(mode_of "$B4"), want 555"
pass

if [ "$SUPPORTED" = "1" ] && immutable_supported "$D4/dest/bin"; then
  run_test "install-failure path restores chattr +i on the original binary"
  chattr -i "$B4" 2>/dev/null || true
  ( set -euo pipefail; cd "$D4"; BINARY="$B4" bash "$SECTION_FILE" >"$D4/out-real" 2>&1 ) || true
  flags=$(lsattr -d "$B4" 2>/dev/null | cut -d' ' -f1 || true)
  case "$flags" in
    *i*) pass ;;
    *) fail "flags are '$flags', want the immutable bit set" ;;
  esac
else
  echo "SKIP: chattr +i restore on install failure (no chattr or no immutable-bit filesystem here)"
fi

echo

echo "=== 场景 5: install.sh 原生 Go 扩展共存保护（zero-node 宿主机） ==="
# 背景：宿主机已移除 Node.js，~/.pig/agent/extensions/codegraph-go 是指向
# 原生 Go 扩展目录（含 go.mod）的软链。此时若 install.sh 把同名 .ts 拷进
# 扩展目录，pig 启动会调起不存在的 node 重打包、所有会话起不来。修复：默认
# 目标路径下检测到 $DEST%.ts 为含 go.mod 的目录时，自愈清理残留 .ts 并
# exit 0 跳过安装；显式 PI_EXT_DEST 或无原生扩展时行为不变。
# 全程使用临时 HOME，绝不触碰真实 /root/.pig。
INSTALL_SCRIPT="$SCRIPT_DIR/integrations/pi/install.sh"

run_test "install.sh bash syntax check (bash -n)"
bash -n "$INSTALL_SCRIPT" && pass || fail "syntax error"

# (a) 原生 Go 扩展软链 + 预置脏 .ts：不传 PI_EXT_DEST -> exit 0、跳过、清残留
D5="$(mktemp -d)"; CLEANUP_DIRS="$CLEANUP_DIRS $D5"
mkdir -p "$D5/home/.pig/agent/extensions" "$D5/native-src"
: >"$D5/native-src/go.mod"
ln -s "$D5/native-src" "$D5/home/.pig/agent/extensions/codegraph-go"
printf 'stale ts copy\n' >"$D5/home/.pig/agent/extensions/codegraph-go.ts"

run_test "default DEST skips install when a native Go extension is active"
rc5a=0
HOME="$D5/home" bash "$INSTALL_SCRIPT" >"$D5/out-a" 2>&1 || rc5a=$?
[ "$rc5a" = "0" ] || fail "exit code is $rc5a, want 0: $(cat "$D5/out-a")"
grep -q 'skipped: native Go extension active' "$D5/out-a" \
  || fail "missing skipped marker: $(cat "$D5/out-a")"
pass

run_test "stale .ts self-healed and native go.mod untouched"
[ ! -e "$D5/home/.pig/agent/extensions/codegraph-go.ts" ] \
  || fail "stale .ts was not removed"
[ -f "$D5/native-src/go.mod" ] || fail "native go.mod damaged"
[ -d "$D5/home/.pig/agent/extensions/codegraph-go" ] || fail "native ext dir gone"
pass

# (b) 同一夹具显式传 PI_EXT_DEST -> 照常安装
run_test "explicit PI_EXT_DEST still installs the .ts"
rc5b=0
PI_EXT_DEST="$D5/home/.pig/agent/extensions/codegraph-go.ts" \
  bash "$INSTALL_SCRIPT" >"$D5/out-b" 2>&1 || rc5b=$?
[ "$rc5b" = "0" ] || fail "exit code is $rc5b, want 0: $(cat "$D5/out-b")"
grep -q 'changed: installed' "$D5/out-b" \
  || fail "missing changed marker: $(cat "$D5/out-b")"
[ -f "$D5/home/.pig/agent/extensions/codegraph-go.ts" ] || fail ".ts not installed"
pass

# (c) 无原生扩展的新鲜 HOME 连跑两次 -> changed 然后 unchanged
D6="$(mktemp -d)"; CLEANUP_DIRS="$CLEANUP_DIRS $D6"
mkdir -p "$D6/home"

run_test "fresh HOME installs then reports unchanged on second run"
rc5c=0
HOME="$D6/home" bash "$INSTALL_SCRIPT" >"$D6/out-c1" 2>&1 || rc5c=$?
[ "$rc5c" = "0" ] || fail "first run rc=$rc5c: $(cat "$D6/out-c1")"
grep -q 'changed: installed' "$D6/out-c1" \
  || fail "first run missing changed marker: $(cat "$D6/out-c1")"
rc5d=0
HOME="$D6/home" bash "$INSTALL_SCRIPT" >"$D6/out-c2" 2>&1 || rc5d=$?
[ "$rc5d" = "0" ] || fail "second run rc=$rc5d: $(cat "$D6/out-c2")"
grep -q 'unchanged' "$D6/out-c2" \
  || fail "second run missing unchanged marker: $(cat "$D6/out-c2")"
pass

echo
echo "Results: $PASSED_TESTS/$TOTAL_TESTS passed"
[ "$PASSED_TESTS" = "$TOTAL_TESTS" ] || exit 1

#!/bin/bash
# 用途: 验证 deploy.sh 与 scripts/cleanup_daemon_registry.py 共享 registry cleanup 逻辑与安全边界
# 运行方式: ./test_deploy_registry_cleanup.sh 或 bash test_deploy_registry_cleanup.sh
# 依赖环境: bash, python3, mktemp, rm, mkdir

set -euo pipefail

# 可配置参数
TEST_TMP_BASE="${TEST_TMP_BASE:-/tmp}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DEPLOY_SCRIPT="$SCRIPT_DIR/deploy.sh"
CLEANUP_SCRIPT="$SCRIPT_DIR/scripts/cleanup_daemon_registry.py"

TOTAL_TESTS=0
PASSED_TESTS=0

run_test() {
  local desc="$1"
  TOTAL_TESTS=$((TOTAL_TESTS + 1))
  echo -n "Test $TOTAL_TESTS: $desc ... "
}

assert_file_exists() {
  local file="$1"
  if [ -f "$file" ]; then
    echo "OK"
    PASSED_TESTS=$((PASSED_TESTS + 1))
  else
    echo "FAILED (expected file to exist: $file)"
    exit 1
  fi
}

assert_file_not_exists() {
  local file="$1"
  if [ ! -f "$file" ]; then
    echo "OK"
    PASSED_TESTS=$((PASSED_TESTS + 1))
  else
    echo "FAILED (expected file NOT to exist: $file)"
    exit 1
  fi
}

# --- 场景 0: 脚本语法与静态对齐验证 ---
echo "=== 场景 0: 静态分析与共享实现对齐验证 ==="
run_test "deploy.sh bash syntax check (bash -n)"
if bash -n "$DEPLOY_SCRIPT"; then
  echo "OK"
  PASSED_TESTS=$((PASSED_TESTS + 1))
else
  echo "FAILED"
  exit 1
fi

run_test "test_deploy_registry_cleanup.sh syntax check (bash -n)"
if bash -n "$0"; then
  echo "OK"
  PASSED_TESTS=$((PASSED_TESTS + 1))
else
  echo "FAILED"
  exit 1
fi

run_test "cleanup_daemon_registry.py python syntax check (py_compile)"
if python3 -m py_compile "$CLEANUP_SCRIPT"; then
  echo "OK"
  PASSED_TESTS=$((PASSED_TESTS + 1))
else
  echo "FAILED"
  exit 1
fi

run_test "cleanup_daemon_registry.py is executable"
if [ -x "$CLEANUP_SCRIPT" ]; then
  echo "OK"
  PASSED_TESTS=$((PASSED_TESTS + 1))
else
  echo "FAILED"
  exit 1
fi

run_test "deploy.sh references shared cleanup script without inline duplication"
if grep -q 'scripts/cleanup_daemon_registry.py' "$DEPLOY_SCRIPT" && ! grep -q 'json.load(fp)' "$DEPLOY_SCRIPT"; then
  echo "OK"
  PASSED_TESTS=$((PASSED_TESTS + 1))
else
  echo "FAILED"
  exit 1
fi

# 准备独立沙箱目录
TMP_DIR=$(mktemp -d "${TEST_TMP_BASE%/}/codegraph_test_reg.XXXXXX")
trap 'rm -rf "$TMP_DIR"' EXIT

REG_DIR="$TMP_DIR/daemons"
mkdir -p "$REG_DIR"

execute_cleanup() {
  local killed_pid="$1"
  local workdir="$2"
  local code_home="${3:-$TMP_DIR}"
  
  (
    CODEGRAPH_HOME="$code_home"
    KILLED_PID="$killed_pid"
    WORKDIR="$workdir"
    
    REG_DIR="${CODEGRAPH_HOME:-$HOME/.codegraph}/daemons"
    if [ -d "$REG_DIR" ]; then
      if ! command -v python3 >/dev/null 2>&1; then
        echo "DEPLOY FAILED: python3 is required for registry cleanup" >&2
        exit 1
      fi
      python3 "$CLEANUP_SCRIPT" "$REG_DIR" "${KILLED_PID:-}" "${WORKDIR:-}"
    fi
  )
}

# --- 场景 1: 精确 PID 匹配与防止子串误删 ---
echo "=== 场景 1: PID 比较（精确匹配 vs 子串） ==="
cat > "$REG_DIR/match_pid.json" <<'EOF'
{"pid": 123, "root": "/project/other1", "version": "1.0", "socketPath": "/tmp/s1.sock"}
EOF
cat > "$REG_DIR/substr_longer.json" <<'EOF'
{"pid": 12345, "root": "/project/other2", "version": "1.0", "socketPath": "/tmp/s2.sock"}
EOF
cat > "$REG_DIR/substr_shorter.json" <<'EOF'
{"pid": 12, "root": "/project/other3", "version": "1.0", "socketPath": "/tmp/s3.sock"}
EOF

execute_cleanup "123" "/project/unrelated"

run_test "Exact PID 123 deleted"
assert_file_not_exists "$REG_DIR/match_pid.json"

run_test "Substr PID 12345 preserved"
assert_file_exists "$REG_DIR/substr_longer.json"

run_test "Substr PID 12 preserved"
assert_file_exists "$REG_DIR/substr_shorter.json"

# --- 场景 2: 精确 Root 路径比较与防止前缀/子目录误删 ---
echo "=== 场景 2: Root 路径比较（精确匹配 vs 路径前缀） ==="
cat > "$REG_DIR/match_root.json" <<'EOF'
{"pid": 9001, "root": "/workspace/repo", "version": "1.0", "socketPath": "/tmp/s4.sock"}
EOF
cat > "$REG_DIR/prefix_root.json" <<'EOF'
{"pid": 9002, "root": "/workspace/repo-extended", "version": "1.0", "socketPath": "/tmp/s5.sock"}
EOF
cat > "$REG_DIR/subdir_root.json" <<'EOF'
{"pid": 9003, "root": "/workspace/repo/subdir", "version": "1.0", "socketPath": "/tmp/s6.sock"}
EOF

execute_cleanup "" "/workspace/repo"

run_test "Exact root /workspace/repo deleted"
assert_file_not_exists "$REG_DIR/match_root.json"

run_test "Prefix root /workspace/repo-extended preserved"
assert_file_exists "$REG_DIR/prefix_root.json"

run_test "Subdir root /workspace/repo/subdir preserved"
assert_file_exists "$REG_DIR/subdir_root.json"

# --- 场景 3: 异常与损坏 JSON 保护（不删除） ---
echo "=== 场景 3: 损坏 / 畸形 JSON 保护 ==="
echo "{ unclosed json" > "$REG_DIR/malformed_syntax.json"
echo "" > "$REG_DIR/malformed_empty.json"
echo "[123, 456]" > "$REG_DIR/malformed_array.json"
echo '"string payload"' > "$REG_DIR/malformed_scalar.json"
echo '{"not_a_valid_dict"}' > "$REG_DIR/malformed_bad.json"

execute_cleanup "123" "/workspace/repo"

run_test "Malformed syntax JSON preserved"
assert_file_exists "$REG_DIR/malformed_syntax.json"

run_test "Empty JSON file preserved"
assert_file_exists "$REG_DIR/malformed_empty.json"

run_test "Array JSON preserved"
assert_file_exists "$REG_DIR/malformed_array.json"

run_test "Scalar JSON preserved"
assert_file_exists "$REG_DIR/malformed_scalar.json"

run_test "Bad dictionary JSON preserved"
assert_file_exists "$REG_DIR/malformed_bad.json"

# --- 场景 4: 幂等性测试 ---
echo "=== 场景 4: 重复执行幂等性 ==="
execute_cleanup "123" "/workspace/repo"
execute_cleanup "123" "/workspace/repo"

run_test "Idempotent re-run succeeds without side-effects"
assert_file_exists "$REG_DIR/malformed_syntax.json"

# --- 场景 5: 工具缺失检查与 fail fast ---
echo "=== 场景 5: 缺少 python3 时 fail fast 退出 ==="
run_test "Fail fast when python3 is missing"
MISSING_OUT=$( (PATH="/nonexistent" execute_cleanup "123" "/workspace/repo" 2>&1) || true )
if echo "$MISSING_OUT" | grep -q "DEPLOY FAILED: python3 is required"; then
  echo "OK"
  PASSED_TESTS=$((PASSED_TESTS + 1))
else
  echo "FAILED (expected DEPLOY FAILED message, got: $MISSING_OUT)"
  exit 1
fi

# --- 场景 6: 真实操作错误（删除失败）时 fail fast 退出 ---
echo "=== 场景 6: 删除操作失败时 fail fast 退出 ==="
cat > "$REG_DIR/unremovable.json" <<'EOF'
{"pid": 7777, "root": "/workspace/locked", "version": "1.0", "socketPath": "/tmp/s7.sock"}
EOF
if chattr +i "$REG_DIR/unremovable.json" 2>/dev/null; then
  run_test "Fail fast on operational remove error (immutable file)"
  REMOVE_ERR_OUT=$(execute_cleanup "7777" "" 2>&1 || true)
  chattr -i "$REG_DIR/unremovable.json" 2>/dev/null || true
  rm -f "$REG_DIR/unremovable.json" 2>/dev/null || true
  if echo "$REMOVE_ERR_OUT" | grep -q "DEPLOY FAILED: error removing registry record"; then
    echo "OK"
    PASSED_TESTS=$((PASSED_TESTS + 1))
  else
    echo "FAILED (expected remove error failure, got: $REMOVE_ERR_OUT)"
    exit 1
  fi
fi

# --- 场景 7: 真实操作错误（读取失败）时 fail fast 退出 ---
echo "=== 场景 7: 读取操作失败时 fail fast 退出 ==="
mkdir -p "$REG_DIR/unreadable_dir.json"
run_test "Fail fast on operational read error (directory as .json)"
READ_ERR_OUT=$(execute_cleanup "8888" "" 2>&1 || true)
rmdir "$REG_DIR/unreadable_dir.json" 2>/dev/null || true
if echo "$READ_ERR_OUT" | grep -q "DEPLOY FAILED: error reading registry record"; then
  echo "OK"
  PASSED_TESTS=$((PASSED_TESTS + 1))
else
  echo "FAILED (expected read error failure, got: $READ_ERR_OUT)"
  exit 1
fi

# --- 场景 8: 共享脚本 CLI 独立直接调用验证 ---
echo "=== 场景 8: 共享脚本 CLI 直接执行与边界调用 ==="
run_test "Direct CLI invocation without arguments fails with usage"
USAGE_OUT=$("$CLEANUP_SCRIPT" 2>&1 || true)
if echo "$USAGE_OUT" | grep -q "Usage:"; then
  echo "OK"
  PASSED_TESTS=$((PASSED_TESTS + 1))
else
  echo "FAILED (expected Usage: output, got: $USAGE_OUT)"
  exit 1
fi

run_test "Direct CLI invocation on non-existent directory exits 0"
if "$CLEANUP_SCRIPT" "$TMP_DIR/nonexistent_daemons_dir" 123 "/test/path"; then
  echo "OK"
  PASSED_TESTS=$((PASSED_TESTS + 1))
else
  echo "FAILED"
  exit 1
fi

echo "=== 全部通过 ($PASSED_TESTS/$TOTAL_TESTS) ==="

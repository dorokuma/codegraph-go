#!/usr/bin/env bash
# install.sh — 把仓库版 Pi 扩展同步安装到 ~/.pig/agent/extensions/。
#
# 背景：deploy.sh 只部署 Go 二进制，Pi 扩展（integrations/pi/codegraph-go.ts）
# 的同步此前依赖 integrations/pi/README.md 的手工 install，容易漏——部署副本
# 曾落后仓库一个提交，旧版 formatCleanText 会把 codegraph 返回的代码围栏与
# file:line:content 二次剥掉，导致 action=node / includeCode=true 拿不到源码
# 正文。本脚本把该步骤收进 deploy.sh，也可单独执行验证：
#   integrations/pi/install.sh
#
# DEST 语义定稿：目标是一个「文件路径」（~/.pig/agent/extensions/
# codegraph-go.ts），不接受目录——指向已存在目录时显式失败，不静默补全
# 文件名、不静默拷进目录。PI_EXT_DEST 可覆盖（验证/多机部署场景）。
#
# 幂等：目标 md5 与源一致时报告未变化且不重写（不动 mtime）。
# 失败语义：所有校验/IO 失败都必须有可见输出（stderr）并非零退出——
# 静默失败正是本次要消灭的漂移形态（原 md5 失效路径实测为静默 exit 127、
# stdout/stderr 全空）。R2 双审 must-fix，详见 .agents/notes/
# 20261003-pi-ext-deploy-sync.md。
set -euo pipefail

fail() { echo "install.sh FAILED: $*" >&2; exit 1; }

# md5sum 缺失必须显式失败：曾经的 2>/dev/null 把「command not found」也吞了，
# 表现为零输出 + exit 127，同步失败却无任何信号。
command -v md5sum >/dev/null 2>&1 || fail "md5sum not found in PATH (PATH=$PATH)"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SRC="$ROOT/integrations/pi/codegraph-go.ts"

# 目标解析顺序：优先采纳显式覆盖 PI_EXT_DEST，仅在未指定时才校验 HOME、
# 回落到默认路径。修前顺序倒挂（先校验 HOME 再取 PI_EXT_DEST），导致显式设了
# PI_EXT_DEST 也照样被「HOME is not set」拦下，而报错文案偏又教用户「set
# PI_EXT_DEST explicitly」——文案与行为自相矛盾（实测：env -u HOME
# PI_EXT_DEST=/tmp/x.ts bash install.sh 修复前 FAILED、exit 1）。
if [ -n "${PI_EXT_DEST:-}" ]; then
  DEST="$PI_EXT_DEST"
  EXPLICIT_DEST=1
else
  # HOME 未导出时 set -u 只会抛一句晦涩的「未绑定的变量」；显式给出可操作提示。
  HOME_DIR="${HOME:-}"
  [ -n "$HOME_DIR" ] || fail "HOME is not set (or empty) — export HOME or set PI_EXT_DEST explicitly"
  DEST="$HOME_DIR/.pig/agent/extensions/codegraph-go.ts"
  EXPLICIT_DEST=0
fi
[ -n "$DEST" ] || fail "destination is empty"
case "$DEST" in
  /*) ;;
  *) fail "destination is not an absolute path: $DEST" ;;
esac
case "$DEST" in
  */)
    fail "PI_EXT_DEST ($DEST) must not end with '/' — expected the extension file path (须指定文件名而非目录路径)"
    ;;
esac
if [ -d "$DEST" ]; then
  fail "destination is a directory: $DEST — expected the extension file path (须指定文件名而非目录本体)"
fi

[ -f "$SRC" ] || fail "source not found: $SRC"

# 原生 Go 扩展共存保护（zero-node 宿主机）：默认目标路径下若已存在同名原生
# Go 扩展目录（含 go.mod，如 ~/.pig/agent/extensions/codegraph-go 指向
# pi-extensions/go/codegraph-go 的软链），说明本机走原生扩展路线——把 .ts
# 拷进去会让 pig 启动时调起已不存在的 node 重打包，所有会话直接起不来
# （spawn:runtime_unavailable: TypeScript extensions need Node.js 22.13）。
# 此时先自愈清理残留的 .ts 副本/坏软链，再显式 exit 0，绝不写入 TypeScript
# 扩展。仅作用于默认路径（EXPLICIT_DEST=0）；显式 PI_EXT_DEST 时 100% 保留
# 原有安装与 md5 幂等行为。
if [ "$EXPLICIT_DEST" -eq 0 ]; then
  NATIVE_EXT="${DEST%.ts}"
  if [ -d "$NATIVE_EXT" ] && [ -f "$NATIVE_EXT/go.mod" ]; then
    # 残留文件或非目录软链自愈：.ts 已存在说明上次同步曾写入过，先清掉。
    if [ -e "$DEST" ] || [ -L "$DEST" ]; then
      rm -f -- "$DEST" || fail "failed to remove stale TypeScript extension: $DEST"
    fi
    echo "pi extension sync"
    echo "  native Go ext: $NATIVE_EXT"
    echo "skipped: native Go extension active at $NATIVE_EXT (TypeScript $DEST not installed)"
    exit 0
  fi
fi

md5_of() {
  local line
  # 不吞 stderr、不吞退出码：md5sum 本身的失败（如文件不可读）也要显性化。
  if ! line="$(md5sum "$1")"; then
    fail "md5sum failed for: $1"
  fi
  printf '%s' "${line%% *}"
}

SRC_MD5="$(md5_of "$SRC")"
[ -n "$SRC_MD5" ] || fail "source md5 is empty: $SRC"
if [ -f "$DEST" ]; then
  DEST_MD5="$(md5_of "$DEST")"
  [ -n "$DEST_MD5" ] || fail "target md5 is empty: $DEST"
else
  DEST_MD5="(none)"
fi

echo "pi extension sync"
echo "  source:      $SRC"
echo "  source md5:  $SRC_MD5"
echo "  destination: $DEST"
echo "  target md5:  $DEST_MD5"

if [ -f "$DEST" ] && [ "$DEST_MD5" = "$SRC_MD5" ]; then
  echo "unchanged: destination already matches source (md5 $SRC_MD5), nothing written"
else
  # 目标父目录在新机器上可能不存在（~/.pig/agent/extensions），先建好。
  DEST_DIR="$(dirname "$DEST")"
  mkdir -p "$DEST_DIR" || fail "failed to create destination directory: $DEST_DIR"
  install -m 644 "$SRC" "$DEST" || fail "failed to install $SRC to $DEST"
  echo "changed: installed $DEST (md5 $SRC_MD5)"
fi

echo "note: Pi 只在会话启动时加载扩展——需 /reload 或新会话才生效"

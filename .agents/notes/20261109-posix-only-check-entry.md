---
status: active # active | superseded
superseded_by: ""
supersedes: ""
模块: tools # db | extraction | resolution | server | sync | tools
---

# `make check` 的 `go build -o /dev/null` 仅面向 POSIX/Linux 宿主（基线遗留留痕）

## 一句话结论
- `Makefile` 的 `check` 目标中 `CGO_ENABLED=1 go build -o /dev/null ./cmd/codegraph-go` 面向 POSIX/Linux 宿主与 Ubuntu CI，当前成立；若未来支持原生 Windows 开发机，再改为临时文件+清理方案。

## 背景
- C 档首轮 `oracle` 审查观察项 `[pattern:posix-only-check-entry]`：`make check` 的构建烟测把二进制写向 `/dev/null`，该路径在 Windows 上不存在，意味着检查入口隐含 POSIX 假设。
- 本仓因 `smacker/go-tree-sitter` 的 CGO 例外本就只支持 Linux/macOS（见 `pin_other.go` 注释：非 unix 平台整仓构建不受支持），且 CI 为 Ubuntu runner，故无现实影响。

## 决策
- 保持 `go build -o /dev/null` 不变：简单、零残留、与现有 POSIX-only 约束一致。
- 触发变更条件：未来若明确支持原生 Windows 开发机，届时把输出改为 `$(shell mktemp)`/临时文件并在 recipe 末尾清理。

## 被放弃的方案（必填）
- 方案 A：现在就改为临时文件+`trap` 清理以「面向未来」兼容 Windows。放弃理由：当前无任何 Windows 构建支持计划，引入额外复杂度属过度设计；且 `check` 目标其余步骤（`gofmt -l .`、shell recipe）同样隐含 POSIX shell。
- 方案 B：在 Makefile 中加平台条件分支（Windows 下改用 `NUL`）。放弃理由：同上，且 `$(OS)`/`COMSPEC` 探测会污染极简检查入口。

## 来源
- 关联审查：codegraph-go `chore/baseline-l2-make-check` 分支 C 档首轮 `oracle` 审查（`[pattern:posix-only-check-entry]`，1/2 项）。

---
status: deployed-uncommitted
tags: [pattern:mcp-arguments-jsonstring-unwrap]
---
# MCP tools/call arguments 字符串化解包兼容

agy lazy 工具桥 `call_mcp_tool` 声明 `Arguments` 为 `type:string`，宿主只解一层
JSON；模型偶发双重 stringify 的 `"{...}"` 透传到服务端违反规范。修复：同款
`internal/server/argunwrap.go` 挂 `AddReceivingMiddleware`（daemon 侧生效，
daemon.log 可见 unwrap 日志）。详见 CHANGELOG [Unreleased] 与同批
pi-extensions notes（20261111-mcp-args-jsonstring-unwrap.md）。

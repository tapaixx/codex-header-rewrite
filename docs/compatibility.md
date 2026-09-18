# Compatibility

当前实现按 CLIProxyAPI 当前主线 contract 对齐：

- native ABI: 1
- RPC schema: 6
- `request.intercept_after`
- `request.complete`
- `response.intercept_after`
- `response.intercept_stream_chunk`
- `management.register` / `management.handle`
- `host.auth.list` / `host.auth.get_runtime`
- `host.auth.get` / `host.http.do`（仅测试请求用到）
- metadata: `selected_auth_id` / `selected_auth_index`

宿主若不提供 `host.auth.get` 或 `host.http.do`，Header 改写、历史与模型比对照常工作，只有测试请求的「发送」会报 host callback 失败；「预览」不依赖这两个能力。

通用 plugin ABI 当前没有“最终 outbound http.Request 已完成构造”的 provider-independent hook，所以插件不能保证看到最终 transport Header。

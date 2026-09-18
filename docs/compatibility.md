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
- metadata: `selected_auth_id` / `selected_auth_index`

通用 plugin ABI 当前没有“最终 outbound http.Request 已完成构造”的 provider-independent hook，所以插件不能保证看到最终 transport Header。

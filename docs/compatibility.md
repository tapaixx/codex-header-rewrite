# Compatibility

当前实现按 CLIProxyAPI 当前主线 contract 对齐：

- native ABI: 1
- RPC schema: 6
- `plugin.quiesce`（热更新前刷盘并释放 bbolt 文件锁）
- `request.intercept_after`
- `request.complete`
- `response.intercept_after`
- `response.intercept_stream_chunk`
- `management.register` / `management.handle`
- `host.auth.list` / `host.auth.get_runtime`
- `host.auth.get`（线上响应 state 的套餐判定，以及测试请求读取凭证）/ `host.http.do`（仅测试请求）
- `host.http.do` 的 `wire_profile`（Codex 测试端点的 HTTP/1.1 Header 顺序）
- Management `GET /auth-files/models`（测试表单的可用模型下拉；缺失时自动回退为手填）
- metadata: `selected_auth_id` / `selected_auth_index`

宿主若不提供 `host.auth.get`，Header 改写、历史与模型比对仍工作，但线上 state 因套餐未知只记录历史、不进入不降智池，测试请求的「发送」也无法读取凭证；读取失败不会永久缓存，后续请求会重试。缺少 `host.http.do` 时只有测试请求的「发送」失败；「预览」不依赖这两个能力。旧宿主忽略或不支持 `wire_profile` 时仍能发送请求，只是没有稳定的 HTTP Header 线级配置。

通用 plugin ABI 当前没有“最终 outbound http.Request 已完成构造”的 provider-independent hook，所以插件不能保证看到最终 transport Header。

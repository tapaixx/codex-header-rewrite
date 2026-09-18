# Security

Plugin Resource 路由只提供静态 HTML；credential、rule、history、test、cleanup 都放在受 CPA Management API 认证保护的路由下。

面板优先读取宿主管理面板已保存的管理密钥（localStorage，含 `enc::v1` / `enc::v2` 混淆信封），因此默认不引入第二套凭据生命周期。只有读不到宿主密钥时才回退到手工填写，手工填写的密钥仅存当前标签页 `sessionStorage`，不会写入插件数据库。无法解码的信封不会被当作密钥原样发送。

历史写盘前永久脱敏：
- Authorization / Proxy-Authorization
- Cookie / Set-Cookie
- X-Api-Key / Api-Key / X-Goog-Api-Key / X-Auth-Token
- 名称包含 token / secret / password / credential / api-key / apikey 的 Header

`Chatgpt-Account-Id` 不在脱敏名单内：它是不透明账号标识（不是邮箱），而且它本身就是规则常改写的字段，遮掉就失去了 Header 检查的意义。

规则值必须以原值持久化才能重启后继续生效，因此 bbolt DB 本身应视作敏感数据，文件权限为 `0600`。

## 请求体与响应体

插件读取响应载荷，只为取出上游声明的模型名，取完即丢。请求体与响应体都不写入持久化，也不返回给面板 —— 唯一例外是测试请求失败时返回一段截断的上游错误文本（600 字符上限），它只出现在这一次的 API 响应里，不入库。

## 测试请求的额度与目标地址

- 真实测试请求会消耗该凭证的真实额度，只能由操作者显式触发；插件自身不调度任何请求。
- 自定义端点必须是绝对 `https` URL；明文 `http` 只允许 `localhost`、`127.0.0.1` 或 `::1`，URL 中不得嵌入用户名/密码。
- `chatgpt.com` 默认附带所选凭证。其他主机默认不读取、不附带凭证；只有操作者明确勾选后，才会把 access token 与账号 ID 发送给该地址，面板同时显示醒目警告并在发送前二次确认。
- 账号标识只从 `id_token.chatgpt_account_id` 及其显式别名读取。同级的 `account` / `id` 字段装的是操作者邮箱，不是账号标识，取不到标识时如实报错，不拿地址顶替。
- 凭证材料在发出请求前即时读取，用完不保留、不记录、不返回面板。
- 测试请求不复制历史 Header 模板；插件只保留 JSON/SSE 必需的 `Content-Type` 与 `Accept`，不设置 `User-Agent`。历史详情里的 Turn-State 可通过“解码 state”按钮送入解码接口，不需要手动复制粘贴。

## 回合状态 blob

- 短期溯源索引只在内存中保存 blob 摘要与来源凭证；不降智 State 池则会把每个「凭证 + 模型」最新的原始 blob 写入配置的 bbolt `data_path`（默认在 `plugins/data`），以便 CPA 重启和插件更新后恢复。数据库权限为 `0600`，目录为 `0700`；备份和持久卷应按敏感运行数据保护。
- 原始池值只通过需要 CPA 管理密钥的 Management API `GET /codex-header-rewrite/turn-states` 返回；响应带 `Cache-Control: no-store`，普通代理流量端点不暴露该值。
- blob 原文本身仍会随 Request / Response Header 一起进入历史 —— 它是本插件要检查的对象，与 Authorization 不同，遮掉就失去意义。它是不透明的回合状态，不是长期凭证。
- 解码只读 Fernet 信封的版本、时间戳与长度结构；插件不持有、也不需要 Fernet 密钥，不会尝试解密密文。
- 摘除守卫只在确认来源是另一个凭证或模型时生效；来源未知时不动客户端回带的值。

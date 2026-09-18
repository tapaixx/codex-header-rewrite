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
- 测试请求携带 bearer token，因此目标地址被钉死在 `https://chatgpt.com/backend-api/codex/` 前缀内：scheme、host 不可改，只能改路径。任意目标地址等于把凭证交给第三方。
- 账号标识只从 `id_token.chatgpt_account_id` 及其显式别名读取。同级的 `account` / `id` 字段装的是操作者邮箱，不是账号标识，取不到标识时如实报错，不拿地址顶替。
- 凭证材料在发出请求前即时读取，用完不保留、不记录、不返回面板。

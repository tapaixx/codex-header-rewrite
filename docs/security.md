# Security

Plugin Resource 路由只提供静态 HTML；credential、rule、history、cleanup 都放在受 CPA Management API 认证保护的路由下。

Management Key 仅存浏览器 `sessionStorage`，不会写入插件数据库。

历史写盘前永久脱敏：
- Authorization / Proxy-Authorization
- Cookie / Set-Cookie
- X-Api-Key / Api-Key / X-Goog-Api-Key / X-Auth-Token
- 名称包含 token / secret / password / credential / api-key / apikey 的 Header

规则值必须以原值持久化才能重启后继续生效，因此 bbolt DB 本身应视作敏感数据，文件权限为 `0600`。

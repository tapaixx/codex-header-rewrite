# Architecture

```text
Client
 -> CPA credential selection
 -> request.intercept_after
    -> selected_auth_index
    -> Codex credential check
    -> apply per-credential Set/Remove
    -> create attempt
 -> Codex Provider Executor
 -> upstream
 -> response.intercept_after / response.intercept_stream_chunk
    -> header-init chunk: record response headers
    -> data chunks: read the model the upstream declares
 -> request.complete
    -> finalize attempt (upstream model, mismatch verdict)
    -> async persistence queue
```

规则和历史以稳定 `auth_index` 为主键。credential retry 时，同一 RequestID 可产生多个 attempt。

## 热更新生命周期

CPA 替换版本化 `.so` 时先对旧实例调用 `plugin.quiesce`，再注册新实例。插件在 quiesce 中停止
持久化队列、刷盘并关闭 bbolt，必须在返回前释放 DB 的进程级文件锁；否则新实例会卡在打开同一
数据库，宿主的 apply 锁也不会释放，后续卸载同样会被阻塞。若新实例注册失败，CPA 会重新注册
旧实例，`plugin.register` 会重新打开数据库并恢复规则。

## 上游模型观测

模型观测读取响应载荷中的 `response.model`、Claude 的 `message.model` 和顶层 `model`。CPA 的线上回调位于格式转换之后，模型字段可能由宿主补写；记录表示响应报告的模型，并不证明底层实际服务模型。回调入口先识别独立完整 JSON/data 行，其他数据交给支持 LF/CRLF 和跨块帧的 SSE 解析器；JSON `type` 也用于终局判定。未读到字段显示“未获取到”，不认定上游未声明。

观测器按帧读取 SSE：

- 终局事件（`response.completed` / `response.done` / `response.failed` / `response.incomplete` / `response.cancelled`）的声明覆盖先前声明；否则保留第一个声明。
- 两个声明互相矛盾时记为 `model_conflict`，不猜测哪个为准。
- chunk 边界和 SSE 帧边界不对齐。先按 `\n\n` 切出完整帧；剩下的尾巴**当场尝试按一帧解析**：解析成功说明它本来就是完整的一帧（宿主逐事件回调时不带空行分隔符），解析失败说明只是被截断，才缓存到下一个 chunk（上限 64 KiB），`request.complete` 时再 flush 一次。
- 一帧里的多个 `data:` 行按 SSE 规范用 `\n` 拼接后解析；拼接结果不是合法 JSON 时，再退化为把每一行当作独立载荷（宿主把多个事件塞进一帧的情形）。
- 上游一次都没声明模型时，判定结果是「未知」（`model_mismatch` 缺省），不是「一致」。

被拦截的降智响应同样会读模型：chunk 仍然到达插件，只是不下发给客户端。后台重试也读自己的响应体，取模型名并留一份遮蔽副本。

载荷最外层是 JSON 数组时逐元素读取；事件包在 `data` 键下时再往里看一层。

## Body 记录与遮蔽

```text
request.intercept_after   -> pendingAttempt.requestBody = maskRequestBody(req.Body)      // 遮蔽 input
response / stream chunk   -> pendingAttempt.responseBody += chunk                        // 原样累积，上限 4×maxStoredBodyBytes
request.complete          -> ResponseBody = storedBody(maskResponseBody(累积))           // 遮蔽 output，再截到 256 KB
AppendHistory             -> record.takeBodies() -> history_bodies/<auth>/<record id>
```

遮蔽走 `maskBodyField`：JSON 文档递归替换同名字段，SSE 按 `data:` 行逐帧替换并保留 `event:` 行、`[DONE]` 与帧结构，解析不了的原样返回。流式必须累积完再遮蔽——按 chunk 遮蔽会放过跨 chunk 拆开的帧。

body 与记录分桶存放：`History` 一页要解码整个桶来排序，载荷放在一起就得为一个不显示它们的列表读几十 MB。`GET /history/body?auth_index=&id=` 按需单取；淘汰、`ClearHistory` 与 `DeleteCredentialData` 都会连带删除。

载荷只在内存里解析，取出模型名后即丢弃；请求体和响应体都不进入持久化。

## 回合状态溯源

`X-Codex-Turn-State` 由上游在响应头返回，客户端在同一回合的下一次请求原样回带。插件按 blob 的 SHA-256 摘要前 16 位建立（摘要 → 来源凭证）内存索引：

```text
response.intercept_after / stream header-init
 -> 响应头里有 blob -> 记 (摘要 -> auth_index, label, mintedAt)

request.intercept_after
 -> 请求头里有 blob -> 查摘要
    -> 命中且来源凭证 != 当前 auth_index -> 标记 turn_state_cross_account
       -> 注入开关打开 -> 把该 Header 加入 ClearHeaders，并从"改写后"视图中移除
    -> 命中且来源凭证 == 当前 auth_index -> 标记回带一致
    -> 未命中 -> 不判定（未知不是一致）
```

跨号回带不是客户端的错误行为，而是代理选号的结果：CPA 的 `routing.strategy` 默认 `round-robin`，按请求轮换凭证，`routing.session-affinity` 默认关闭；即使开启，绑定凭证不可用时仍会自动故障转移。客户端只看到一个端点，原样回带它收到的 state。

复用判定按三条规则：同凭证、同模型、在复用窗口内。窗口是 `headerRule.state_ttl_seconds`，按凭证维护，单位秒，默认 200；读取时走 `turnStateReuseWindowLocked(authIndex)`，字段为 0 的旧规则读成默认值。跨号与跨模型是确定不可复用，注入开关打开时会摘除；过期只标记不摘除，因为窗口未经上游确认。

每个上游返回值都会分类：Team 套餐 `≤ 332` 字符、个人套餐（非 Team）`≤ 292` 字符才是不降智。分档只判断套餐名是不是 `team`，没见过的套餐名一律走个人档，因此不需要枚举个人套餐；未声明套餐或超限值不入池。不降智值铸造进「凭证 + 模型 → 最新原始 blob」持久池，写入配置的 bbolt `data_path`，启动时恢复，乱序返回不回退。内存摘要索引仍有 2 小时 TTL 与 512 条上限；它只负责短期来源关联，不决定持久池是否保留。

blob 本身是 Fernet token：`1 字节版本 + 8 字节大端时间戳 + 16 字节 IV + 16n 字节密文 + 32 字节 HMAC`。插件只读前 9 字节与总长度，不持有密钥、不解密密文。

## 出站注入

```text
request.intercept_after
 -> 规则开关关闭、无规则，或 inject_turn_state 关闭 -> 不注入（按客户端原样透传）
 -> 规则开关开启
    -> 规则里手工写过 X-Codex-Turn-State（设置或移除） -> 不注入（operator 优先）
    -> 否则查池 (auth_index + model)
       -> 套餐已知且 state 合格 -> 写入 updates，并从 ClearHeaders 撤回同名移除
       -> 否则不注入
```

```text
response（header-init / response.intercept_after）
 -> 铸造判定为疑似降智
    -> 本次注入的是已过期的池 state（注入时 mintedAt 距今 > 该凭证的 state_ttl_seconds）
       -> 池里该 (凭证 + 模型) 仍是同一 digest -> 删除池项（内存 + bbolt），attempt 记 turn_state_invalidated
       -> 池里已是更新的 state -> 不动
    -> 注入的 state 尚在窗口内 -> 不动（单次降智不足为证）
```

注入与摘除共享同一个 Header：刚把不可复用的回带值排入 ClearHeaders 时，注入会撤回那条移除再写入新值，避免一次响应里同时下发"设置"和"清除"同一 Header 的矛盾指令。命中注入会在 attempt 上记 `turn_state_injected`。

## 降智响应拦截

```text
response.intercept_after / stream header-init
 -> 判定为疑似降智 且 rule.RejectDegradedResponse 且实际发出模型命中范围
    -> attempt.rejecting = true，记 turn_state_rejected
    -> 非流式：Body 换成 {"error":{"type":"degraded_turn_state",...}}，加 X-Codex-Header-Rewrite 头
    -> 流式：header-init 只加标记头；第 1 个数据 chunk 换成 `event: error` 帧；之后每个 chunk DropChunk
```

ABI 只允许替换 Headers / Body 与按 chunk 丢弃（`DropChunk`），不能改状态码、不能中止连接，也不会触发宿主的凭证故障转移；上游仍会把响应流完。

范围由 `reject_degraded_models` 指定：空列表匹配全部，非空与 `sentModel(Model, RequestedModel)` 精确比较。响应拦截不依赖请求改写开关。只有被拦截且启用 `retry_on_degraded` 的请求才启动后台最小请求；`retry_attempts` 是最大次数，拿到合格 state 后提前结束。

重试的 Header 由 `retryHeaders` 组装：`Content-Type` / `Accept` / `Originator` / `Authorization` / `Chatgpt-Account-Id`，再加上会话 `Cookie`：

```text
request.intercept_after
 -> pendingAttempt.clientCookie = joinCookieHeader(req.Headers)   // HTTP/2 多段按 "; " 拼回
response（header-init / intercept_after）判定拦截且要重试
 -> clientCookie = applySetCookies(clientCookie, responseHeaders)
    -> 只取 name=value，丢掉 Path/Domain/Expires/Secure/HttpOnly/SameSite
    -> 同名覆盖且保持原位置；新签发的追加在末尾
    -> 空值或 Max-Age<=0 -> 删除该 crumb（不回空值）
 -> scheduleDegradedRetry
```

**Header key 一律大小写不敏感读取**：ABI 的 header map 直接由 JSON 反序列化而来（`out.Headers = http.Header(slices)`），键保留宿主序列化时的拼写，HTTP/2 下是小写；而 `http.Header.Get` / `Values` 只规范化它查的键，不规范化 map 里已有的键，且规范化对某些名字不是恒等（`X-OpenAI-...` 的规范形式是 `X-Openai-...`）。所以所有入站 header 读取都走 `headerValuesFold` / `headerValueFold`，按 `EqualFold` 匹配。由于这些键从不被归一化，同一个 header 可能同时以两种拼写存在，`headerValuesFold` 会把**每一个**匹配键的值都收集起来，并按键排序后拼接，结果不依赖 map 迭代顺序。

注意方向相反的一条：**header 名大小写不敏感，cookie 名大小写敏感**（RFC 6265）。`session` 和 `Session` 是两个 cookie，`applySetCookies` 不会让其中一个覆盖另一个。

`clientCookie` 只存在于在途 `pendingAttempt` 上，不是 `historyRecord` 的字段。`Cookie` 与 `Set-Cookie` 自 v0.20.1 起不在 `exactSensitiveHeaders` 中，因此 `redactHeaders` 按明文记录它们——这是为了能比对重试与线上请求的会话；代价是 bbolt 文件里含可用会话。

## 测试请求

测试请求不走 CPA 的 provider executor 转发链路，而是插件调用 CPA 宿主的 `host.http.do`：

```text
management POST /codex-header-rewrite/test
 -> host.auth.get_runtime  (Codex 类型校验)
 -> 解析端点与附带凭证策略
 -> host.auth.get          (仅在需要附带凭证时读取)
 -> 组装基础 Header + 本次临时 Header
 -> 套用规则（可选）+ 本次临时移除
 -> host.http.do           (dry_run 时跳过；使用宿主代理感知传输层)
 -> 读响应载荷取上游模型 -> 比对
 -> 可选写入历史（origin=test）
```

因此测试结果反映的是**规则生效后的 Header**，而不是线上请求最终到达 socket 的 Header —— 线上路径的 Codex executor 仍可能覆盖 `Authorization`、`User-Agent`、`Chatgpt-Account-Id` 等字段。请求确实从 CPA 发出并继承 CPA 的代理；Codex 端点还通过 `wire_profile` 固定 HTTP/1.1 Header 顺序并禁用自动压缩注入，但不复刻完整 TLS 指纹。`dry_run` 不读凭证、不发请求，只解析 Header。

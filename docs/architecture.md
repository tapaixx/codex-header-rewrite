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
- chunk 边界和 SSE 帧边界不对齐，未完成的尾帧会缓存到下一个 chunk（上限 64 KiB），`request.complete` 时再 flush 一次。
- 上游一次都没声明模型时，判定结果是「未知」（`model_mismatch` 缺省），不是「一致」。

载荷只在内存里解析，取出模型名后即丢弃；请求体和响应体都不进入持久化。

## 回合状态溯源

`X-Codex-Turn-State` 由上游在响应头返回，客户端在同一回合的下一次请求原样回带。插件按 blob 的 SHA-256 摘要前 16 位建立（摘要 → 来源凭证）内存索引：

```text
response.intercept_after / stream header-init
 -> 响应头里有 blob -> 记 (摘要 -> auth_index, label, mintedAt)

request.intercept_after
 -> 请求头里有 blob -> 查摘要
    -> 命中且来源凭证 != 当前 auth_index -> 标记 turn_state_cross_account
       -> 规则开启守卫 -> 把该 Header 加入 ClearHeaders，并从"改写后"视图中移除
    -> 命中且来源凭证 == 当前 auth_index -> 标记回带一致
    -> 未命中 -> 不判定（未知不是一致）
```

复用判定按三条规则：同凭证、同模型、在复用窗口（经验值 1 小时）内。跨号与跨模型是确定不可复用，守卫会摘除；过期只标记不摘除，因为窗口未经上游确认。

每个上游返回值都会分类：Team 套餐 `≤ 332` 字符、个人套餐（非 Team）`≤ 292` 字符才是不降智。分档只判断套餐名是不是 `team`，没见过的套餐名一律走个人档，因此不需要枚举个人套餐；未声明套餐或超限值不入池。不降智值铸造进「凭证 + 模型 → 最新原始 blob」持久池，写入配置的 bbolt `data_path`，启动时恢复，乱序返回不回退。内存摘要索引仍有 2 小时 TTL 与 512 条上限；它只负责短期来源关联，不决定持久池是否保留。

blob 本身是 Fernet token：`1 字节版本 + 8 字节大端时间戳 + 16 字节 IV + 16n 字节密文 + 32 字节 HMAC`。插件只读前 9 字节与总长度，不持有密钥、不解密密文。

## 出站注入

```text
request.intercept_after
 -> 规则开关关闭或无规则 -> 不注入（按客户端原样透传）
 -> 规则开关开启
    -> 规则里手工写过 X-Codex-Turn-State（设置或移除） -> 不注入（operator 优先）
    -> 否则查池 (auth_index + model)
       -> 套餐已知且 state 合格 -> 写入 updates，并从 ClearHeaders 撤回同名移除
       -> 否则不注入
```

注入与守卫摘除共享同一个 Header：守卫刚把不可复用的回带值排入 ClearHeaders 时，注入会撤回那条移除再写入新值，避免一次响应里同时下发"设置"和"清除"同一 Header 的矛盾指令。命中注入会在 attempt 上记 `turn_state_injected`。

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

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

## 上游模型观测

上游实际服务的模型只出现在响应载荷里：Codex Responses 流式事件用 `response.model`，非流式响应体用 `model`。Response Header 里没有这个信息，所以只看 Header 无法发现「请求 A 模型、上游给了 B 模型」。

观测器按帧读取 SSE：

- 终局事件（`response.completed` / `response.done` / `response.failed` / `response.incomplete` / `response.cancelled`）的声明覆盖先前声明；否则保留第一个声明。
- 两个声明互相矛盾时记为 `model_conflict`，不猜测哪个为准。
- chunk 边界和 SSE 帧边界不对齐，未完成的尾帧会缓存到下一个 chunk（上限 64 KiB），`request.complete` 时再 flush 一次。
- 上游一次都没声明模型时，判定结果是「未知」（`model_mismatch` 缺省），不是「一致」。

载荷只在内存里解析，取出模型名后即丢弃；请求体和响应体都不进入持久化。

## 测试请求

测试请求不走 CPA 的转发链路，而是插件通过 `host.http.do` 直接向 Codex 后端发一次请求：

```text
management POST /codex-header-rewrite/test
 -> host.auth.get_runtime  (Codex 类型校验)
 -> host.auth.get          (access token + id_token.chatgpt_account_id)
 -> 组装基础 Header + 本次临时 Header
 -> 套用规则（可选）+ 本次临时移除
 -> host.http.do           (dry_run 时跳过)
 -> 读响应载荷取上游模型 -> 比对
 -> 可选写入历史（origin=test）
```

因此测试结果反映的是**规则生效后的 Header**，而不是线上请求最终到达 socket 的 Header —— 线上路径的 Codex executor 仍可能覆盖 `Authorization`、`User-Agent`、`Chatgpt-Account-Id` 等字段。`dry_run` 不读凭证、不发请求，只解析 Header。

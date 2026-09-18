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
 -> response.intercept_after / response.intercept_stream_chunk(header-init)
 -> request.complete
    -> finalize attempt
    -> async persistence queue
```

规则和历史以稳定 `auth_index` 为主键。credential retry 时，同一 RequestID 可产生多个 attempt。

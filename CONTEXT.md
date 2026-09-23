# Codex Turn State

This context describes how observed Codex turn-state blobs are classified and retained for safe reuse analysis.

## Language

**Observed state**:
An `X-Codex-Turn-State` blob received from an upstream response, before any quality decision.
_Avoid_: Minted state

**Non-degraded state**:
An observed state the rule that applies (`degraded.go`) did not mark as degraded: for a live request the injection rule, for a probe the latency rule. The former wire-length rule (team at most 332 characters, personal at most 292) is kept as `judgeByLength` and no longer runs.
_Avoid_: Good state, valid state

**Minting**:
Adding a non-degraded state to the state pool, replacing the previous entry for the same credential and model when it is newer. States from the plugin's own requests (probe, retry) are minted automatically. A live request's state is never minted automatically: the injection rule either calls it degraded or judges nothing, so it is recorded and waits for a manual pool.
_Avoid_: Observing, receiving

**Injection rule**:
The live judgement (`judgeInjectedTurn`). A request that went out carrying an injected state and came back with no state in its response headers is non-degraded; one that came back with a state is degraded. Nothing injected means no verdict.
_Avoid_: Echo rule

**Latency rule**:
The probe judgement (`judgeProbeLatency`), read from the probe's own upstream round trip: at most 10 seconds is non-degraded and pools, up to 20 seconds is suspect, beyond that is degraded.
_Avoid_: Timeout rule

**Suspect state**:
A probe state the latency rule placed in the middle band. Kept out of the pool without being called degraded; carried in history as `suspect`, never beside a non-degraded verdict.
_Avoid_: Maybe degraded

**Manual pool**:
An operator minting a recorded live state from the history drawer. The plugin rebuilds the entry from the stored record (response state, request Cookie merged with Set-Cookie) without calling the upstream, and it takes the slot even from a newer state.
_Avoid_: Force pool, pin

**State pool**:
The durable set containing only the newest minted state for each credential and model. It stores the original state in the plugin bbolt database so CPA rebuilds and plugin updates do not empty the pool.
_Avoid_: Provenance table, recent states

**Degraded state**:
An observed state whose character length exceeds its credential plan limit. It remains visible in history but never enters the state pool.
_Avoid_: Invalid state

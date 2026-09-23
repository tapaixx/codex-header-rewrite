# Codex Turn State

This context describes how observed Codex turn-state blobs are classified and retained for safe reuse analysis.

## Language

**Observed state**:
An `X-Codex-Turn-State` blob received from an upstream response, before any quality decision.
_Avoid_: Minted state

**Non-degraded state**:
An observed state the judgement in force (`degraded.go`) did not mark as degraded. The judgement is currently paused, so every observed state counts as non-degraded; the former wire-length rule (team at most 332 characters, personal at most 292, no plan claim means unclassified) is kept as `judgeByLength`.
_Avoid_: Good state, valid state

**Minting**:
Adding a non-degraded state to the state pool, replacing the previous entry for the same credential and model when it is newer. States from the plugin's own requests (probe, retry) are minted automatically. A live request's state is minted automatically only while a rule actually judges it (`autoPoolsLive`); while the judgement is paused it is recorded and waits for a manual pool.
_Avoid_: Observing, receiving

**Response marker**:
A string named in the server's plugin config (`probe_degraded_markers`). A probe response whose event names, event types or field names contain one is degraded. Markers are server configuration only and never appear in the repository, the binary, the panel or the management API.
_Avoid_: Signature, fingerprint

**Manual pool**:
An operator minting a recorded live state from the history drawer. The plugin rebuilds the entry from the stored record (response state, request Cookie merged with Set-Cookie) without calling the upstream, and it takes the slot even from a newer state.
_Avoid_: Force pool, pin

**State pool**:
The durable set containing only the newest minted state for each credential and model. It stores the original state in the plugin bbolt database so CPA rebuilds and plugin updates do not empty the pool.
_Avoid_: Provenance table, recent states

**Degraded state**:
An observed state whose character length exceeds its credential plan limit. It remains visible in history but never enters the state pool.
_Avoid_: Invalid state

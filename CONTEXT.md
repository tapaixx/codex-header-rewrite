# Codex Turn State

This context describes how observed Codex turn-state blobs are classified and retained for safe reuse analysis.

## Language

**Observed state**:
An `X-Codex-Turn-State` blob received from an upstream response, before any quality decision.
_Avoid_: Minted state

**Non-degraded state**:
An observed state whose wire-text length is within the inclusive limit for its credential plan: Team at most 332 characters, Pro at most 292 characters.
_Avoid_: Good state, valid state

**Minting**:
Adding a non-degraded state to the state pool, replacing the previous entry for the same credential and model when it is newer.
_Avoid_: Observing, receiving

**State pool**:
The durable set containing only the newest minted state for each credential and model. It stores the original state in the plugin bbolt database so CPA rebuilds and plugin updates do not empty the pool.
_Avoid_: Provenance table, recent states

**Degraded state**:
An observed state whose character length exceeds its credential plan limit. It remains visible in history but never enters the state pool.
_Avoid_: Invalid state

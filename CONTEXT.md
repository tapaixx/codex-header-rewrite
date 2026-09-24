# Codex Turn State

This context describes how observed Codex turn-state blobs are classified and retained for safe reuse analysis.

## Language

**Observed state**:
An `X-Codex-Turn-State` blob received from an upstream response, before any quality decision.
_Avoid_: Minted state

**Non-degraded state**:
An observed state the rule that applies (`degraded.go`) did not mark as degraded: for a live request the injection rule, for a probe with multi-check on the same rule applied to its check request. The former wire-length rule (team at most 332 characters, personal at most 292) is kept as `judgeByLength` and no longer runs.
_Avoid_: Good state, valid state

**Minting**:
Adding a non-degraded state to the state pool, replacing the previous entry for the same credential and model when it is newer. States from the plugin's own requests (probe, retry) are minted automatically. A live request's state is never minted automatically: the injection rule either calls it degraded or judges nothing, so it is recorded and waits for a manual pool.
_Avoid_: Observing, receiving

**Injection rule**:
The live judgement (`judgeInjectedTurn`). A request that went out carrying an injected state and came back with no state in its response headers is non-degraded; one that came back with a state is degraded. Nothing injected means no verdict.
_Avoid_: Echo rule

**Multi-check**:
The probe's optional verification (`probe_verify`, `judgeProbeVerification`). After a probe obtains a state it sends one more minimal request carrying that state and its session (the cookie the cookie-source mode presented, updated by the response's Set-Cookie); the state pools only if the upstream writes no new state back. A failed check gives no verdict and does not pool. Off, the probe pools unjudged.
_Avoid_: Double probe, re-probe

**Probe session**:
The cookie a probe pools with its state: what its cookie-source mode presented, updated by the response's Set-Cookie. In the cookie-pool mode that is the drawn cookie, or only what the exit handed back when the pool had none usable.
_Avoid_: Primed cookie

**Credential-level cookie**:
The credential's direct jar: the cookie its live traffic presented, updated by what its responses set. Viewable in the State pool card; the probe's credential mode presents it.
_Avoid_: Credential jar mode

**Cookie pool**:
Per credential, the newest session cookie vouched for by a non-degraded verdict (a clean multi-check, or a live turn that held), keyed by its `__oailb` routing target; a newer cookie for the same target replaces the older. Status follows the `__oailb` expiry. The probe's `cookie_pool` mode sends a random unexpired entry, or nothing.
_Avoid_: Cookie jar

**Manual pool**:
An operator minting a recorded live state from the history drawer. The plugin rebuilds the entry from the stored record (response state, request Cookie merged with Set-Cookie) without calling the upstream, and it takes the slot even from a newer state.
_Avoid_: Force pool, pin

**State pool**:
The durable set containing only the newest minted state for each credential and model. It stores the original state in the plugin bbolt database so CPA rebuilds and plugin updates do not empty the pool.
_Avoid_: Provenance table, recent states

**Degraded state**:
An observed state whose character length exceeds its credential plan limit. It remains visible in history but never enters the state pool.
_Avoid_: Invalid state

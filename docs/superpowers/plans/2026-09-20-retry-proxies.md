# Retry SOCKS proxies and disabled Header rules

> **For agentic workers:** Execute inline using executing-plans; the user requested direct implementation and release with limited checks.

**Goal:** Per-credential random SOCKS selection on every degraded retry; empty means direct; disabled Header rules cannot be edited.

**Architecture:** CPA HTTPRequest has no proxy override. Only background retries use a dedicated Go HTTP transport, with explicit no-environment-proxy semantics, timeout, cancellation and no redirects. Main requests and manual tests remain unchanged.

**Tech Stack:** Go net/http, embedded HTML/JS, existing Go and Playwright tests.

**Spec:** Latest user request in this session: multiple SOCKS proxies, random per retry, empty direct; grey disabled Header rules; publish after necessary checks.

## Constraints

- No CPA/global credential proxy mutation. Do not claim CPA wire fingerprint for background retries.
- Preserve unrelated files and untracked binary/preview artifacts.
- Reject malformed proxy configuration without echoing proxy credentials in errors.
- Keep response-side rejection independent of Header rule enablement.

## Task 1: Proxy configuration and retry transport

Files: model.go (retry_proxies JSON), headers.go (validation), retry_proxy.go (validation/selection/transport), degraded_retry.go (transport seam), retry_proxy_test.go and degraded_retry_test.go.

- [ ] Add failing validation roundtrip test using JSON retry_proxies and reject http:// / missing-port values; confirm failure.
- [ ] Add RetryProxies []string, trim/dedupe validation via parseRetryProxy(raw string), persist through existing rule store.
- [ ] Select under state lock in retryOnce, use retryHTTPDoFunc(context.Context, hostHTTPRequest, string). Empty string selects direct transport; nonempty selects SOCKS. No fallback; no redirect; close body unread; 30s timeout; cancellation on plugin shutdown.
- [ ] Verify direct requests ignore HTTP_PROXY, real fake-SOCKS handshake reaches a local fixture, invalid/proxy errors do not expose credentials, and existing retry pooling/quiesce behavior remains intact.

## Task 2: Configuration UI

Files: web/index.html and scripts/degraded-controls-test.mjs.

- [ ] Add failing browser check for disabled Header editor and retry proxy input.
- [ ] Use disabled fieldset for .rule-blocks, retain enable/save actions and saved values. Preserve existing styling and show explanation.
- [ ] Add retryProxies textarea, newline-separated URLs; wire load/reset/collect/autosave/rollback. Disable unless rejection and retry are both enabled.
- [ ] Run browser smoke at desktop/mobile sizes; check field persistence and rollback.

## Task 3: Release

Files: README.md, public plugin store registry in temporary clone.

- [ ] Document transport exception and proxy syntax; bump install examples to v0.14.0.
- [ ] Run normal and localtest Go suites and one browser smoke; inspect diff.
- [ ] Commit explicit files as release: v0.14.0, push main; wait for CI/release and verify artifacts.
- [ ] Update public store to 0.14.0 and verify remote entry. Report published version and transport caveat.

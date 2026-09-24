// Builds preview.html: the real panel with the management API replaced by
// fixture data, so the interface can be opened straight from the filesystem
// without a CPA, a credential or a database behind it.
//
// It is the shipped web/index.html verbatim plus one injected script. Nothing
// is stubbed inside the panel itself, so what the preview shows is what the
// plugin renders -- if a change breaks the preview it has broken the panel.
//
//   node scripts/make-preview.mjs
import fs from 'node:fs';

const panel = fs.readFileSync(new URL('../web/index.html', import.meta.url), 'utf8');

const now = Date.now();
const at = (secondsAgo) => new Date(now - secondsAgo * 1000).toISOString();

const requestBody = JSON.stringify({
  model: 'gpt-6-astra',
  instructions: 'You are Codex, a coding agent.',
  input: '[MASKED 412880 bytes]',
  tools: [{ type: 'function', name: 'shell' }, { type: 'function', name: 'apply_patch' }],
  reasoning: { effort: 'high', context: 'all_turns' },
  parallel_tool_calls: false,
  stream: true,
  store: false,
});

// Written the way a real stream arrived: the separators between event and data
// are gone, which is exactly the shape that used to read as no model at all.
const unframedResponseBody =
  'event: response.created' +
  'data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_68f2","object":"response","created_at":1790057487,"status":"in_progress","model":"gpt-6-astra","output":"[MASKED 2 bytes]","reasoning":{"context":"all_turns","effort":"low","summary":"detailed"},"service_tier":"auto","store":false,"temperature":1.0,"tools":"[MASKED 14822 bytes]"}}' +
  'event: response.in_progress' +
  'data: {"masked":"1184 bytes","type":"response.in_progress"}' +
  'event: response.output_text.delta' +
  'data: {"masked":"118 bytes","type":"response.output_text.delta"}' +
  'event: response.output_item.done' +
  'data: {"masked":"406 bytes","type":"response.output_item.done"}' +
  'event: response.completed' +
  'data: {"type":"response.completed","sequence_number":13,"response":{"id":"resp_68f2","object":"response","status":"completed","model":"gpt-6-astra","output":"[MASKED 2210 bytes]","service_tier":"default","usage":"[MASKED 2104 bytes]"}}';

const responseBody = [
  'event: response.created',
  'data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_68f2","object":"response","model":"gpt-6-astra","status":"in_progress","output":"[MASKED 2 bytes]"}}',
  '',
  'event: response.completed',
  'data: {"type":"response.completed","sequence_number":41,"response":{"id":"resp_68f2","object":"response","model":"gpt-6-astra","status":"completed","output":"[MASKED 2210 bytes]","usage":{"input_tokens":18422,"output_tokens":512}}}',
  '',
  'data: [DONE]',
  '',
].join('\n');

const teamState = 'gAAAAAB' + 'q'.repeat(324);
const degradedState = 'gAAAAAB' + 'q'.repeat(348);

// Cookies as the upstream really sets them: a routing JWT (unsigned here), a
// Cloudflare bot token with its embedded issue time, and the LB affinity cookie.
const b64url = (obj) => Buffer.from(JSON.stringify(obj)).toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
const nowSec = Math.floor(Date.now() / 1000);
const oailb = `${b64url({ alg: 'HS256', typ: 'JWT', kid: 'gw-2' })}.${b64url({ host: 'gw-iad-7.internal', iss: 'oai-lb', aud: 'chatgpt.com', iat: nowSec - 600, exp: nowSec + 6600 })}.${b64url({ sig: 'not-a-real-signature-just-bytes-for-preview' })}`;
const cfbm = `2d4f1c9a7e3b5d0f8a6c4e2b1d9f7a5c3e1b0d8f6a4c2e0b9d7f5a3c1e8b6d4f2-${nowSec - 300}-1.0.1.1-Q1o9u4Kz8mX2nP5vT7wY0aB3cD6eF9gH`;
const credentials = [
  { auth_index: 'acct-a', auth_id: 'auth-a', name: 'alex.json', label: 'alex@example.com', email: 'alex@example.com', provider: 'codex', type: 'codex', plan_type: 'team', plan_resolved: true },
  { auth_index: 'acct-b', auth_id: 'auth-b', name: 'sam.json', label: 'sam@example.com', email: 'sam@example.com', provider: 'codex', type: 'codex', plan_type: 'plus', plan_resolved: true },
];

const rules = {
  'acct-a': {
    auth_index: 'acct-a', enabled: true,
    set: { 'X-Codex-Beta-Features': 'remote_compaction_v2' },
    remove: ['X-Openai-Internal-Codex-Responses-Lite'],
    inject_turn_state: true, state_ttl_seconds: 200,
    reject_degraded_response: true, reject_degraded_models: ['gpt-6-astra'],
    retry_on_degraded: false, retry_attempts: 2,
    retry_proxies: ['socks5://127.0.0.1:1080'],
    probe_enabled: true, probe_verify: true,
    probe_models: ['gpt-6-astra', 'gpt-5.6-luna'],
    probe_proxies: ['socks5://user:secret@127.0.0.1:1080'],
    probe_cookie_ttl_seconds: 1800,
    probe_interval_seconds: 30,
    probe_window_start_minute: 60,
    probe_window_end_minute: 900,
    probe_resolve_exit_ip: true,
    updated_at: at(3600),
  },
  'acct-b': { auth_index: 'acct-b', enabled: false, set: {}, remove: [], inject_turn_state: false, state_ttl_seconds: 0, updated_at: at(86400) },
};

const liveHeadersBefore = {
  'Authorization': ['Bearer [REDACTED]'],
  'Chatgpt-Account-Id': ['1d2b0e1c-0000-4a6d-9a11-6f0b9f2a77c1'],
  'Content-Type': ['application/json'],
  'Cookie': [`__oailb=${oailb}; __cf_bm=${cfbm}; __cflb=02DiuFnsSsHWYH8WqVXbZzkQHoM3kAqeZi2kTEqx4y8; oai-did=demo-device`],
  'Originator': ['@agentclientprotocol/codex-acp'],
  'X-Codex-Turn-State': [teamState],
  'X-Openai-Internal-Codex-Responses-Lite': ['true'],
};
const liveHeadersAfter = {
  ...liveHeadersBefore,
  'X-Codex-Beta-Features': ['remote_compaction_v2'],
};
delete liveHeadersAfter['X-Openai-Internal-Codex-Responses-Lite'];

const record = (over) => ({
  auth_index: 'acct-a', auth_id: 'auth-a', credential_name: 'alex.json', credential_label: 'alex@example.com',
  attempt: 1, source_format: 'openai-response', stream: true, origin: 'live',
  model: 'gpt-6-astra', requested_model: 'gpt-6-astra', upstream_model: 'gpt-6-astra', model_mismatch: false,
  request_effort: 'high', upstream_effort: 'high',
  status_code: 200, outcome: 'succeeded',
  before_headers: liveHeadersBefore, after_headers: liveHeadersAfter,
  response_headers: { 'X-Codex-Turn-State': [teamState], 'X-Request-Id': ['req_9f2a'], 'Set-Cookie': [`__cf_bm=${cfbm}; path=/; expires=${new Date(Date.now() + 1800 * 1000).toUTCString()}; domain=.chatgpt.com; HttpOnly; Secure; SameSite=None`, `__cflb=02DiuFnsSsHWYH8WqVXbZzkQHoM3kAqeZi2kTEqx4y8; SameSite=None; Secure; path=/; expires=${new Date(Date.now() + 86400 * 1000).toUTCString()}; HttpOnly`, `__oailb=${oailb}; path=/; Max-Age=7200; SameSite=Lax; Secure`] },
  ...over,
});

const items = [
  record({ id: 'rec-1#1', request_id: 'rec-1', started_at: at(30), completed_at: at(26),
    cookie_injected: true,
    // Nothing was injected, so the live rule judged nothing: the state is
    // recorded and waits for a manual pool.
    turn_state_minted: { digest: 'a13f9c21b4e0', chars: 332, bytes: 249, version: 128, issued_at: at(28), fernet_like: true, decodable: true, plan_type: 'team', judgement: 'injected', manual_pool: true } }),
  // The healthy live turn: a state went out and the upstream wrote none back.
  record({ id: 'rec-1b#1', request_id: 'rec-1b', started_at: at(60), completed_at: at(55),
    turn_state_injected: true, turn_state_held: true,
    response_headers: { 'X-Request-Id': ['req_5c10'] } }),
  record({ id: 'rec-2#1', request_id: 'rec-2', started_at: at(180), completed_at: at(171),
    outcome: 'succeeded', turn_state_rejected: true, turn_state_injected: true,
    response_headers: { 'X-Codex-Turn-State': [degradedState], 'X-Request-Id': ['req_71bd'] },
    turn_state_minted: { digest: '7c02dd48fa91', chars: 356, bytes: 265, version: 128, issued_at: at(179), fernet_like: true, decodable: true, plan_type: 'team', judgement: 'injected', non_degraded: false, pooled: false },
    turn_state_invalidated: true,
    turn_state_echo: { digest: 'a13f9c21b4e0', chars: 332, bytes: 249, version: 128, issued_at: at(400), fernet_like: true, decodable: true },
    turn_state_origin_index: 'acct-a', turn_state_origin_label: 'alex@example.com', turn_state_origin_model: 'gpt-6-astra',
    turn_state_cross_account: false, turn_state_cross_model: false, turn_state_age_seconds: 400, turn_state_expired: true }),
  { ...record({}), id: 'retry-1789952479137822421-6#1', request_id: 'retry-1789952479137822421-6', origin: 'retry',
    started_at: at(176), completed_at: at(174), retry_attempts: 2, source_format: 'plugin_retry',
    before_headers: { 'Accept': ['text/event-stream'], 'Authorization': ['Bearer [REDACTED]'], 'Chatgpt-Account-Id': ['1d2b0e1c-0000-4a6d-9a11-6f0b9f2a77c1'], 'Content-Type': ['application/json'], 'Cookie': ['__Secure-next-auth.session-token=demo-session-value; oai-did=demo-device'], 'Originator': ['codex-cli'] },
    after_headers: { 'Accept': ['text/event-stream'], 'Authorization': ['Bearer [REDACTED]'], 'Chatgpt-Account-Id': ['1d2b0e1c-0000-4a6d-9a11-6f0b9f2a77c1'], 'Content-Type': ['application/json'], 'Cookie': ['__Secure-next-auth.session-token=demo-session-value; oai-did=demo-device'], 'Originator': ['codex-cli'] },
    turn_state_minted: { digest: 'e4418b0c7d35', chars: 332, bytes: 249, version: 128, issued_at: at(175), fernet_like: true, decodable: true, plan_type: 'team', judgement: 'paused', pooled: true } },
  record({ id: 'rec-3#1', request_id: 'rec-3', started_at: at(900), completed_at: at(893),
    model: 'gpt-5.6-luna', requested_model: 'gpt-5.6-luna', upstream_model: 'gpt-5.6-sol',
    model_mismatch: true, request_effort: 'xhigh', upstream_effort: 'low' }),
  record({ id: 'rec-4#1', request_id: 'rec-4', started_at: at(1800), completed_at: at(1799),
    outcome: 'failed', status_code: 429, upstream_model: '', model_mismatch: null,
    error: '{"error":{"message":"Rate limit reached for gpt-6-astra","type":"rate_limit_error","code":"rate_limit_exceeded"}}' }),
];

const bodies = {
  'rec-1#1': { request_body: requestBody, request_bytes: 413021, response_body: responseBody, response_bytes: 2480 },
  'rec-2#1': { request_body: requestBody, request_bytes: 3324118, response_body: unframedResponseBody, response_bytes: 1960 },
  'retry-1789952479137822421-6#1': {
    request_body: JSON.stringify({ model: 'gpt-6-astra', instructions: 'You are Codex, a coding agent.', input: '[MASKED 74 bytes]', stream: true, store: false, tools: [], parallel_tool_calls: false }),
    request_bytes: 286,
    response_body: responseBody, response_bytes: 2312,
  },
};

const turnStates = [
  // Pooled by the probe: probe-1 returned it a few seconds ago.
  { state: teamState, digest: 'c91a0e77bb42', auth_index: 'acct-a', label: 'alex@example.com', model: 'gpt-6-astra', plan_type: 'team', chars: 332, max_chars: 0, minted_at: at(11), age_seconds: 11, expired: false, reuse_window_seconds: 200,
    cookie: `__oailb=${oailb}; __Secure-next-auth.session-token=demo-session-value; oai-did=demo-device` },
  // Pooled before the session was recorded, which reads as "no session" rather
  // than as an error.
  { state: teamState, digest: 'b7710f3e55aa', auth_index: 'acct-a', label: 'alex@example.com', model: 'gpt-5.6-luna', plan_type: 'team', chars: 332, max_chars: 0, minted_at: at(900), age_seconds: 900, expired: true, reuse_window_seconds: 200, cookie: '' },
];

// Probe rows: one succeeded and pooled, one primed through a rotating exit,
// one failed, so the list shows each shape it can take.
const probeRow = (over) => ({
  auth_index: 'acct-a', auth_id: 'auth-a', credential_name: 'alex.json', credential_label: 'alex@example.com',
  attempt: 1, source_format: 'plugin_probe', stream: true, origin: 'probe',
  model: 'gpt-6-astra', requested_model: 'gpt-6-astra', upstream_model: 'gpt-6-astra', model_mismatch: false,
  request_effort: 'low', upstream_effort: 'low', status_code: 200, outcome: 'succeeded',
  probe_exit_region: 'IAD',
  // Probes go out with no cookie; the session is what the response sets.
  before_headers: { 'Authorization': ['Bearer [REDACTED]'], 'Content-Type': ['application/json'], 'Originator': ['codex-cli'] },
  after_headers: { 'Authorization': ['Bearer [REDACTED]'], 'Content-Type': ['application/json'], 'Originator': ['codex-cli'] },
  response_headers: { 'X-Codex-Turn-State': [teamState], 'Cf-Ray': ['a3e22f4439f2dddf-IAD'], 'X-Codex-Primary-Used-Percent': ['47'],
    'Set-Cookie': [`__oailb=${oailb}; Path=/; Secure; HttpOnly`, `__cf_bm=for-this-exit; path=/; domain=.chatgpt.com; HttpOnly; Secure; SameSite=None`] },
  ...over,
});
const probes = [
  probeRow({ id: 'probe-1#1', request_id: 'probe-1', started_at: at(12), completed_at: at(10),
    probe_egress: 'socks5://127.0.0.1:1080', probe_verified: true, probe_verify_status: 200,
    // Multi-check: the second request carried this state and drew none back.
    turn_state_minted: { digest: 'c91a0e77bb42', chars: 332, bytes: 249, version: 128, issued_at: at(11), fernet_like: true, decodable: true, plan_type: 'team', judgement: 'verify', non_degraded: true, pooled: true } }),
  // The check request itself failed, so there is no verdict and no pooling.
  probeRow({ id: 'probe-1b#1', request_id: 'probe-1b', started_at: at(48), completed_at: at(33),
    probe_egress: 'socks5://127.0.0.1:1080', probe_verified: true, probe_verify_status: 502,
    probe_verify_error: 'verification returned HTTP 502',
    turn_state_minted: { digest: '88be41d0c7a2', chars: 332, bytes: 249, version: 128, issued_at: at(34), fernet_like: true, decodable: true, plan_type: 'team', judgement: 'verify', pooled: false } }),
  probeRow({ id: 'probe-2#1', request_id: 'probe-2', started_at: at(75), completed_at: at(73),
    model: 'gpt-5.6-luna', requested_model: 'gpt-5.6-luna', upstream_model: 'gpt-5.6-luna',
    probe_egress: '', probe_exit_region: 'LHR',
    turn_state_minted: { digest: '2f80ab19cc63', chars: 356, bytes: 265, version: 128, issued_at: at(74), fernet_like: true, decodable: true, plan_type: 'team', judgement: 'verify', non_degraded: false, pooled: false },
    probe_verified: true, probe_verify_status: 200 }),
  probeRow({ id: 'probe-3#1', request_id: 'probe-3', started_at: at(140), completed_at: at(139),
    // Written before probes stopped carrying a cookie.
    probe_egress: 'socks5://127.0.0.1:1080', probe_cookie_mode: 'rotating_proxy', probe_primed: true, outcome: 'failed', status_code: 429,
    upstream_model: '', model_mismatch: null, probe_exit_region: '',
    error: 'probe returned HTTP 429' }),
];

const fixtures = { credentials, rules, items, bodies, turnStates, teamState, probes, oailb, cfbm };

const stub = `
<script>
/* Preview only: the management API is answered from fixtures baked in at build
   time by scripts/make-preview.mjs. Nothing here runs in the shipped panel. */
(() => {
  const F = ${JSON.stringify(fixtures)};
  const json = (payload, status = 200) => new Response(JSON.stringify(payload), { status, headers: { "Content-Type": "application/json" } });
  // Opened from the filesystem, localStorage is unavailable in some browsers
  // and throws in others, so the panel found no key and asked for one. Give it
  // a working in-memory store instead of hoping the real one is there.
  const memory = new Map([["managementKey", "preview"]]);
  const shim = {
    getItem: (k) => (memory.has(String(k)) ? memory.get(String(k)) : null),
    setItem: (k, v) => { memory.set(String(k), String(v)); },
    removeItem: (k) => { memory.delete(String(k)); },
    clear: () => memory.clear(),
    key: (i) => [...memory.keys()][i] ?? null,
    get length() { return memory.size; },
  };
  for (const name of ["localStorage", "sessionStorage"]) {
    try { Object.defineProperty(window, name, { configurable: true, get: () => shim }); } catch {}
  }
  try { window.localStorage.setItem("managementKey", "preview"); } catch {}
  // The path is split by hand rather than with new URL(path, location.href):
  // resolving a root-relative path against a file: base is rejected outright by
  // some browsers, and the whole preview died on the first request with
  // "Failed to construct 'URL': Invalid URL". Nothing here needs a real URL.
  window.fetch = async (input, init = {}) => {
    const raw = String((input && input.url) || input || "");
    const mark = raw.indexOf("?");
    const path = (mark < 0 ? raw : raw.slice(0, mark)).split("#")[0];
    const q = new URLSearchParams(mark < 0 ? "" : raw.slice(mark + 1));
    const authIndex = q.get("auth_index") || "acct-a";
    const body = init.body ? JSON.parse(init.body) : null;
    if (path.endsWith("/credentials")) {
      return json({ credentials: F.credentials, test_defaults: { model: "gpt-6-astra", prompt: "hi", endpoint: "https://chatgpt.com/backend-api/codex/responses" } });
    }
    if (path.endsWith("/history/body")) {
      const found = F.bodies[q.get("id")];
      return json({ id: q.get("id"), found: Boolean(found), masked_request_fields: ["input"], masked_response_fields: ["output", "tools", "usage"],
        max_stored_bytes: 262144, request_body: "", response_body: "", request_bytes: 0, response_bytes: 0, ...(found || {}) });
    }
    if (path.endsWith("/history/clear")) return json({ cleared: authIndex });
    if (path.endsWith("/probes/clear")) return json({ cleared: authIndex });
    if (path.endsWith("/probes")) {
      const rows = authIndex === "acct-a" ? F.probes : [];
      return json({ auth_index: authIndex, page: 1, page_size: 10, total: rows.length, total_pages: 1,
        // The schedule follows the rule the preview has saved, as the plugin's does.
        items: rows, limit: 500, enabled: !!F.rules[authIndex]?.probe_enabled, within_window: true,
        pool_paused: F.rules[authIndex]?.maintain_state_pool === false,
        last_live_at: new Date(Date.now() - 42000).toISOString(), live_age_seconds: 42,
        next_due_at: new Date(Date.now() + 18000).toISOString() });
    }
    if (path.endsWith("/turn-state/pool")) {
      // The plugin rebuilds the entry from the stored record; the preview does
      // the same from its fixture and answers the way the route does.
      const record = F.items.find((item) => item.id === body?.id);
      if (!record) return json({ error: "history record not found" }, 404);
      if (F.rules[body.auth_index]?.maintain_state_pool === false) return json({ error: "state pool maintenance is off for this credential" }, 409);
      const minted = record.turn_state_minted || {};
      const issued = new Date(minted.issued_at || Date.now());
      const index = F.turnStates.findIndex((s) => s.auth_index === body.auth_index && s.model === record.model);
      const replacedNewer = index >= 0 && F.turnStates[index].digest !== minted.digest && new Date(F.turnStates[index].minted_at) > issued;
      const entry = { state: record.response_headers["X-Codex-Turn-State"][0], digest: minted.digest, auth_index: body.auth_index,
        label: record.credential_label, model: record.model, plan_type: minted.plan_type, chars: minted.chars, max_chars: 0,
        minted_at: issued.toISOString(), age_seconds: Math.round((Date.now() - issued) / 1000), expired: false, reuse_window_seconds: 200,
        cookie: (record.before_headers.Cookie || [""])[0] };
      if (index >= 0) F.turnStates[index] = entry; else F.turnStates.push(entry);
      record.turn_state_minted = { ...minted, pooled: true, manual_pool: true };
      return json({ info: record.turn_state_minted, replaced_newer: replacedNewer, model: record.model });
    }
    if (path.endsWith("/history")) {
      const items = authIndex === "acct-a" ? F.items : [];
      return json({ auth_index: authIndex, page: 1, page_size: 10, total: items.length, total_pages: 1, items });
    }
    if (path.endsWith("/session")) {
      return json(authIndex === "acct-a"
        ? { auth_index: authIndex, found: true, cookie: "__oailb=" + F.oailb + "; __cf_bm=" + F.cfbm + "; oai-did=demo-device",
            refreshed_at: new Date(Date.now() - 42000).toISOString(), last_live_at: new Date(Date.now() - 42000).toISOString() }
        : { auth_index: authIndex, found: false, cookie: "" });
    }
    if (path.endsWith("/quota")) {
      return json({ auth_index: authIndex, refreshed: q.get("refresh") === "1", quota: authIndex === "acct-a" ? {
        primary_used_percent: 47, primary_reset_at: new Date(Date.now() + 82 * 60000).toISOString(), primary_window_minutes: 300,
        secondary_used_percent: 15, secondary_reset_at: new Date(Date.now() + 3 * 86400000).toISOString(), secondary_window_minutes: 10080,
        observed_at: new Date(Date.now() - 42000).toISOString() } : null });
    }
    if (path.endsWith("/turn-states")) {
      return json({ turn_states: F.turnStates.filter((s) => s.auth_index === authIndex), reuse_window_seconds: F.rules[authIndex]?.state_ttl_seconds || 200 });
    }
    if (path.endsWith("/turn-state/decode")) {
      const token = (body && body.token) || "";
      return json({ info: { digest: "a13f9c21b4e0", chars: token.length, bytes: Math.max(0, Math.round((token.length * 3) / 4) - 8),
        version: 128, issued_at: new Date(Date.now() - 45000).toISOString(), fernet_like: true, decodable: true,
        plan_type: "team", judgement: "paused" },
        origin_label: "alex@example.com", origin_auth_index: "acct-a" });
    }
    if (path.endsWith("/rule")) {
      if ((init.method || "GET").toUpperCase() === "PUT") { F.rules[body.auth_index] = { ...body, updated_at: new Date().toISOString() }; }
      if ((init.method || "GET").toUpperCase() === "DELETE") { delete F.rules[authIndex]; return json({ deleted: authIndex }); }
      return json({ rule: F.rules[authIndex] || null });
    }
    if (path.endsWith("/test")) {
      return json({ outcome: "succeeded", status_code: 200, latency_ms: 812, upstream_model: "gpt-6-astra", model_mismatch: false,
        before_headers: F.items[0].before_headers, after_headers: F.items[0].after_headers, response_headers: F.items[0].response_headers });
    }
    if (path.includes("/models")) return json({ models: [{ id: "gpt-6-astra" }, { id: "gpt-5.6-luna" }, { id: "gpt-5.6-sol" }] });
    // Anything unmatched is answered empty rather than attempted for real: a
    // preview opened from the filesystem has nothing to reach.
    return json({});
  };
})();
</script>
`;

// Runs after the panel. The pre-script shim covers browsers that let a page
// replace window.localStorage; this covers the ones that do not, by taking over
// key resolution itself and restarting the load the panel had already abandoned.
// It also puts any error on screen: a preview that fails silently is a preview
// that cannot be reported.
const takeover = `
<script>
(() => {
  const show = (what) => {
    let bar = document.getElementById("previewError");
    if (!bar) {
      bar = document.createElement("div");
      bar.id = "previewError";
      bar.setAttribute("style", "position:fixed;left:0;right:0;bottom:0;z-index:9999;padding:9px 14px;"
        + "font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre-wrap;"
        + "background:#7f1d1d;color:#fff;max-height:40vh;overflow:auto");
      document.body.appendChild(bar);
    }
    bar.textContent = "preview.html 出错（把这段发给我）：\\n" + what;
  };
  addEventListener("error", (e) => show((e.message || "error") + "\\n" + (e.filename || "") + ":" + (e.lineno || 0)));
  addEventListener("unhandledrejection", (e) => show("unhandled rejection: " + ((e.reason && (e.reason.stack || e.reason.message)) || String(e.reason))));
  try {
    // hostKey is a top-level function declaration, so it is a property of the
    // global object and can be replaced from here.
    if (typeof hostKey === "function") window.hostKey = () => "preview";
    const overlay = document.getElementById("keyOverlay");
    if (overlay) overlay.classList.remove("open");
    if (typeof renderKeyChip === "function") renderKeyChip();
    if (typeof loadCredentials === "function") loadCredentials().catch((error) => show("loadCredentials: " + (error && error.message || error)));
  } catch (error) {
    show("takeover: " + (error && error.stack || error));
  }
})();
</script>
`;

const marker = '<script>\n"use strict";';
if (!panel.includes(marker)) throw new Error('panel script preamble not found; update the injection point');
let out = panel.replace(marker, stub.trim() + '\n' + marker);
if (!out.includes('</body>')) throw new Error('no </body> to append the takeover to');
out = out.replace('</body>', takeover.trim() + '\n</body>');
fs.writeFileSync(new URL('../preview.html', import.meta.url), out);
console.log(`preview.html written (${(out.length / 1024).toFixed(0)} KB)`);

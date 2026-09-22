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
  'data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_68f2","object":"response","created_at":1790057487,"status":"in_progress","model":"gpt-6-astra","output":"[MASKED 2 bytes]","reasoning":{"context":"all_turns","effort":"low","summary":"detailed"},"service_tier":"auto","store":false,"temperature":1.0}}' +
  'event: response.output_text.delta' +
  'data: {"type":"response.output_text.delta","content_index":0,"delta":"\u55e8","output_index":0,"sequence_number":4}' +
  'event: response.completed' +
  'data: {"type":"response.completed","sequence_number":13,"response":{"id":"resp_68f2","object":"response","status":"completed","model":"gpt-6-astra","output":"[MASKED 2210 bytes]","service_tier":"default","usage":{"input_tokens":21190,"output_tokens":11,"total_tokens":21201}}}';

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
    retry_on_degraded: true, retry_attempts: 2,
    retry_proxies: ['socks5://127.0.0.1:1080'],
    updated_at: at(3600),
  },
  'acct-b': { auth_index: 'acct-b', enabled: false, set: {}, remove: [], inject_turn_state: false, state_ttl_seconds: 0, updated_at: at(86400) },
};

const liveHeadersBefore = {
  'Authorization': ['Bearer [REDACTED]'],
  'Chatgpt-Account-Id': ['1d2b0e1c-0000-4a6d-9a11-6f0b9f2a77c1'],
  'Content-Type': ['application/json'],
  'Cookie': ['__Secure-next-auth.session-token=demo-session-value; oai-did=demo-device'],
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
  status_code: 200, outcome: 'succeeded',
  before_headers: liveHeadersBefore, after_headers: liveHeadersAfter,
  response_headers: { 'X-Codex-Turn-State': [teamState], 'X-Request-Id': ['req_9f2a'], 'Set-Cookie': ['oai-did=demo-device; Path=/; HttpOnly'] },
  ...over,
});

const items = [
  record({ id: 'rec-1#1', request_id: 'rec-1', started_at: at(30), completed_at: at(26),
    turn_state_injected: true,
    turn_state_minted: { digest: 'a13f9c21b4e0', chars: 332, bytes: 249, version: 128, issued_at: at(28), fernet_like: true, decodable: true, plan_type: 'team', max_chars: 332, non_degraded: true, pooled: true } }),
  record({ id: 'rec-2#1', request_id: 'rec-2', started_at: at(180), completed_at: at(171),
    outcome: 'succeeded', turn_state_rejected: true, turn_state_injected: true,
    response_headers: { 'X-Codex-Turn-State': [degradedState], 'X-Request-Id': ['req_71bd'] },
    turn_state_minted: { digest: '7c02dd48fa91', chars: 356, bytes: 265, version: 128, issued_at: at(179), fernet_like: true, decodable: true, plan_type: 'team', max_chars: 332, non_degraded: false, pooled: false },
    turn_state_echo: { digest: 'a13f9c21b4e0', chars: 332, bytes: 249, version: 128, issued_at: at(400), fernet_like: true, decodable: true },
    turn_state_origin_index: 'acct-a', turn_state_origin_label: 'alex@example.com', turn_state_origin_model: 'gpt-6-astra',
    turn_state_cross_account: false, turn_state_cross_model: false, turn_state_age_seconds: 400, turn_state_expired: true }),
  { ...record({}), id: 'retry-1789952479137822421-6#1', request_id: 'retry-1789952479137822421-6', origin: 'retry',
    started_at: at(176), completed_at: at(174), retry_attempts: 2, source_format: 'plugin_retry',
    before_headers: { 'Accept': ['text/event-stream'], 'Authorization': ['Bearer [REDACTED]'], 'Chatgpt-Account-Id': ['1d2b0e1c-0000-4a6d-9a11-6f0b9f2a77c1'], 'Content-Type': ['application/json'], 'Cookie': ['__Secure-next-auth.session-token=demo-session-value; oai-did=demo-device'], 'Originator': ['codex-cli'] },
    after_headers: { 'Accept': ['text/event-stream'], 'Authorization': ['Bearer [REDACTED]'], 'Chatgpt-Account-Id': ['1d2b0e1c-0000-4a6d-9a11-6f0b9f2a77c1'], 'Content-Type': ['application/json'], 'Cookie': ['__Secure-next-auth.session-token=demo-session-value; oai-did=demo-device'], 'Originator': ['codex-cli'] },
    turn_state_minted: { digest: 'e4418b0c7d35', chars: 332, bytes: 249, version: 128, issued_at: at(175), fernet_like: true, decodable: true, plan_type: 'team', max_chars: 332, non_degraded: true, pooled: true } },
  record({ id: 'rec-3#1', request_id: 'rec-3', started_at: at(900), completed_at: at(893),
    model: 'gpt-5.6-luna', requested_model: 'gpt-5.6-luna', upstream_model: 'gpt-5.6-sol', model_mismatch: true }),
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
  { state: teamState, digest: 'a13f9c21b4e0', auth_index: 'acct-a', label: 'alex@example.com', model: 'gpt-6-astra', plan_type: 'team', chars: 332, max_chars: 332, minted_at: at(45), age_seconds: 45, expired: false, reuse_window_seconds: 200 },
  { state: teamState, digest: 'b7710f3e55aa', auth_index: 'acct-a', label: 'alex@example.com', model: 'gpt-5.6-luna', plan_type: 'team', chars: 332, max_chars: 332, minted_at: at(900), age_seconds: 900, expired: true, reuse_window_seconds: 200 },
];

const fixtures = { credentials, rules, items, bodies, turnStates, teamState };

const stub = `
<script>
/* Preview only: the management API is answered from fixtures baked in at build
   time by scripts/make-preview.mjs. Nothing here runs in the shipped panel. */
(() => {
  const F = ${JSON.stringify(fixtures)};
  const json = (payload) => new Response(JSON.stringify(payload), { status: 200, headers: { "Content-Type": "application/json" } });
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
  const real = window.fetch;
  window.fetch = async (input, init = {}) => {
    const url = new URL(String(input && input.url ? input.url : input), location.href);
    const path = url.pathname;
    const q = url.searchParams;
    const authIndex = q.get("auth_index") || "acct-a";
    const body = init.body ? JSON.parse(init.body) : null;
    if (path.endsWith("/credentials")) {
      return json({ credentials: F.credentials, test_defaults: { model: "gpt-6-astra", prompt: "hi", endpoint: "https://chatgpt.com/backend-api/codex/responses" } });
    }
    if (path.endsWith("/history/body")) {
      const found = F.bodies[q.get("id")];
      return json({ id: q.get("id"), found: Boolean(found), masked_request_field: "input", masked_response_field: "output",
        max_stored_bytes: 262144, request_body: "", response_body: "", request_bytes: 0, response_bytes: 0, ...(found || {}) });
    }
    if (path.endsWith("/history/clear")) return json({ cleared: authIndex });
    if (path.endsWith("/history")) {
      const items = authIndex === "acct-a" ? F.items : [];
      return json({ auth_index: authIndex, page: 1, page_size: 10, total: items.length, total_pages: 1, items });
    }
    if (path.endsWith("/turn-states")) {
      return json({ turn_states: F.turnStates.filter((s) => s.auth_index === authIndex), reuse_window_seconds: F.rules[authIndex]?.state_ttl_seconds || 200 });
    }
    if (path.endsWith("/turn-state/decode")) {
      const token = (body && body.token) || "";
      return json({ info: { digest: "a13f9c21b4e0", chars: token.length, bytes: Math.max(0, Math.round((token.length * 3) / 4) - 8),
        version: 128, issued_at: new Date(Date.now() - 45000).toISOString(), fernet_like: true, decodable: true,
        plan_type: "team", max_chars: 332, non_degraded: token.length <= 332 },
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
    if (path.includes("/v0/management/")) return json({});
    return real(input, init);
  };
})();
</script>
`;

const marker = '<script>\n"use strict";';
if (!panel.includes(marker)) throw new Error('panel script preamble not found; update the injection point');
const out = panel.replace(marker, stub.trim() + '\n' + marker);
fs.writeFileSync(new URL('../preview.html', import.meta.url), out);
console.log(`preview.html written (${(out.length / 1024).toFixed(0)} KB)`);

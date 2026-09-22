//go:build localtest

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func probeRuleFixture() headerRule {
	return headerRule{
		AuthIndex: "idx-a", ProbeEnabled: true,
		ProbeModels: []string{"gpt-6-astra"}, ProbeCookieTTLSeconds: 1800,
		ProbeIntervalSeconds: 5, ProbeCookieMode: probeCookieCredential,
	}
}

// The window is minutes into a UTC day. A local working day routinely becomes
// a cross-midnight window once converted, so a start after an end has to mean
// "wraps", not "empty".
func TestProbeWindow(t *testing.T) {
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 9, 22, hour, minute, 0, 0, time.UTC)
	}
	cases := []struct {
		name       string
		start, end int
		when       time.Time
		want       bool
	}{
		{"unset is all day", 0, 0, at(3, 0), true},
		{"equal bounds are all day", 540, 540, at(3, 0), true},
		{"inside a normal window", 540, 1080, at(12, 0), true},
		{"at the start is inside", 540, 1080, at(9, 0), true},
		{"at the end is outside", 540, 1080, at(18, 0), false},
		{"before a normal window", 540, 1080, at(8, 59), false},
		{"after a normal window", 540, 1080, at(18, 1), false},
		{"inside a wrapping window, late", 1320, 120, at(23, 0), true},
		{"inside a wrapping window, early", 1320, 120, at(1, 0), true},
		{"outside a wrapping window", 1320, 120, at(12, 0), false},
		{"at the end of a wrapping window", 1320, 120, at(2, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := headerRule{ProbeWindowStartMinute: tc.start, ProbeWindowEndMinute: tc.end}
			if got := withinProbeWindow(rule, tc.when); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// Models the pool already holds a fresh state for are not probed. The rest are
// ordered by how badly they need one, because they run in sequence and the
// back of the queue waits.
func TestProbePendingModelsAndOrder(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	rule := probeRuleFixture()
	rule.ProbeModels = []string{"fresh", "stale", "never", " ", "stale"}
	state.mu.Lock()
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", StateTTLSeconds: 200}
	noteTurnStateMintLocked(fernetToken(0x80, time.Now().Add(-10*time.Second), 1), "idx-a", "A", "fresh", "team", "")
	noteTurnStateMintLocked(fernetToken(0x80, time.Now().Add(-9000*time.Second), 1), "idx-a", "A", "stale", "team", "")
	state.mu.Unlock()

	got, _ := probePendingModels("idx-a", rule)
	// never pooled first, then the oldest; fresh is not probed at all, blanks
	// and duplicates are dropped.
	if len(got) != 2 || got[0] != "never" || got[1] != "stale" {
		t.Fatalf("pending=%v", got)
	}
}

// The liveness gate is about the account, not the cookie: only real traffic
// sets it, so an idle account stops being probed.
func TestProbeLivenessGate(t *testing.T) {
	store := resetState(t)
	rule := probeRuleFixture()

	if _, live := probeCredentialLiveness("idx-a", rule); live {
		t.Fatal("a credential with no session has never been used")
	}
	if err := store.SaveSession(credentialSession{AuthIndex: "idx-a", Cookie: "a=1", LastLiveAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, live := probeCredentialLiveness("idx-a", rule); live {
		t.Fatal("two hours past a thirty minute window is not live")
	}
	if err := store.SaveSession(credentialSession{AuthIndex: "idx-a", Cookie: "a=1", LastLiveAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	session, live := probeCredentialLiveness("idx-a", rule)
	if !live || session.Cookie != "a=1" {
		t.Fatalf("live=%v cookie=%q", live, session.Cookie)
	}
}

// A probe request must never refresh the account's liveness clock: if it did,
// the probe would keep an abandoned account warm forever.
func TestProbeNeverRefreshesLiveness(t *testing.T) {
	store := resetState(t)
	before := time.Now().Add(-time.Minute).UTC()
	if err := store.SaveSession(credentialSession{AuthIndex: "idx-a", Cookie: "old=1", LastLiveAt: before}); err != nil {
		t.Fatal(err)
	}
	rememberSession("idx-a", "", probeCookieCredential, "new=2")
	session, _, _ := store.Session("idx-a", "")
	if !session.LastLiveAt.Equal(before) {
		t.Fatalf("liveness moved: %v -> %v", before, session.LastLiveAt)
	}
	if session.Cookie != "new=2" {
		t.Fatalf("cookie=%q", session.Cookie)
	}
}

// A jar is only written from a response that came back through the egress it
// is keyed to. A proxied probe touching the credential jar would put a cookie
// bound to the proxy's address where the direct traffic's cookie belongs.
func TestProxiedProbeNeverTouchesTheCredentialJar(t *testing.T) {
	store := resetState(t)
	if err := store.SaveSession(credentialSession{AuthIndex: "idx-a", Cookie: "direct=1", LastLiveAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// In credential mode a proxied response writes nothing at all: its cookie
	// belongs to the proxy's address, and the only jar this mode reads is the
	// direct one.
	rememberSession("idx-a", "socks5://127.0.0.1:1080", probeCookieCredential, "proxy=2")
	direct, _, _ := store.Session("idx-a", "")
	if direct.Cookie != "direct=1" {
		t.Fatalf("the credential jar was overwritten: %q", direct.Cookie)
	}
	if _, found, _ := store.Session("idx-a", "socks5://127.0.0.1:1080"); found {
		t.Fatal("credential mode should leave no proxy jar behind either")
	}
	// A static-proxy jar is keyed to its own egress and does get written.
	rememberSession("idx-a", "socks5://127.0.0.1:1080", probeCookieStaticProxy, "proxy=2")
	proxied, found, _ := store.Session("idx-a", "socks5://127.0.0.1:1080")
	if !found || proxied.Cookie != "proxy=2" {
		t.Fatalf("static proxy jar: found=%v cookie=%q", found, proxied.Cookie)
	}
	// A rotating egress stores nothing: no address it could be keyed to.
	rememberSession("idx-a", "socks5://127.0.0.1:2080", probeCookieRotatingProxy, "rotating=3")
	if _, found, _ := store.Session("idx-a", "socks5://127.0.0.1:2080"); found {
		t.Fatal("a rotating egress must not leave a jar behind")
	}
}

func TestProbeCookieModeFallsBackOnlyWithoutProxies(t *testing.T) {
	rule := probeRuleFixture()
	rule.ProbeCookieMode = ""
	if got := probeCookieModeFor(rule); got != probeCookieCredential {
		t.Fatalf("no proxies should mean the credential jar, got %q", got)
	}
	rule.ProbeProxies = []string{"socks5://127.0.0.1:1080"}
	rule.ProbeProxyEnabled = true
	rule.ProbeCookieMode = probeCookieRotatingProxy
	if got := probeCookieModeFor(rule); got != probeCookieRotatingProxy {
		t.Fatalf("got %q", got)
	}
}

// probeRows drains the writer the way lastAttempt does: the queue batches, so
// a test that reads straight from the store sees nothing yet.
func probeRows(t *testing.T) []historyRecord {
	t.Helper()
	state.mu.Lock()
	writer := state.writer
	state.writer = nil
	store := state.store
	state.mu.Unlock()
	if writer != nil {
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.ProbeHistory("idx-a", 1, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// probeStub answers every background request, recording what went out.
func probeStub(t *testing.T, blob string, setCookie []string) *[]hostHTTPRequest {
	t.Helper()
	oldGet, oldDo, oldRuntime := hostAuthGetFunc, probeHTTPDoFunc, hostAuthGetRuntimeFunc
	hostAuthGetRuntimeFunc = func(index string) (hostAuthGetRuntimeResponse, error) {
		return hostAuthGetRuntimeResponse{Auth: hostAuthFileEntry{ID: "auth-a", AuthIndex: index, Provider: "codex"}}, nil
	}
	var doc map[string]any
	_ = json.Unmarshal(credentialDocumentWithPlan(t, "team"), &doc)
	doc["access_token"] = "secret-token"
	doc["chatgpt_account_id"] = "acct-1"
	raw, _ := json.Marshal(doc)
	hostAuthGetFunc = func(string) (json.RawMessage, error) { return raw, nil }
	sent := []hostHTTPRequest{}
	probeHTTPDoFunc = func(_ context.Context, _ *http.Transport, req hostHTTPRequest, _ bool) (hostHTTPResponse, error) {
		sent = append(sent, req)
		headers := http.Header{turnStateHeader: {blob}, "Cf-Ray": {"a3e22f4439f2dddf-IAD"},
			"X-Codex-Primary-Used-Percent": {"47"}, "X-Codex-Primary-Window-Minutes": {"300"},
			"X-Codex-Secondary-Used-Percent": {"15"}, "X-Codex-Secondary-Reset-After-Seconds": {"3600"}}
		for _, value := range setCookie {
			headers.Add("Set-Cookie", value)
		}
		return hostHTTPResponse{StatusCode: 200, Headers: headers,
			Body: []byte(`{"type":"response.completed","response":{"model":"gpt-6-astra"}}`)}, nil
	}
	t.Cleanup(func() { hostAuthGetFunc, probeHTTPDoFunc, hostAuthGetRuntimeFunc = oldGet, oldDo, oldRuntime })
	return &sent
}

// End to end: a probe pools the state together with the session that produced
// it, and leaves one row saying how it went.
func TestProbePoolsTheStateWithItsSession(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now(), 1)
	sent := probeStub(t, blob, []string{"__cf_bm=fresh; Path=/"})
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()

	rule := probeRuleFixture()
	probeModel(context.Background(), "idx-a", "gpt-6-astra", rule, credentialSession{Cookie: "carried=1"})

	if len(*sent) != 1 {
		t.Fatalf("the credential mode sends one request, sent %d", len(*sent))
	}
	if got := (*sent)[0].Headers.Get("Cookie"); got != "carried=1" {
		t.Fatalf("the credential jar's cookie should have gone out, got %q", got)
	}
	state.mu.Lock()
	origin, pooled := state.turnStateLatest[turnStateLatestKey("idx-a", "gpt-6-astra")]
	state.mu.Unlock()
	if !pooled {
		t.Fatal("a non-degraded state should have been pooled")
	}
	if origin.cookie != "carried=1; __cf_bm=fresh" {
		t.Fatalf("the session pooled with it is wrong: %q", origin.cookie)
	}
	rows := probeRows(t)
	if len(rows) != 1 {
		t.Fatalf("one probe row expected, got %d", len(rows))
	}
	row := rows[0]
	if row.Origin != originProbe || row.Model != "gpt-6-astra" || row.Outcome != "succeeded" {
		t.Fatalf("row=%#v", row)
	}
	if row.ProbeExitRegion != "IAD" {
		t.Fatalf("the datacentre should come from Cf-Ray, got %q", row.ProbeExitRegion)
	}
	// The probe's own response is read like any other: the model it declares
	// and its effort land on the row, so the probe history reads like the rest.
	if row.UpstreamModel != "gpt-6-astra" || row.ModelMismatch == nil || *row.ModelMismatch {
		t.Fatalf("the row should carry the model the upstream declared: %#v", row)
	}
	// And the allowance the response reported is kept for the credential.
	if quota, ok := credentialQuotaFor("idx-a"); !ok || quota.PrimaryUsedPercent != 47 || quota.SecondaryUsedPercent != 15 || quota.SecondaryResetAt.IsZero() {
		t.Fatalf("the quota reading should be kept: %+v %v", quota, ok)
	}
	if row.ProbePrimed {
		t.Fatal("the credential mode does not prime")
	}
}

// A rotating pool cannot reuse a stored cookie, so it spends a request getting
// one for this connection and uses it on the same connection. The priming
// response's state is not the state this probe is for.
func TestRotatingProxyPrimesThenUses(t *testing.T) {
	store := resetState(t)
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now(), 1)
	sent := probeStub(t, blob, []string{"__cf_bm=for-this-exit; Path=/"})
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()

	rule := probeRuleFixture()
	rule.ProbeProxies = []string{"socks5://127.0.0.1:1080"}
	rule.ProbeProxyEnabled = true
	rule.ProbeCookieMode = probeCookieRotatingProxy
	probeModel(context.Background(), "idx-a", "gpt-6-astra", rule, credentialSession{Cookie: "not-this-one=1"})

	if len(*sent) != 2 {
		t.Fatalf("priming plus the probe is two requests, sent %d", len(*sent))
	}
	if got := (*sent)[0].Headers.Get("Cookie"); got != "" {
		t.Fatalf("the priming request carries no cookie, got %q", got)
	}
	if got := (*sent)[1].Headers.Get("Cookie"); got != "__cf_bm=for-this-exit" {
		t.Fatalf("the probe should use what priming obtained, got %q", got)
	}
	if _, found, _ := store.Session("idx-a", "socks5://127.0.0.1:1080"); found {
		t.Fatal("a rotating egress leaves no jar behind")
	}
	rows := probeRows(t)
	if len(rows) != 1 {
		t.Fatalf("priming is part of this probe, not a probe of its own: %d rows", len(rows))
	}
	if !rows[0].ProbePrimed {
		t.Fatal("the row should say it primed")
	}
}

// The due time says both "running" and "not yet due", so a scan can never
// start a second task for a credential that is already working.
func TestProbeScheduleIsReentrant(t *testing.T) {
	resetState(t)
	probeSchedule.Lock()
	probeSchedule.nextDue = map[string]time.Time{}
	probeSchedule.Unlock()
	state.mu.Lock()
	state.rules["idx-a"] = probeRuleFixture()
	state.mu.Unlock()

	now := time.Now()
	probeSchedule.Lock()
	first := probeDueLocked("idx-a", now)
	probeSchedule.nextDue["idx-a"] = now.Add(probeRunning)
	second := probeDueLocked("idx-a", now)
	probeSchedule.Unlock()
	if !first || second {
		t.Fatalf("first=%v second=%v", first, second)
	}
	setProbeDue("idx-a", now.Add(-time.Second))
	probeSchedule.Lock()
	third := probeDueLocked("idx-a", now)
	probeSchedule.Unlock()
	if !third {
		t.Fatal("a task that finished makes the credential due again")
	}
}

func TestCloudflareRegion(t *testing.T) {
	cases := map[string]string{
		"a3e22f4439f2dddf-IAD": "IAD",
		"abc-lhr":              "LHR",
		"no-dash":              "",
		"":                     "",
		"trailing-":            "",
		"abc-TOOLONG":          "",
	}
	for ray, want := range cases {
		headers := http.Header{}
		if ray != "" {
			headers.Set("Cf-Ray", ray)
		}
		if got := cloudflareRegion(headers); got != want {
			t.Fatalf("%q -> %q want %q", ray, got, want)
		}
	}
	if got := cloudflareRegion(http.Header{"cf-ray": {"x-sin"}}); got != "SIN" {
		t.Fatalf("header names fold: %q", got)
	}
	_ = strings.TrimSpace("")
}

// The probe fills the pool, so its gate is its own switch and the pool's
// master switch together; an unset master switch is on.
func TestProbeActiveFollowsThePoolSwitch(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name string
		rule headerRule
		want bool
	}{
		{"off by default", headerRule{}, false},
		{"switch on, pool unset", headerRule{ProbeEnabled: true}, true},
		{"switch on, pool on", headerRule{ProbeEnabled: true, MaintainStatePool: &on}, true},
		{"switch on, pool frozen", headerRule{ProbeEnabled: true, MaintainStatePool: &off}, false},
		{"switch off, pool on", headerRule{MaintainStatePool: &on}, false},
	}
	for _, tc := range cases {
		if got := probeActive(tc.rule); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// Outside the window the next look is the window's opening, not an interval
// that would tick towards nothing; a wrapped window opening later today and
// one that opened already both resolve to a moment after now.
func TestNextProbeWindowOpen(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	later := nextProbeWindowOpen(headerRule{ProbeWindowStartMinute: 22 * 60, ProbeWindowEndMinute: 2 * 60}, now)
	if !later.Equal(time.Date(2026, 9, 22, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("22:00 today, got %v", later)
	}
	earlier := nextProbeWindowOpen(headerRule{ProbeWindowStartMinute: 8 * 60, ProbeWindowEndMinute: 9 * 60}, now)
	if !earlier.Equal(time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("08:00 tomorrow, got %v", earlier)
	}
}

// A switched-off probe schedules nothing at all.
func TestSwitchedOffProbeSchedulesNothing(t *testing.T) {
	resetState(t)
	state.mu.Lock()
	state.rules["idx-off"] = headerRule{AuthIndex: "idx-off", ProbeEnabled: false, ProbeModels: []string{"m"}}
	state.mu.Unlock()
	setProbeDue("idx-off", time.Now().Add(time.Hour))
	runProbeTask("idx-off", make(chan struct{}))
	probeSchedule.Lock()
	_, scheduled := probeSchedule.nextDue["idx-off"]
	probeSchedule.Unlock()
	if scheduled {
		t.Fatal("an off probe must leave no next-due time behind")
	}
}

// A saved rule re-plans the probe: an idle credential is due on the next scan,
// a running one throws its own next-due write away and is due when it ends.
func TestRuleChangeReplansTheProbe(t *testing.T) {
	setProbeDue("idx-idle", time.Now().Add(time.Hour))
	rescheduleProbe("idx-idle")
	probeSchedule.Lock()
	_, idleStill := probeSchedule.nextDue["idx-idle"]
	probeSchedule.Unlock()
	if idleStill {
		t.Fatal("an idle credential must be due on the next scan after its rule changes")
	}
	setProbeDue("idx-busy", time.Now().Add(probeRunning))
	rescheduleProbe("idx-busy")
	setProbeDue("idx-busy", time.Now().Add(time.Hour)) // the task ending under the old plan
	probeSchedule.Lock()
	_, busyStill := probeSchedule.nextDue["idx-busy"]
	probeSchedule.Unlock()
	if busyStill {
		t.Fatal("a running credential must re-plan when its task ends, not keep the old plan")
	}
}

// A task that died mid-request leaves the running marker behind; once it is
// older than any task could be, the credential is due again.
func TestStaleRunningMarkerBecomesDue(t *testing.T) {
	now := time.Now()
	probeSchedule.Lock()
	defer probeSchedule.Unlock()
	if probeSchedule.nextDue == nil {
		probeSchedule.nextDue = map[string]time.Time{}
	}
	probeSchedule.nextDue["idx-live"] = now.Add(probeRunning)
	if probeDueLocked("idx-live", now.Add(probeTaskMaxAge/2)) {
		t.Fatal("a task within its allowed age is still running")
	}
	if !probeDueLocked("idx-live", now.Add(probeTaskMaxAge+time.Minute)) {
		t.Fatal("a marker older than the task limit must count as due")
	}
}

// The pool changing under the probe re-plans it: a mint or an eviction clears
// an idle credential's plan so the next scan looks again.
func TestPoolChangesReplanTheProbe(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	setProbeDue("idx-a", time.Now().Add(time.Hour))
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	pooled := noteTurnStateMintLocked(fernetToken(0x80, time.Now(), 1), "idx-a", "A", "gpt-5.6-luna", "team", "")
	state.mu.Unlock()
	if !pooled {
		t.Fatal("fixture state did not enter the pool")
	}
	probeSchedule.Lock()
	_, planned := probeSchedule.nextDue["idx-a"]
	probeSchedule.Unlock()
	if planned {
		t.Fatal("a mint must clear the old plan")
	}
}

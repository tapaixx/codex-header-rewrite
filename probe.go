package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// The probe keeps a credential's state pool warm on a timer instead of waiting
// for a degraded response to react to. It is the same request the retry makes
// -- one minimal "hi" under the credential and model -- with two differences
// that follow from it being scheduled rather than provoked:
//
//   - it only runs while the account is actually in use. A probe that kept an
//     abandoned account warm would burn quota forever on nobody's behalf, so a
//     live request must have happened within the configured window. Probe
//     requests deliberately do not refresh that clock; if they did, the probe
//     would keep itself alive.
//   - it chooses its own egress, which means it must also choose which cookie
//     jar to present. A Cloudflare bot token is bound to the address that
//     obtained it, so presenting one address's token from another is worse
//     than presenting none. probeCookieMode names which of the three coherent
//     answers applies to this deployment.
//
// One task per credential, and within a task the models run in sequence: they
// share one cookie jar, and parallel requests would race to decide which
// response's Set-Cookie wins.

// probeScanInterval is how often the scheduler looks for work, not how often a
// probe runs -- each credential carries its own due time. Scanning finely
// means a changed interval takes effect on the next second rather than when
// some timer built from the old value happens to fire.
const probeScanInterval = time.Second

// probeSchedule is the due time per credential. A task sets it far ahead while
// it runs, so the same field says both "already running" and "not yet due" --
// two states that can never disagree because they are one value.
var probeSchedule struct {
	sync.Mutex
	nextDue map[string]time.Time
	// rescan marks a credential whose rule changed while its task was running:
	// the task's own next-due write is discarded and the scan looks again.
	rescan map[string]bool
}

// probeRunning is far enough ahead that no scan will pick the credential up
// again, and is always replaced by a deferred write when the task ends.
const probeRunning = 365 * 24 * time.Hour

// probeRequestTimeout bounds one upstream call, and probeTaskMaxAge bounds a
// whole task: a request that never answers must not leave the credential
// marked as running forever, which would silence its probe for good.
const (
	probeRequestTimeout = 90 * time.Second
	probeTaskMaxAge     = 10 * time.Minute
)

var probeHTTPDoFunc = doHTTPOverTransport

func probeDueLocked(authIndex string, now time.Time) bool {
	if probeSchedule.nextDue == nil {
		probeSchedule.nextDue = map[string]time.Time{}
	}
	due, ok := probeSchedule.nextDue[authIndex]
	if !ok || !due.After(now) {
		return true
	}
	// The running marker is far in the future by construction; one that is
	// older than any task could legitimately be is a task that died.
	if due.After(now.Add(probeRunning/2)) && now.Sub(due.Add(-probeRunning)) > probeTaskMaxAge {
		return true
	}
	return false
}

// setProbeDue records when the credential is next looked at; a zero time
// means nothing is scheduled, which is what a switched-off probe shows.
func setProbeDue(authIndex string, at time.Time) {
	probeSchedule.Lock()
	defer probeSchedule.Unlock()
	if probeSchedule.nextDue == nil {
		probeSchedule.nextDue = map[string]time.Time{}
	}
	if probeSchedule.rescan[authIndex] {
		delete(probeSchedule.rescan, authIndex)
		at = time.Time{}
	}
	if at.IsZero() {
		delete(probeSchedule.nextDue, authIndex)
		return
	}
	probeSchedule.nextDue[authIndex] = at
}

// rescheduleProbe is called whenever a credential's rule is saved or removed:
// whatever was planned was planned under the old settings. An idle credential
// is looked at on the next scan; a running one re-plans as soon as it ends.
func rescheduleProbe(authIndex string) {
	probeSchedule.Lock()
	defer probeSchedule.Unlock()
	if probeSchedule.nextDue == nil {
		probeSchedule.nextDue = map[string]time.Time{}
	}
	if due, ok := probeSchedule.nextDue[authIndex]; ok && due.After(time.Now().Add(probeRunning/2)) {
		if probeSchedule.rescan == nil {
			probeSchedule.rescan = map[string]bool{}
		}
		probeSchedule.rescan[authIndex] = true
		return
	}
	delete(probeSchedule.nextDue, authIndex)
}

// nextProbeWindowOpen is the next UTC instant the window starts, for a task
// that found itself outside it: the countdown then says when the probe can
// run again rather than ticking towards a check that will do nothing.
func nextProbeWindowOpen(rule headerRule, now time.Time) time.Time {
	now = now.UTC()
	open := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(time.Duration(rule.ProbeWindowStartMinute) * time.Minute)
	if !open.After(now) {
		open = open.Add(24 * time.Hour)
	}
	return open
}

// startProbeScheduler runs until quiesce. It reads the rules the plugin
// already holds rather than asking the host for credentials every second: a
// credential whose probe is on necessarily has a saved rule.
func startProbeScheduler(stop <-chan struct{}) {
	state.retryWG.Add(1)
	go func() {
		defer state.retryWG.Done()
		ticker := time.NewTicker(probeScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				scanProbes(stop)
			}
		}
	}()
}

func scanProbes(stop <-chan struct{}) {
	now := time.Now()
	state.mu.Lock()
	if state.quiescing || state.store == nil {
		state.mu.Unlock()
		return
	}
	due := make([]string, 0, 4)
	probeSchedule.Lock()
	for authIndex, rule := range state.rules {
		if !probeActive(rule) || !probeDueLocked(authIndex, now) {
			continue
		}
		probeSchedule.nextDue[authIndex] = now.Add(probeRunning)
		due = append(due, authIndex)
	}
	probeSchedule.Unlock()
	state.mu.Unlock()

	for _, authIndex := range due {
		state.retryWG.Add(1)
		go func(index string) {
			defer state.retryWG.Done()
			runProbeTask(index, stop)
		}(authIndex)
	}
}

// runProbeTask is one task cycle. Whatever happens, the credential becomes due
// again one interval from now -- measured from the end, so the gap between
// upstream calls is the configured interval rather than something between zero
// and it.
// probeActive is the probe's whole gate: its own switch, and the pool it fills
// not being frozen. Both the scan and the task ask it, so they cannot drift.
func probeActive(rule headerRule) bool { return rule.ProbeEnabled && rule.poolMaintained() }

func runProbeTask(authIndex string, stop <-chan struct{}) {
	rule, ok := probeRule(authIndex)
	interval := probeInterval(rule)
	// What is scheduled is what will actually happen next: nothing while the
	// probe is off, the window's opening while outside it, the first expiry
	// while every state is fresh, and the interval only after real work.
	next := time.Time{}
	defer func() { setProbeDue(authIndex, next) }()
	if !ok || !probeActive(rule) {
		return
	}
	if now := time.Now().UTC(); !withinProbeWindow(rule, now) {
		next = nextProbeWindowOpen(rule, now)
		return
	}
	session, live := probeCredentialLiveness(authIndex, rule)
	if !live {
		next = time.Now().Add(interval)
		return
	}
	pending, soonest := probePendingModels(authIndex, rule)
	if len(pending) == 0 {
		next = soonest
		return
	}
	next = time.Now().Add(interval)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for _, model := range pending {
		select {
		case <-stop:
			return
		default:
		}
		modelCtx, cancelModel := context.WithTimeout(ctx, probeRequestTimeout)
		probeModel(modelCtx, authIndex, model, rule, session)
		cancelModel()
	}
}

func probeRule(authIndex string) (headerRule, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.quiescing || state.store == nil {
		return headerRule{}, false
	}
	rule, ok := state.rules[authIndex]
	return rule, ok
}

func probeInterval(rule headerRule) time.Duration {
	if rule.ProbeIntervalSeconds >= minProbeIntervalSec {
		return time.Duration(rule.ProbeIntervalSeconds) * time.Second
	}
	return defaultProbeIntervalSec * time.Second
}

// withinProbeWindow reads the window as minutes into a UTC day. Equal bounds
// mean all day; a start after an end crosses midnight, which is what a local
// working day usually becomes once it is expressed in UTC.
func withinProbeWindow(rule headerRule, now time.Time) bool {
	start, end := rule.ProbeWindowStartMinute, rule.ProbeWindowEndMinute
	if start == end {
		return true
	}
	minute := now.Hour()*60 + now.Minute()
	if start < end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}

// probeCredentialLiveness answers "is this account still in use", which is not
// the same question as "is this cookie fresh" and must not share a clock with
// it. Only real client traffic sets LastLiveAt.
func probeCredentialLiveness(authIndex string, rule headerRule) (credentialSession, bool) {
	state.mu.Lock()
	store := state.store
	state.mu.Unlock()
	if store == nil {
		return credentialSession{}, false
	}
	session, found, err := store.Session(authIndex, "")
	if err != nil || !found || session.LastLiveAt.IsZero() {
		return credentialSession{}, false
	}
	ttl := rule.ProbeCookieTTLSeconds
	if ttl < minProbeCookieTTLSec {
		return credentialSession{}, false
	}
	if time.Since(session.LastLiveAt) > time.Duration(ttl)*time.Second {
		return credentialSession{}, false
	}
	return session, true
}

// probePendingModels is the set of configured models whose pooled state is
// missing or past the credential's freshness window, ordered by how badly each
// needs one: never pooled first, then oldest. Sequential probing means the
// models at the back wait, so the back is where the least urgent belong.
// probePendingModels lists the probe models whose pooled state is missing or
// past the window, most urgent first, and alongside it the moment the first
// still-fresh state will expire -- when there is nothing to do now, that is
// when there next will be.
func probePendingModels(authIndex string, rule headerRule) ([]string, time.Time) {
	state.mu.Lock()
	defer state.mu.Unlock()
	window := turnStateReuseWindowLocked(authIndex)
	now := time.Now()
	var soonest time.Time
	type candidate struct {
		model  string
		minted time.Time
	}
	seen := make(map[string]struct{}, len(rule.ProbeModels))
	pending := make([]candidate, 0, len(rule.ProbeModels))
	for _, raw := range rule.ProbeModels {
		model := strings.TrimSpace(raw)
		if model == "" {
			continue
		}
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		origin, pooled := state.turnStateLatest[turnStateLatestKey(authIndex, model)]
		if pooled && now.Sub(origin.mintedAt) <= window {
			if expiry := origin.mintedAt.Add(window); soonest.IsZero() || expiry.Before(soonest) {
				soonest = expiry
			}
			continue
		}
		minted := time.Time{}
		if pooled {
			minted = origin.mintedAt
		}
		pending = append(pending, candidate{model: model, minted: minted})
	}
	sort.SliceStable(pending, func(i, j int) bool { return pending[i].minted.Before(pending[j].minted) })
	out := make([]string, 0, len(pending))
	for _, item := range pending {
		out = append(out, item.model)
	}
	return out, soonest
}

// probeEgress picks the exit for one request. Selection is per request, so a
// rotating pool spreads across its addresses rather than pinning to whichever
// one the task happened to start with.
func probeEgress(rule headerRule) string {
	pool := rule.probeProxyPool()
	if len(pool) == 0 {
		return ""
	}
	return pool[rand.IntN(len(pool))]
}

func probeCookieModeFor(rule headerRule) string {
	if len(rule.probeProxyPool()) == 0 {
		return probeCookieCredential
	}
	if validProbeCookieMode(rule.ProbeCookieMode) {
		return rule.ProbeCookieMode
	}
	return probeCookieCredential
}

// probeAttempt is what one model's probe did, kept together so the history row
// and the pooling decision read the same facts.
type probeAttempt struct {
	model    string
	egress   string
	cookie   string
	primed   bool
	started  time.Time
	sent     http.Header
	received http.Header
	status   int
	err      string
	blob     string
	pooled   bool
	info     turnStateInfo
	request  string
	response string
	reqBytes int
	resBytes int
	// What the upstream declared, read from the response body on the same
	// terms as a proxied response. With a priming request this is the real
	// probe's response -- the priming response is only read for Set-Cookie.
	upstreamModel  string
	upstreamEffort string
	modelConflict  bool
	requestEffort  string
}

// probeModel runs one model's probe: at most two requests over one transport,
// so that a rotating pool cannot change address between priming a cookie and
// using it. A SOCKS5 tunnel is one connection to one exit, so reuse is what
// makes the pair mean anything.
func probeModel(ctx context.Context, authIndex, model string, rule headerRule, session credentialSession) {
	attempt := probeAttempt{model: model, egress: probeEgress(rule), started: time.Now().UTC()}
	mode := probeCookieModeFor(rule)

	if !retryIdentityMatches(authIndex, credentialAuthID(authIndex)) {
		attempt.err = "credential identity changed or unavailable"
		recordProbe(authIndex, rule, attempt)
		return
	}
	document, err := hostAuthGetFunc(authIndex)
	if err != nil {
		attempt.err = "credential is not readable through the host"
		recordProbe(authIndex, rule, attempt)
		return
	}
	material := parseTestAuthMaterial(document)
	plan := credentialPlanType(document)
	if material.accessToken == "" || plan == "" {
		attempt.err = "credential has no usable access token or plan"
		recordProbe(authIndex, rule, attempt)
		return
	}

	transport, err := newBackgroundTransport(attempt.egress)
	if err != nil {
		attempt.err = err.Error()
		recordProbe(authIndex, rule, attempt)
		return
	}
	defer transport.CloseIdleConnections()

	cookie, primed, err := probeCookieFor(ctx, transport, authIndex, attempt.egress, mode, session, material)
	if err != nil {
		attempt.err = err.Error()
		attempt.primed = primed
		recordProbe(authIndex, rule, attempt)
		return
	}
	attempt.cookie, attempt.primed = cookie, primed

	body, marshalErr := probeRequestBody(model)
	if marshalErr != nil {
		attempt.err = marshalErr.Error()
		recordProbe(authIndex, rule, attempt)
		return
	}
	headers := retryHeaders(material, cookie)
	response, callErr := probeHTTPDoFunc(ctx, transport, hostHTTPRequest{
		Method: http.MethodPost, URL: defaultTestURL, Headers: headers, Body: body,
	}, attempt.egress != "")

	attempt.sent = redactHeaders(headers)
	attempt.received = redactHeaders(response.Headers)
	attempt.status = response.StatusCode
	attempt.request, _ = storedBody(maskRequestBody(body))
	attempt.reqBytes = len(body)
	attempt.response, _ = storedBody(maskResponseBody(response.Body))
	attempt.resBytes = len(response.Body)
	var observer modelObserver
	observer.observeBody(response.Body)
	attempt.upstreamModel, attempt.modelConflict, attempt.upstreamEffort = observer.model(), observer.conflicted(), observer.effort()
	attempt.requestEffort = requestThinkingLevel(body, "", model)

	if callErr != nil {
		attempt.err = callErr.Error()
		recordProbe(authIndex, rule, attempt)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		attempt.err = fmt.Sprintf("probe returned HTTP %d", response.StatusCode)
		recordProbe(authIndex, rule, attempt)
		return
	}

	// The session this response leaves behind belongs to the egress it came
	// back through, and nowhere else.
	rememberSession(authIndex, attempt.egress, mode, applySetCookies(cookie, response.Headers))
	noteQuota(authIndex, response.Headers)

	attempt.blob = headerTurnState(response.Headers)
	if attempt.blob == "" {
		attempt.err = fmt.Sprintf("no %s in the response (HTTP %d)", turnStateHeader, response.StatusCode)
		recordProbe(authIndex, rule, attempt)
		return
	}
	state.mu.Lock()
	info := classifyTurnState(decodeTurnState(attempt.blob), attempt.blob, plan)
	if judgeDegraded(attempt.blob, plan).Eligible {
		// The state and the session that produced it enter the pool together.
		info.Pooled = noteTurnStateMintLocked(attempt.blob, authIndex, credentialLabelLocked(authIndex), model, plan,
			applySetCookies(cookie, response.Headers))
	}
	state.mu.Unlock()
	attempt.info, attempt.pooled = info, info.Pooled
	recordProbe(authIndex, rule, attempt)
}

// probeCookieFor answers which cookie this request presents. The three modes
// are not preferences: each is the only correct answer for a different kind of
// egress.
func probeCookieFor(ctx context.Context, transport *http.Transport, authIndex, egress, mode string, session credentialSession, material testAuthMaterial) (string, bool, error) {
	switch mode {
	case probeCookieStaticProxy:
		// A stable proxy keeps its own jar, filled by the probes that went
		// through it. Empty until the first response comes back.
		stored, found, err := loadSession(authIndex, egress)
		if err != nil || !found {
			return "", false, nil
		}
		return stored.Cookie, false, nil
	case probeCookieRotatingProxy:
		// Nothing stored could be this connection's address, so the cookie is
		// obtained on this connection and used on it. The priming response's
		// state is discarded: a state fetched without a cookie is not the
		// state this probe is for.
		cookie, primed, err := primeCookie(ctx, transport, egress, material)
		return cookie, primed, err
	default:
		// The jar the live traffic fills. Correct when the probe leaves the
		// same way live traffic does.
		return session.Cookie, false, nil
	}
}

// primeCookie spends one request to obtain a cookie for this connection's
// exit. Its response is read only for Set-Cookie.
func primeCookie(ctx context.Context, transport *http.Transport, egress string, material testAuthMaterial) (string, bool, error) {
	body, err := probeRequestBody(probeWarmupModel)
	if err != nil {
		return "", true, err
	}
	response, callErr := probeHTTPDoFunc(ctx, transport, hostHTTPRequest{
		Method: http.MethodPost, URL: defaultTestURL, Headers: retryHeaders(material, ""), Body: body,
	}, egress != "")
	if callErr != nil {
		return "", true, callErr
	}
	return applySetCookies("", response.Headers), true, nil
}

// rememberSession writes a jar, and only the jar belonging to the egress the
// response came back through. A probe that went out through a proxy must never
// touch the credential jar that live traffic fills, and never touches
// LastLiveAt at all.
func rememberSession(authIndex, egress, mode, cookie string) {
	if cookie == "" || mode == probeCookieRotatingProxy {
		return
	}
	if mode == probeCookieCredential && egress != "" {
		// The response came back through a proxy; its cookie is bound to that
		// address and would poison the direct jar.
		return
	}
	state.mu.Lock()
	store := state.store
	state.mu.Unlock()
	if store == nil {
		return
	}
	existing, _, _ := store.Session(authIndex, egress)
	existing.AuthIndex, existing.Egress = authIndex, egress
	existing.Cookie, existing.RefreshAt = cookie, time.Now().UTC()
	_ = store.SaveSession(existing)
}

func loadSession(authIndex, egress string) (credentialSession, bool, error) {
	state.mu.Lock()
	store := state.store
	state.mu.Unlock()
	if store == nil {
		return credentialSession{}, false, nil
	}
	return store.Session(authIndex, egress)
}

func credentialAuthID(authIndex string) string {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.credentials[authIndex].AuthID
}

func credentialLabelLocked(authIndex string) string {
	cred := state.credentials[authIndex]
	if cred.Label != "" {
		return cred.Label
	}
	return cred.Name
}

// probeWarmupModel is what the priming request asks for. It only exists to
// collect a Set-Cookie, so it asks for the cheapest thing it can.
const probeWarmupModel = "gpt-5.6-luna"

func probeRequestBody(model string) ([]byte, error) {
	return json.Marshal(retryPayload(model))
}

var probeRecordSeq struct {
	sync.Mutex
	n int64
}

// recordProbe writes one row per model request. The priming request is part of
// the same probe, not a probe of its own, so it is reported as a flag on this
// row rather than as a second row -- "500 rows" has to mean "the last 500
// probes" for the list to be readable.
func recordProbe(authIndex string, rule headerRule, attempt probeAttempt) {
	state.mu.Lock()
	writer := state.writer
	store := state.store
	cred := state.credentials[authIndex]
	quiescing := state.quiescing
	state.mu.Unlock()
	if quiescing || store == nil {
		return
	}
	probeRecordSeq.Lock()
	probeRecordSeq.n++
	id := fmt.Sprintf("probe-%d-%d#1", attempt.started.UnixNano(), probeRecordSeq.n)
	probeRecordSeq.Unlock()

	outcome := "failed"
	if attempt.err == "" {
		outcome = "succeeded"
	}
	record := historyRecord{
		ID: id, RequestID: id, Attempt: 1,
		AuthIndex: authIndex, AuthID: cred.AuthID,
		CredentialName: cred.Name, CredentialLabel: cred.Label, CredentialPlan: cred.PlanType,
		Model: attempt.model, RequestedModel: attempt.model,
		SourceFormat: "plugin_probe", Stream: true, Origin: originProbe,
		StartedAt: attempt.started, CompletedAt: time.Now().UTC(),
		StatusCode: attempt.status, Outcome: outcome, Error: attempt.err,
		// The probe does not rewrite its own request, so both views of it are
		// the headers it sent.
		BeforeHeaders: attempt.sent, AfterHeaders: attempt.sent, ResponseHeaders: attempt.received,
		RequestBody: attempt.request, RequestBytes: attempt.reqBytes,
		ResponseBody: attempt.response, ResponseBytes: attempt.resBytes,
		ProbeEgress: attempt.egress, ProbePrimed: attempt.primed,
		ProbeCookieMode: probeCookieModeFor(rule),
		ProbeExitRegion: cloudflareRegion(attempt.received),
		UpstreamModel: attempt.upstreamModel, UpstreamEffort: attempt.upstreamEffort,
		ModelConflict: attempt.modelConflict, RequestEffort: attempt.requestEffort,
	}
	record.ModelMismatch = modelMismatch(attempt.model, attempt.upstreamModel)
	if attempt.info.Digest != "" {
		info := attempt.info
		record.TurnStateMinted = &info
	}
	if writer != nil {
		writer.EnqueueProbe(record)
		return
	}
	_ = store.AppendProbeHistory(record)
}

// cloudflareRegion reads the datacentre a response entered through, which is
// the trailing IATA code of Cf-Ray. It costs nothing, arrives on every
// response, and is accurate for that request -- unlike an exit address
// resolved separately, which a rotating pool makes a guess.
func cloudflareRegion(headers http.Header) string {
	ray := headerValueFold(headers, "Cf-Ray")
	index := strings.LastIndex(ray, "-")
	if index < 0 || index == len(ray)-1 {
		return ""
	}
	code := strings.ToUpper(strings.TrimSpace(ray[index+1:]))
	if len(code) != 3 {
		return ""
	}
	return code
}

// rememberLiveSessionLocked records what real client traffic leaves behind:
// the session as of this response, and the time the request started. That time
// is the probe's licence to run -- it says the account is in use -- so only
// live traffic may set it. Callers hold state.mu.
func rememberLiveSessionLocked(attempt *pendingAttempt, session string) {
	if state.store == nil || attempt.AuthIndex == "" {
		return
	}
	record, _, _ := state.store.Session(attempt.AuthIndex, "")
	record.AuthIndex, record.Egress = attempt.AuthIndex, ""
	record.LastLiveAt = attempt.StartedAt
	if session != "" {
		record.Cookie, record.RefreshAt = session, time.Now().UTC()
	}
	_ = state.store.SaveSession(record)
}

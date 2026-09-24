package main

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

var tokenRE = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

// Cookie and Set-Cookie are deliberately absent: the operator asked to read
// them, because comparing the session a retry presents against the one the
// live request carried is the only way to tell the two apart. They are
// recorded verbatim, which means a copy of the database carries usable
// sessions -- that is the trade this makes. Bearer tokens and API keys are
// still redacted.
var exactSensitiveHeaders = map[string]struct{}{
	"authorization": {}, "proxy-authorization": {},
	"x-api-key": {}, "api-key": {}, "x-goog-api-key": {}, "x-auth-token": {},
}
var sensitiveHeaderFragments = []string{"token", "secret", "password", "credential", "api-key", "apikey"}

// schemeHeaders are the sensitive headers whose value begins with an
// authentication scheme rather than with the secret itself.
var schemeHeaders = map[string]struct{}{"authorization": {}, "proxy-authorization": {}}
var warningHeaders = map[string]struct{}{
	"authorization": {}, "content-type": {}, "content-length": {}, "host": {},
	"user-agent": {}, "chatgpt-account-id": {}, "originator": {}, "openai-beta": {},
}

func validateRule(rule headerRule) (headerRule, error) {
	rule.AuthIndex = strings.TrimSpace(rule.AuthIndex)
	if rule.AuthIndex == "" {
		return rule, fmt.Errorf("auth_index is required")
	}
	cleanSet := make(map[string]string, len(rule.Set))
	seenSet := make(map[string]struct{}, len(rule.Set))
	for rawKey, value := range rule.Set {
		key := http.CanonicalHeaderKey(strings.TrimSpace(rawKey))
		if key == "" || !tokenRE.MatchString(key) {
			return rule, fmt.Errorf("invalid header name %q", rawKey)
		}
		if strings.ContainsAny(value, "\r\n") {
			return rule, fmt.Errorf("header %s contains CR/LF", key)
		}
		lower := strings.ToLower(key)
		cleanSet[key] = value
		seenSet[lower] = struct{}{}
	}
	cleanRemove := normalizedRemove(rule.Remove)
	for _, key := range cleanRemove {
		if !tokenRE.MatchString(key) {
			return rule, fmt.Errorf("invalid header name %q", key)
		}
		if _, exists := seenSet[strings.ToLower(key)]; exists {
			return rule, fmt.Errorf("header %s cannot be set and removed at the same time", key)
		}
	}
	rule.Set = cleanSet
	rule.Remove = cleanRemove
	// A rule saved before the switch meant "inject" carries its value under the
	// old name; fold it in once and stop writing the old key.
	if rule.LegacyGuard {
		rule.InjectTurnState = true
		rule.LegacyGuard = false
	}
	models := make([]string, 0, len(rule.RejectDegradedModels))
	seenModels := map[string]bool{}
	for _, model := range rule.RejectDegradedModels {
		model = strings.TrimSpace(model)
		if model != "" && !seenModels[model] {
			models = append(models, model)
			seenModels[model] = true
		}
	}
	rule.RejectDegradedModels = models
	proxies := make([]string, 0, len(rule.RetryProxies))
	seenProxies := map[string]bool{}
	for i, raw := range rule.RetryProxies {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		proxyURL, err := parseRetryProxy(raw)
		if err != nil {
			return rule, fmt.Errorf("retry_proxies[%d]: %w", i+1, err)
		}
		value := proxyURL.String()
		if !seenProxies[value] {
			proxies = append(proxies, value)
			seenProxies[value] = true
		}
	}
	rule.RetryProxies = proxies
	if rule.RetryAttempts < 0 {
		rule.RetryAttempts = 0
	}
	if rule.RetryAttempts > maxRetryAttempts {
		return rule, fmt.Errorf("retry_attempts must not exceed %d", maxRetryAttempts)
	}
	// Zero is how the field says "use the default", so it is kept rather than
	// clamped up. A number outside the range is refused instead of silently
	// rewritten: the operator is tuning a window, and a saved value that is not
	// the one they typed would mislead them about what the pool is doing.
	if rule.StateTTLSeconds < 0 || (rule.StateTTLSeconds > 0 && rule.StateTTLSeconds < minStateTTLSeconds) {
		return rule, fmt.Errorf("state_ttl_seconds must be at least %d", minStateTTLSeconds)
	}
	if rule.StateTTLSeconds > maxStateTTLSeconds {
		return rule, fmt.Errorf("state_ttl_seconds must not exceed %d", maxStateTTLSeconds)
	}

	rule.ProbeModels = dedupeTrimmed(rule.ProbeModels)
	probeProxies, err := normalizeProxyList("probe_proxies", rule.ProbeProxies)
	if err != nil {
		return rule, err
	}
	rule.ProbeProxies = probeProxies
	rule.ProbeCookieMode = strings.TrimSpace(rule.ProbeCookieMode)
	for name, minute := range map[string]int{
		"probe_window_start_minute": rule.ProbeWindowStartMinute,
		"probe_window_end_minute":   rule.ProbeWindowEndMinute,
	} {
		if minute < 0 || minute >= minutesPerDay {
			return rule, fmt.Errorf("%s must be between 0 and %d", name, minutesPerDay-1)
		}
	}
	if rule.ProbeCookieMode != "" && !validProbeCookieMode(rule.ProbeCookieMode) {
		return rule, fmt.Errorf("probe_cookie_mode must be one of %s, %s, %s, %s",
			probeCookieCredential, probeCookieStaticProxy, probeCookieRotatingProxy, probeCookiePool)
	}
	// A frozen pool has nothing for the retry or the probe to fill: both are
	// switched off with it, so the stored rule says what actually runs.
	if !rule.poolMaintained() {
		rule.RetryOnDegraded = false
		rule.ProbeEnabled = false
	}
	if rule.ProbeEnabled {
		// Both fetch states for the same credential on their own schedule.
		// Running them together would have two writers competing for one pool.
		if rule.RetryOnDegraded {
			return rule, fmt.Errorf("probe_enabled and retry_on_degraded cannot both be on")
		}
		if len(rule.ProbeModels) == 0 {
			return rule, fmt.Errorf("probe_models is required while the probe is on")
		}
		if rule.ProbeCookieTTLSeconds < minProbeCookieTTLSec {
			return rule, fmt.Errorf("probe_cookie_ttl_seconds must be at least %d", minProbeCookieTTLSec)
		}
		if rule.ProbeIntervalSeconds < minProbeIntervalSec {
			return rule, fmt.Errorf("probe_interval_seconds must be at least %d", minProbeIntervalSec)
		}
		// With no proxy the egress is the host's own, so the jar the live
		// traffic fills is the only coherent choice and is not worth asking
		// about. With proxies there is no safe default: each mode is correct
		// for a different kind of egress and the wrong one sends a cookie
		// bound to one address out through another.
		// The cookie pool works for any egress, so it is kept either way;
		// without a proxy the two jar-per-proxy modes fall back to the
		// credential jar.
		if len(rule.probeProxyPool()) == 0 {
			if rule.ProbeCookieMode != probeCookiePool {
				rule.ProbeCookieMode = probeCookieCredential
			}
		} else if rule.ProbeCookieMode == "" {
			return rule, fmt.Errorf("probe_cookie_mode must be chosen once the probe proxy list is switched on")
		}
	}
	return rule, nil
}

func dedupeTrimmed(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func normalizeProxyList(field string, values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for i, raw := range values {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		proxyURL, err := parseRetryProxy(raw)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", field, i+1, err)
		}
		value := proxyURL.String()
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out, nil
}

func rejectsDegradedModel(rule headerRule, model string) bool {
	if !rule.RejectDegradedResponse {
		return false
	}
	if len(rule.RejectDegradedModels) == 0 {
		return true
	}
	for _, configured := range rule.RejectDegradedModels {
		if strings.TrimSpace(configured) == strings.TrimSpace(model) {
			return true
		}
	}
	return false
}

func isSensitiveHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if _, ok := exactSensitiveHeaders[lower]; ok {
		return true
	}
	for _, fragment := range sensitiveHeaderFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func redactHeaderValue(name, value string) string {
	if !isSensitiveHeader(name) {
		return value
	}
	// Only an authorization value carries a scheme worth keeping: "Bearer
	// [REDACTED]" tells the operator more than "[REDACTED]" does. No other
	// sensitive header has one, and assuming they all did leaked material --
	// a Cookie's first crumb is itself a name=value pair, so keeping whatever
	// preceded the first space published one entire cookie.
	if _, ok := schemeHeaders[strings.ToLower(strings.TrimSpace(name))]; ok {
		if parts := strings.SplitN(strings.TrimSpace(value), " ", 2); len(parts) == 2 && parts[0] != "" {
			return parts[0] + " [REDACTED]"
		}
	}
	return "[REDACTED]"
}

func redactHeaders(in http.Header) http.Header {
	if in == nil {
		return nil
	}
	out := make(http.Header, len(in))
	for key, values := range in {
		dst := make([]string, len(values))
		for i, value := range values {
			dst[i] = redactHeaderValue(key, value)
		}
		out[key] = dst
	}
	return out
}

func specialHeaderWarnings(rule headerRule) []string {
	seen := map[string]struct{}{}
	for key := range rule.Set {
		if _, ok := warningHeaders[strings.ToLower(key)]; ok {
			seen[http.CanonicalHeaderKey(key)] = struct{}{}
		}
	}
	for _, key := range rule.Remove {
		if _, ok := warningHeaders[strings.ToLower(key)]; ok {
			seen[http.CanonicalHeaderKey(key)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

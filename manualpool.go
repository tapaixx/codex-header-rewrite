package main

import (
	"errors"
	"net/http"
	"strings"
)

// A live response's state is pooled by hand while the judgement is paused
// (see autoPoolsLive). The operator picks the row in the history drawer; the
// plugin rebuilds what the live path would have pooled from the stored record
// alone: the blob from the recorded response headers, the session from the
// recorded Cookie updated by the recorded Set-Cookie -- the same merge the
// live path applies. Nothing is re-requested from the upstream.

type manualPoolError struct {
	status  int
	message string
}

func (e *manualPoolError) Error() string { return e.message }

func manualPoolFailure(status int, message string) error {
	return &manualPoolError{status: status, message: message}
}

// manualPoolResult is what the panel needs to update the row and say what
// happened to the slot.
type manualPoolResult struct {
	Info turnStateInfo `json:"info"`
	// ReplacedNewer is set when the slot held a state issued after this one;
	// a manual pool takes the slot anyway.
	ReplacedNewer bool   `json:"replaced_newer,omitempty"`
	Model         string `json:"model"`
}

func poolFromHistory(authIndex, id string) (manualPoolResult, error) {
	authIndex, id = strings.TrimSpace(authIndex), strings.TrimSpace(id)
	if authIndex == "" || id == "" {
		return manualPoolResult{}, manualPoolFailure(http.StatusBadRequest, "auth_index and id are required")
	}
	state.mu.Lock()
	store := state.store
	rule, hasRule := state.rules[authIndex]
	cred := state.credentials[authIndex]
	state.mu.Unlock()
	if store == nil {
		return manualPoolResult{}, manualPoolFailure(http.StatusServiceUnavailable, "persistence is not initialized")
	}
	if hasRule && !rule.poolMaintained() {
		return manualPoolResult{}, manualPoolFailure(http.StatusConflict, "state pool maintenance is off for this credential")
	}
	page, err := store.History(authIndex, 1, historyLimit)
	if err != nil {
		return manualPoolResult{}, err
	}
	var record *historyRecord
	for i := range page.Items {
		if page.Items[i].ID == id {
			record = &page.Items[i]
			break
		}
	}
	if record == nil {
		return manualPoolResult{}, manualPoolFailure(http.StatusNotFound, "history record not found")
	}
	blob := headerTurnState(record.ResponseHeaders)
	if blob == "" {
		return manualPoolResult{}, manualPoolFailure(http.StatusUnprocessableEntity, "the response recorded no X-Codex-Turn-State")
	}
	digest := turnStateDigest(blob)
	if record.TurnStateMinted != nil && record.TurnStateMinted.Digest != "" && record.TurnStateMinted.Digest != digest {
		return manualPoolResult{}, manualPoolFailure(http.StatusUnprocessableEntity, "the recorded response state does not match the record")
	}
	plan := cred.PlanType
	if record.TurnStateMinted != nil && record.TurnStateMinted.PlanType != "" {
		plan = record.TurnStateMinted.PlanType
	}
	verdict := judgeDegraded(blob, plan)
	if !verdict.Eligible {
		return manualPoolResult{}, manualPoolFailure(http.StatusUnprocessableEntity, "the judgement in force keeps this state out of the pool")
	}
	label := record.CredentialLabel
	if label == "" {
		label = record.CredentialName
	}
	model := sentModel(record.Model, record.RequestedModel)
	session := applySetCookies(joinCookieHeader(record.BeforeHeaders), record.ResponseHeaders)
	info := classifyTurnState(decodeTurnState(blob), blob, plan)
	info.ManualPool = true

	state.mu.Lock()
	// Re-read under the lock the mint takes: the switch may have moved since.
	if current, ok := state.rules[authIndex]; ok && !current.poolMaintained() {
		state.mu.Unlock()
		return manualPoolResult{}, manualPoolFailure(http.StatusConflict, "state pool maintenance is off for this credential")
	}
	issued := info.IssuedAt
	previous, held := state.turnStateLatest[turnStateLatestKey(authIndex, strings.TrimSpace(model))]
	replacedNewer := held && previous.digest != digest && !issued.IsZero() && previous.mintedAt.After(issued)
	info.Pooled = mintTurnStateLocked(blob, authIndex, label, model, plan, session, mintManual)
	state.mu.Unlock()
	if !info.Pooled {
		return manualPoolResult{}, errors.New("the state could not be saved to the pool")
	}
	// The row says what happened. A failure here leaves the pool right and the
	// row stale, which the next pool listing corrects on screen.
	_, _ = store.UpdateHistory(authIndex, id, func(rec *historyRecord) {
		if rec.TurnStateMinted == nil {
			minted := info
			rec.TurnStateMinted = &minted
			return
		}
		rec.TurnStateMinted.Pooled, rec.TurnStateMinted.ManualPool = true, true
	})
	return manualPoolResult{Info: info, ReplacedNewer: replacedNewer, Model: model}, nil
}

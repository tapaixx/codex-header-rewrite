//go:build localtest

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type localDiskState struct {
	Rules      map[string]headerRule         `json:"rules"`
	History    map[string][]historyRecord    `json:"history"`
	Bodies     map[string]bodyRecord         `json:"bodies"`
	TurnStates map[string]persistedTurnState `json:"turn_states"`
}
type localPersistence struct {
	mu    sync.Mutex
	path  string
	state localDiskState
}

func openPersistence(path string) (persistence, error) {
	p := &localPersistence{path: path, state: localDiskState{Rules: map[string]headerRule{}, History: map[string][]historyRecord{}, Bodies: map[string]bodyRecord{}, TurnStates: map[string]persistedTurnState{}}}
	raw, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(raw, &p.state)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if p.state.Rules == nil {
		p.state.Rules = map[string]headerRule{}
	}
	if p.state.History == nil {
		p.state.History = map[string][]historyRecord{}
	}
	if p.state.Bodies == nil {
		p.state.Bodies = map[string]bodyRecord{}
	}
	if p.state.TurnStates == nil {
		p.state.TurnStates = map[string]persistedTurnState{}
	}
	return p, nil
}
func (p *localPersistence) save() error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.path, raw, 0600)
}
func (p *localPersistence) Close() error { p.mu.Lock(); defer p.mu.Unlock(); return p.save() }
func (p *localPersistence) Flush() error { p.mu.Lock(); defer p.mu.Unlock(); return p.save() }
func (p *localPersistence) SaveRule(r headerRule) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Rules[r.AuthIndex] = r
	return p.save()
}
func (p *localPersistence) DeleteRule(a string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.state.Rules, a)
	return p.save()
}
func (p *localPersistence) GetRule(a string) (headerRule, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.state.Rules[a]
	return r, ok, nil
}
func (p *localPersistence) ListRules() ([]headerRule, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]headerRule, 0, len(p.state.Rules))
	for _, r := range p.state.Rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthIndex < out[j].AuthIndex })
	return out, nil
}
func (p *localPersistence) SaveTurnState(r persistedTurnState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.TurnStates[turnStateLatestKey(r.AuthIndex, r.Model)] = r
	return p.save()
}
func (p *localPersistence) DeleteTurnState(authIndex, model string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.state.TurnStates, turnStateLatestKey(authIndex, model))
	return p.save()
}
func (p *localPersistence) ListTurnStates() ([]persistedTurnState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]persistedTurnState, 0, len(p.state.TurnStates))
	for _, r := range p.state.TurnStates {
		out = append(out, r)
	}
	return out, nil
}
func (p *localPersistence) AppendHistory(r historyRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	r.BeforeHeaders = redactHeaders(r.BeforeHeaders)
	r.AfterHeaders = redactHeaders(r.AfterHeaders)
	r.ResponseHeaders = redactHeaders(r.ResponseHeaders)
	if bodies := r.takeBodies(); !bodies.empty() {
		p.state.Bodies[bodyKey(r.AuthIndex, r.ID)] = bodies
	}
	items := append(p.state.History[r.AuthIndex], r)
	for len(items) > historyLimit {
		delete(p.state.Bodies, bodyKey(r.AuthIndex, items[0].ID))
		items = items[1:]
	}
	p.state.History[r.AuthIndex] = items
	return p.save()
}
func (p *localPersistence) History(a string, page int) (historyPage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	records := append([]historyRecord(nil), p.state.History[a]...)
	sortHistoryNewestFirst(records)
	return historyPageOf(a, page, records), nil
}
func (p *localPersistence) HistoryBody(a, id string) (bodyRecord, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	body, ok := p.state.Bodies[bodyKey(a, id)]
	return body, ok, nil
}

func bodyKey(authIndex, id string) string { return authIndex + "\x00" + id }

func (p *localPersistence) dropBodiesLocked(authIndex string) {
	for _, record := range p.state.History[authIndex] {
		delete(p.state.Bodies, bodyKey(authIndex, record.ID))
	}
}

func (p *localPersistence) ClearHistory(a string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropBodiesLocked(a)
	delete(p.state.History, a)
	return p.save()
}
func (p *localPersistence) HistoryCount(a string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.state.History[a]), nil
}
func (p *localPersistence) DeleteCredentialData(a string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.state.Rules, a)
	p.dropBodiesLocked(a)
	delete(p.state.History, a)
	for k, r := range p.state.TurnStates {
		if r.AuthIndex == a {
			delete(p.state.TurnStates, k)
		}
	}
	return p.save()
}
func (p *localPersistence) KnownAuthIndexes() ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]struct{}{}
	for k := range p.state.Rules {
		seen[k] = struct{}{}
	}
	for k := range p.state.History {
		seen[k] = struct{}{}
	}
	for _, r := range p.state.TurnStates {
		seen[r.AuthIndex] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

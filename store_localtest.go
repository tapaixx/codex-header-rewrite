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
	Rules   map[string]headerRule      `json:"rules"`
	History map[string][]historyRecord `json:"history"`
}
type localPersistence struct {
	mu    sync.Mutex
	path  string
	state localDiskState
}

func openPersistence(path string) (persistence, error) {
	p := &localPersistence{path: path, state: localDiskState{Rules: map[string]headerRule{}, History: map[string][]historyRecord{}}}
	raw, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(raw, &p.state)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if p.state.Rules == nil { p.state.Rules = map[string]headerRule{} }
	if p.state.History == nil { p.state.History = map[string][]historyRecord{} }
	return p, nil
}
func (p *localPersistence) save() error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0700); err != nil { return err }
	raw, err := json.MarshalIndent(p.state, "", "  ")
	if err != nil { return err }
	return os.WriteFile(p.path, raw, 0600)
}
func (p *localPersistence) Close() error { p.mu.Lock(); defer p.mu.Unlock(); return p.save() }
func (p *localPersistence) Flush() error { p.mu.Lock(); defer p.mu.Unlock(); return p.save() }
func (p *localPersistence) SaveRule(r headerRule) error { p.mu.Lock(); defer p.mu.Unlock(); p.state.Rules[r.AuthIndex]=r; return p.save() }
func (p *localPersistence) DeleteRule(a string) error { p.mu.Lock(); defer p.mu.Unlock(); delete(p.state.Rules,a); return p.save() }
func (p *localPersistence) GetRule(a string) (headerRule,bool,error) { p.mu.Lock(); defer p.mu.Unlock(); r,ok:=p.state.Rules[a]; return r,ok,nil }
func (p *localPersistence) ListRules() ([]headerRule,error) {
	p.mu.Lock(); defer p.mu.Unlock()
	out:=make([]headerRule,0,len(p.state.Rules)); for _,r:=range p.state.Rules{out=append(out,r)}
	sort.Slice(out,func(i,j int)bool{return out[i].AuthIndex<out[j].AuthIndex}); return out,nil
}
func (p *localPersistence) AppendHistory(r historyRecord) error {
	p.mu.Lock(); defer p.mu.Unlock()
	r.BeforeHeaders=redactHeaders(r.BeforeHeaders); r.AfterHeaders=redactHeaders(r.AfterHeaders); r.ResponseHeaders=redactHeaders(r.ResponseHeaders)
	items:=append(p.state.History[r.AuthIndex],r); if len(items)>historyLimit{items=items[len(items)-historyLimit:]}
	p.state.History[r.AuthIndex]=items; return p.save()
}
func (p *localPersistence) History(a string,page int)(historyPage,error){
	p.mu.Lock(); defer p.mu.Unlock(); if page<1{page=1}
	items:=p.state.History[a]; total:=len(items)
	out:=historyPage{AuthIndex:a,Page:page,PageSize:pageSize,Total:total,TotalPages:(total+pageSize-1)/pageSize,Items:[]historyRecord{}}
	start:=total-1-(page-1)*pageSize; for i:=start;i>=0&&len(out.Items)<pageSize;i--{out.Items=append(out.Items,items[i])}
	return out,nil
}
func (p *localPersistence) ClearHistory(a string) error { p.mu.Lock(); defer p.mu.Unlock(); delete(p.state.History,a); return p.save() }
func (p *localPersistence) HistoryCount(a string)(int,error){p.mu.Lock();defer p.mu.Unlock();return len(p.state.History[a]),nil}
func (p *localPersistence) DeleteCredentialData(a string) error {p.mu.Lock();defer p.mu.Unlock();delete(p.state.Rules,a);delete(p.state.History,a);return p.save()}
func (p *localPersistence) KnownAuthIndexes()([]string,error){
	p.mu.Lock();defer p.mu.Unlock();seen:=map[string]struct{}{};for k:=range p.state.Rules{seen[k]=struct{}{}};for k:=range p.state.History{seen[k]=struct{}{}}
	out:=make([]string,0,len(seen));for k:=range seen{out=append(out,k)};sort.Strings(out);return out,nil
}

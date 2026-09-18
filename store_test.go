//go:build localtest

package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistenceHistoryLimitPaginationRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")
	p, err := openPersistence(path); if err != nil { t.Fatal(err) }
	rule := headerRule{AuthIndex: "a", Enabled: true, Set: map[string]string{"X-Test": "v"}, UpdatedAt: time.Now().UTC()}
	if err := p.SaveRule(rule); err != nil { t.Fatal(err) }
	for i := 1; i <= 55; i++ {
		rec := historyRecord{ID: fmt.Sprintf("r-%02d", i), RequestID: fmt.Sprintf("req-%02d", i), AuthIndex: "a", Attempt: 1, StartedAt: time.Unix(int64(i), 0).UTC(), CompletedAt: time.Unix(int64(i+1), 0).UTC(), Outcome: "succeeded"}
		if err := p.AppendHistory(rec); err != nil { t.Fatal(err) }
	}
	page, err := p.History("a", 1); if err != nil { t.Fatal(err) }
	if page.Total != 50 || page.TotalPages != 5 || len(page.Items) != 10 || page.Items[0].ID != "r-55" || page.Items[9].ID != "r-46" { t.Fatalf("page=%+v", page) }
	if err := p.Close(); err != nil { t.Fatal(err) }
	p2, err := openPersistence(path); if err != nil { t.Fatal(err) }
	defer p2.Close()
	got, ok, err := p2.GetRule("a")
	if err != nil || !ok || got.Set["X-Test"] != "v" { t.Fatalf("restart rule=%#v ok=%v err=%v", got, ok, err) }
	count, _ := p2.HistoryCount("a"); if count != 50 { t.Fatalf("count=%d", count) }
}

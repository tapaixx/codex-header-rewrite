//go:build !localtest

package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// The bolt bucket is keyed by an insertion sequence, so reading it backwards
// used to decide the order of the list. It cannot: a record is written when the
// request finishes, and a retry series -- which the response path waits for --
// finishes before the request that started earlier and triggered it.
func TestBoltHistoryOrdersByStartTimeAcrossPages(t *testing.T) {
	p, err := openPersistence(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	at := func(second int) time.Time { return time.Unix(int64(second), 0).UTC() }
	// Insertion runs against start time, so every page boundary lands wrong
	// unless the bucket is ordered before it is cut into pages.
	const total = 25
	for i := 0; i < total; i++ {
		rec := historyRecord{
			ID: fmt.Sprintf("r-%02d", i), RequestID: fmt.Sprintf("r-%02d", i),
			AuthIndex: "a", Attempt: 1, Origin: originLive,
			StartedAt: at(total - i), CompletedAt: at(total + i),
		}
		if err := p.AppendHistory(rec); err != nil {
			t.Fatal(err)
		}
	}
	// One more that starts last and is written last, the shape a retry takes.
	if err := p.AppendHistory(historyRecord{
		ID: "retry", RequestID: "retry", AuthIndex: "a", Attempt: 1, Origin: originRetry,
		StartedAt: at(total + 1), CompletedAt: at(total + 2),
	}); err != nil {
		t.Fatal(err)
	}

	var seen []historyRecord
	for page := 1; page <= 3; page++ {
		got, err := p.History("a", page)
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != total+1 || got.TotalPages != 3 {
			t.Fatalf("page %d: total=%d pages=%d", page, got.Total, got.TotalPages)
		}
		seen = append(seen, got.Items...)
	}
	if len(seen) != total+1 {
		t.Fatalf("collected %d of %d records", len(seen), total+1)
	}
	if seen[0].ID != "retry" {
		t.Fatalf("the newest start is %s, not the retry", seen[0].ID)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].StartedAt.After(seen[i-1].StartedAt) {
			t.Fatalf("record %d (%s) starts after the one above it (%s)", i, seen[i].ID, seen[i-1].ID)
		}
	}
}

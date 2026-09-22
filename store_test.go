//go:build localtest

package main

import (
	"fmt"
	"path/filepath"
	"strings"
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
	page, err := p.History("a", 1, pageSize); if err != nil { t.Fatal(err) }
	wantPages := (historyLimit + pageSize - 1) / pageSize
	wantLast := fmt.Sprintf("r-%02d", 55-pageSize+1)
	if page.Total != historyLimit || page.TotalPages != wantPages || len(page.Items) != pageSize ||
		page.Items[0].ID != "r-55" || page.Items[len(page.Items)-1].ID != wantLast {
		t.Fatalf("page 1 is %s (total=%d pages=%d)", historyIDs(page), page.Total, page.TotalPages)
	}
	if err := p.Close(); err != nil { t.Fatal(err) }
	p2, err := openPersistence(path); if err != nil { t.Fatal(err) }
	defer p2.Close()
	got, ok, err := p2.GetRule("a")
	if err != nil || !ok || got.Set["X-Test"] != "v" { t.Fatalf("restart rule=%#v ok=%v err=%v", got, ok, err) }
	count, _ := p2.HistoryCount("a"); if count != 50 { t.Fatalf("count=%d", count) }
}

// The list is read newest first by the time a request started, but a record is
// only written once the request finishes. A retry series starts after the
// request that triggered it and still gets written first, because the response
// path waits for the series before the request can complete. Ordering by
// insertion put the older request on top of its own retry.
func TestHistoryOrdersByStartTimeNotByInsertion(t *testing.T) {
	p, err := openPersistence(filepath.Join(t.TempDir(), "order.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	at := func(second int) time.Time { return time.Unix(int64(second), 0).UTC() }
	for _, rec := range []historyRecord{
		{ID: "retry", AuthIndex: "a", Attempt: 1, Origin: originRetry, StartedAt: at(20), CompletedAt: at(25)},
		{ID: "live", AuthIndex: "a", Attempt: 1, Origin: originLive, StartedAt: at(10), CompletedAt: at(30)},
	} {
		if err := p.AppendHistory(rec); err != nil {
			t.Fatal(err)
		}
	}
	page, err := p.History("a", 1, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != "retry" || page.Items[1].ID != "live" {
		t.Fatalf("want retry then live, got %s", historyIDs(page))
	}
}

// Sorting one page at a time would leave the boundaries wrong, so the order has
// to be decided before the page is cut.
func TestHistoryOrdersAcrossPageBoundaries(t *testing.T) {
	p, err := openPersistence(filepath.Join(t.TempDir(), "pages.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Start times run backwards against insertion order, so every page boundary
	// is wrong unless the whole bucket is ordered first.
	// Enough to span three pages whatever the page size is, with the last one
	// short so the tail is exercised too.
	total := pageSize*2 + 5
	wantPages := 3
	for i := 0; i < total; i++ {
		rec := historyRecord{
			ID: fmt.Sprintf("r-%02d", i), AuthIndex: "a", Attempt: 1, Origin: originLive,
			StartedAt: time.Unix(int64(total-i), 0).UTC(), CompletedAt: time.Unix(int64(total+i), 0).UTC(),
		}
		if err := p.AppendHistory(rec); err != nil {
			t.Fatal(err)
		}
	}
	var seen []historyRecord
	for page := 1; page <= wantPages; page++ {
		got, err := p.History("a", page, pageSize)
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != total || got.TotalPages != wantPages {
			t.Fatalf("page %d: total=%d pages=%d", page, got.Total, got.TotalPages)
		}
		seen = append(seen, got.Items...)
	}
	if len(seen) != total {
		t.Fatalf("collected %d of %d records", len(seen), total)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].StartedAt.After(seen[i-1].StartedAt) {
			t.Fatalf("record %d (%s) starts after the one above it (%s)", i, seen[i].ID, seen[i-1].ID)
		}
	}
	if seen[0].ID != "r-00" || seen[total-1].ID != fmt.Sprintf("r-%02d", total-1) {
		t.Fatalf("ends are %s .. %s", seen[0].ID, seen[total-1].ID)
	}
}

func historyIDs(page historyPage) string {
	out := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, item.ID)
	}
	return strings.Join(out, ",")
}

// The panel offers three page lengths; anything else falls back to the
// default rather than letting a query size the page however it likes.
func TestHistoryPageSizeIsOneOfTheOffered(t *testing.T) {
	for raw, want := range map[string]int{"10": 10, "20": 20, "50": 50, "": pageSize, "7": pageSize, "500": pageSize, "x": pageSize} {
		if got := historyPageSize(raw); got != want {
			t.Errorf("%q: got %d want %d", raw, got, want)
		}
	}
}

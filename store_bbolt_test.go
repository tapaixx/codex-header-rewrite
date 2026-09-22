//go:build !localtest

package main

import (
	"fmt"
	"path/filepath"
	"strings"
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
	// Enough for three pages whatever the page size is: two full ones plus the
	// retry, so a boundary falls inside the run rather than at its edge.
	total := pageSize*2 + 4
	wantPages := 3
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
	for page := 1; page <= wantPages; page++ {
		got, err := p.History("a", page, pageSize)
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != total+1 || got.TotalPages != wantPages {
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

// Bodies are stored apart from the records so a page of the list never carries
// them, and they are pruned with the record they explain rather than outliving
// it in a bucket nothing bounds.
func TestBodiesAreStoredApartAndPrunedWithTheirRecord(t *testing.T) {
	p, err := openPersistence(filepath.Join(t.TempDir(), "bodies.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	write := func(i int) string {
		id := fmt.Sprintf("r-%02d", i)
		rec := historyRecord{
			ID: id, RequestID: id, AuthIndex: "a", Attempt: 1, Origin: originLive,
			StartedAt: time.Unix(int64(i), 0).UTC(), CompletedAt: time.Unix(int64(i+1), 0).UTC(),
			RequestBody: `{"model":"m","input":"[MASKED 9 bytes]"}`, RequestBytes: 4096,
			ResponseBody: `{"output":"[MASKED 7 bytes]"}`, ResponseBytes: 8192,
		}
		if err := p.AppendHistory(rec); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := write(1)

	// The list is the cheap view: it must not carry payloads at all.
	page, err := p.History("a", 1, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items=%d", len(page.Items))
	}
	if page.Items[0].RequestBody != "" || page.Items[0].ResponseBody != "" {
		t.Fatalf("the list carried payloads: %#v", page.Items[0])
	}
	if page.Items[0].RequestBytes != 0 || page.Items[0].ResponseBytes != 0 {
		t.Fatalf("the list carried payload sizes: %#v", page.Items[0])
	}

	body, found, err := p.HistoryBody("a", first)
	if err != nil || !found {
		t.Fatalf("body not found: %v %v", found, err)
	}
	if body.RequestBytes != 4096 || body.ResponseBytes != 8192 {
		t.Fatalf("sizes lost: %#v", body)
	}
	if !strings.Contains(body.RequestBody, "MASKED") || !strings.Contains(body.ResponseBody, "MASKED") {
		t.Fatalf("payloads lost: %#v", body)
	}

	// Fill past the limit; the first record and its body go together.
	for i := 2; i <= historyLimit+1; i++ {
		write(i)
	}
	if _, found, _ := p.HistoryBody("a", first); found {
		t.Fatal("the evicted record's body is still stored")
	}
	last := fmt.Sprintf("r-%02d", historyLimit+1)
	if _, found, _ := p.HistoryBody("a", last); !found {
		t.Fatal("a live record lost its body")
	}

	// Clearing takes the payloads with it.
	if err := p.ClearHistory("a"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := p.HistoryBody("a", last); found {
		t.Fatal("clearing the history left payloads behind")
	}
}

// A record written before bodies existed, or one that carried none, reads back
// as absent rather than as an error.
func TestAMissingBodyIsNotAnError(t *testing.T) {
	p, err := openPersistence(filepath.Join(t.TempDir(), "nobody.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.AppendHistory(historyRecord{ID: "r", RequestID: "r", AuthIndex: "a", Attempt: 1, StartedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	body, found, err := p.HistoryBody("a", "r")
	if err != nil || found || !body.empty() {
		t.Fatalf("found=%v err=%v body=%#v", found, err, body)
	}
	if _, found, err := p.HistoryBody("nosuch", "r"); err != nil || found {
		t.Fatalf("unknown credential: found=%v err=%v", found, err)
	}
}

// The bucket is capped at historyLimit, not historyLimit+1. Stats().KeyN does
// not count a Put made in the same write transaction, so the prune loop used
// to stop one short and the bucket settled one over the cap forever.
func TestTheHistoryBucketHonoursItsLimitExactly(t *testing.T) {
	p, err := openPersistence(filepath.Join(t.TempDir(), "limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i := 1; i <= historyLimit+5; i++ {
		id := fmt.Sprintf("r-%03d", i)
		if err := p.AppendHistory(historyRecord{ID: id, RequestID: id, AuthIndex: "a", Attempt: 1, StartedAt: time.Unix(int64(i), 0).UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	count, err := p.HistoryCount("a")
	if err != nil || count != historyLimit {
		t.Fatalf("count=%d want %d (err %v)", count, historyLimit, err)
	}
	page, err := p.History("a", 1, pageSize)
	if err != nil || page.Total != historyLimit {
		t.Fatalf("total=%d want %d (err %v)", page.Total, historyLimit, err)
	}
	// The newest survive and the oldest are the ones dropped.
	if page.Items[0].ID != fmt.Sprintf("r-%03d", historyLimit+5) {
		t.Fatalf("newest is %s", page.Items[0].ID)
	}
}

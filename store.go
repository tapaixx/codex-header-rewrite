package main

import (
	"strconv"
	"strings"
	"sort"
	"time"
)

// sortHistoryNewestFirst orders records the way the list reads them: by the
// time the request started, newest first.
//
// Storage order cannot stand in for that. A record is written when the request
// finishes, so concurrent requests land in completion order, and a retry
// series is written before the request that triggered it -- the response path
// waits for the series, so the series is always the one that finishes first
// even though it started later. The batching writer adds its own reordering.
//
// records must arrive oldest-inserted first, which is what both backends
// produce: reversing before a stable sort makes two records that share a start
// time fall back to newest-inserted.
func sortHistoryNewestFirst(records []historyRecord) {
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].StartedAt.After(records[j].StartedAt)
	})
}

// historyPageOf slices one page out of records already ordered newest first.
// historyPageSizes are the page lengths the panel offers; anything else asked
// for falls back to the default so a stray query cannot request the bucket.
var historyPageSizes = []int{10, 20, 50}

func historyPageSize(raw string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	for _, size := range historyPageSizes {
		if n == size {
			return size
		}
	}
	return pageSize
}

func historyPageOf(authIndex string, page, size int, records []historyRecord) historyPage {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = pageSize
	}
	total := len(records)
	result := historyPage{
		AuthIndex:  authIndex,
		Page:       page,
		PageSize:   size,
		Total:      total,
		TotalPages: (total + size - 1) / size,
		Items:      []historyRecord{},
	}
	start := (page - 1) * size
	if start >= total {
		return result
	}
	end := min(start+size, total)
	result.Items = append(result.Items, records[start:end]...)
	return result
}

type persistence interface {
	Close() error
	SaveRule(rule headerRule) error
	DeleteRule(authIndex string) error
	GetRule(authIndex string) (headerRule, bool, error)
	ListRules() ([]headerRule, error)
	AppendHistory(record historyRecord) error
	History(authIndex string, page, size int) (historyPage, error)
	// HistoryBody fetches one record's payloads, which are stored apart from
	// the record so a page of the list never carries them.
	HistoryBody(authIndex, id string) (bodyRecord, bool, error)
	// Probe history is kept apart from request history and bounded separately.
	// Sharing one bucket would let a probe running every few seconds evict the
	// real traffic within the hour, which is the only record of what the proxy
	// actually did.
	AppendProbeHistory(record historyRecord) error
	ProbeHistory(authIndex string, page, size int) (historyPage, error)
	ClearProbeHistory(authIndex string) error
	// SaveSession writes one egress's cookie jar; Session reads it back. Absent
	// is not an error: a jar that has never been filled is the normal state
	// before the first response comes back through that egress.
	SaveSession(session credentialSession) error
	Session(authIndex, egress string) (credentialSession, bool, error)
	ClearHistory(authIndex string) error
	HistoryCount(authIndex string) (int, error)
	DeleteCredentialData(authIndex string) error
	KnownAuthIndexes() ([]string, error)
	SaveTurnState(record persistedTurnState) error
	DeleteTurnState(authIndex, model string) error
	ListTurnStates() ([]persistedTurnState, error)
	Flush() error
}

type queuedPersistence struct {
	backend persistence
	queue   chan historyRecord
	stop    chan struct{}
	done    chan struct{}
}

func newQueuedPersistence(backend persistence) *queuedPersistence {
	q := &queuedPersistence{backend: backend, queue: make(chan historyRecord, 256), stop: make(chan struct{}), done: make(chan struct{})}
	go q.run()
	return q
}

func (q *queuedPersistence) run() {
	defer close(q.done)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]historyRecord, 0, 32)
	flush := func() {
		for _, record := range batch {
			if record.Origin == originProbe {
				_ = q.backend.AppendProbeHistory(record)
				continue
			}
			_ = q.backend.AppendHistory(record)
		}
		batch = batch[:0]
		_ = q.backend.Flush()
	}
	for {
		select {
		case record := <-q.queue:
			batch = append(batch, record)
			if len(batch) >= 32 {
				flush()
			}
		case <-ticker.C:
			if len(batch) > 0 {
				flush()
			}
		case <-q.stop:
			for {
				select {
				case record := <-q.queue:
					batch = append(batch, record)
				default:
					flush()
					return
				}
			}
		}
	}
}

// EnqueueProbe shares the queue but not the destination: probe rows are
// bounded separately, so the writer has to know which family a record belongs
// to. Origin carries that, so the queue stays one channel.
func (q *queuedPersistence) EnqueueProbe(record historyRecord) {
	record.Origin = originProbe
	q.Enqueue(record)
}

func (q *queuedPersistence) Enqueue(record historyRecord) {
	select {
	case q.queue <- record:
	default:
		_ = q.backend.AppendHistory(record)
	}
}

func (q *queuedPersistence) Close() error {
	close(q.stop)
	<-q.done
	return q.backend.Close()
}

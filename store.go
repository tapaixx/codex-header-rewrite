package main

import (
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
func historyPageOf(authIndex string, page int, records []historyRecord) historyPage {
	if page < 1 {
		page = 1
	}
	total := len(records)
	result := historyPage{
		AuthIndex:  authIndex,
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: (total + pageSize - 1) / pageSize,
		Items:      []historyRecord{},
	}
	start := (page - 1) * pageSize
	if start >= total {
		return result
	}
	end := min(start+pageSize, total)
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
	History(authIndex string, page int) (historyPage, error)
	// HistoryBody fetches one record's payloads, which are stored apart from
	// the record so a page of the list never carries them.
	HistoryBody(authIndex, id string) (bodyRecord, bool, error)
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

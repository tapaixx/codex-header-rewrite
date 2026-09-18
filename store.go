package main

import "time"

type persistence interface {
	Close() error
	SaveRule(rule headerRule) error
	DeleteRule(authIndex string) error
	GetRule(authIndex string) (headerRule, bool, error)
	ListRules() ([]headerRule, error)
	AppendHistory(record historyRecord) error
	History(authIndex string, page int) (historyPage, error)
	ClearHistory(authIndex string) error
	HistoryCount(authIndex string) (int, error)
	DeleteCredentialData(authIndex string) error
	KnownAuthIndexes() ([]string, error)
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

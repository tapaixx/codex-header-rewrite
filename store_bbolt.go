//go:build !localtest

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	bolt "go.etcd.io/bbolt"
)

var (
	rulesBucket      = []byte("rules")
	historyBucket    = []byte("history")
	turnStatesBucket = []byte("turn_states")
	// Bodies live apart from the records they belong to. A page of the history
	// decodes every record in the bucket to order it; carrying payloads there
	// would mean reading megabytes to render a list that shows none of them.
	historyBodiesBucket = []byte("history_bodies")
	// Probe rows are bounded separately from request rows: a probe that runs
	// every few seconds would otherwise evict the real traffic within the hour.
	// Bodies are shared, because record ids do not collide and each appender
	// prunes the ones it evicts.
	probeHistoryBucket = []byte("probe_history")
	// One cookie jar per (credential, egress), flat, because there are only
	// ever a handful and they are read one at a time.
	sessionsBucket = []byte("sessions")
)

func sessionKey(authIndex, egress string) []byte {
	return []byte(authIndex + "\x00" + egress)
}

type boltPersistence struct{ db *bolt.DB }

func openPersistence(path string) (persistence, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}
	p := &boltPersistence{db: db}
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(rulesBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(historyBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(historyBodiesBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(probeHistoryBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(sessionsBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(turnStatesBucket)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return p, nil
}

func (p *boltPersistence) Close() error { return p.db.Close() }
func (p *boltPersistence) Flush() error { return p.db.Sync() }

func (p *boltPersistence) SaveRule(rule headerRule) error {
	raw, err := json.Marshal(rule)
	if err != nil {
		return err
	}
	return p.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(rulesBucket).Put([]byte(rule.AuthIndex), raw) })
}
func (p *boltPersistence) DeleteRule(authIndex string) error {
	return p.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(rulesBucket).Delete([]byte(authIndex)) })
}
func (p *boltPersistence) GetRule(authIndex string) (headerRule, bool, error) {
	var out headerRule
	found := false
	err := p.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(rulesBucket).Get([]byte(authIndex))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(append([]byte(nil), raw...), &out)
	})
	return out, found, err
}
func (p *boltPersistence) ListRules() ([]headerRule, error) {
	out := []headerRule{}
	err := p.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(rulesBucket).ForEach(func(_, v []byte) error {
			if v == nil {
				return nil
			}
			var rule headerRule
			if err := json.Unmarshal(v, &rule); err != nil {
				return err
			}
			out = append(out, rule)
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].AuthIndex < out[j].AuthIndex })
	return out, err
}

func (p *boltPersistence) SaveTurnState(record persistedTurnState) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	key := []byte(turnStateLatestKey(record.AuthIndex, record.Model))
	return p.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(turnStatesBucket).Put(key, raw) })
}

func (p *boltPersistence) DeleteTurnState(authIndex, model string) error {
	key := []byte(turnStateLatestKey(authIndex, model))
	return p.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(turnStatesBucket).Delete(key) })
}

func (p *boltPersistence) ListTurnStates() ([]persistedTurnState, error) {
	out := []persistedTurnState{}
	err := p.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(turnStatesBucket).ForEach(func(_, value []byte) error {
			if value == nil {
				return nil
			}
			var record persistedTurnState
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			out = append(out, record)
			return nil
		})
	})
	return out, err
}

func historyChild(tx *bolt.Tx, authIndex string, create bool) (*bolt.Bucket, error) {
	return childBucket(tx, historyBucket, authIndex, create)
}

func probeHistoryChild(tx *bolt.Tx, authIndex string, create bool) (*bolt.Bucket, error) {
	return childBucket(tx, probeHistoryBucket, authIndex, create)
}

func bodiesChild(tx *bolt.Tx, authIndex string, create bool) (*bolt.Bucket, error) {
	return childBucket(tx, historyBodiesBucket, authIndex, create)
}

func childBucket(tx *bolt.Tx, name []byte, authIndex string, create bool) (*bolt.Bucket, error) {
	root := tx.Bucket(name)
	if root == nil {
		if !create {
			return nil, nil
		}
		created, err := tx.CreateBucketIfNotExists(name)
		if err != nil {
			return nil, err
		}
		root = created
	}
	if !create {
		return root.Bucket([]byte(authIndex)), nil
	}
	return root.CreateBucketIfNotExists([]byte(authIndex))
}

// childFunc selects which family of buckets a record family lives in, so the
// request history and the probe history share one appender and one prune loop
// while keeping separate limits.
type childFunc func(*bolt.Tx, string, bool) (*bolt.Bucket, error)

func (p *boltPersistence) AppendHistory(record historyRecord) error {
	return p.appendRecord(record, historyChild, historyLimit)
}

func (p *boltPersistence) AppendProbeHistory(record historyRecord) error {
	return p.appendRecord(record, probeHistoryChild, probeHistoryLimit)
}

func (p *boltPersistence) appendRecord(record historyRecord, child childFunc, limit int) error {
	record.BeforeHeaders = redactHeaders(record.BeforeHeaders)
	record.AfterHeaders = redactHeaders(record.AfterHeaders)
	record.ResponseHeaders = redactHeaders(record.ResponseHeaders)
	bodies := record.takeBodies()
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	var bodiesRaw []byte
	if !bodies.empty() {
		if bodiesRaw, err = json.Marshal(bodies); err != nil {
			return err
		}
	}
	return p.db.Update(func(tx *bolt.Tx) error {
		b, err := child(tx, record.AuthIndex, true)
		if err != nil {
			return err
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, seq)
		if err := b.Put(key, raw); err != nil {
			return err
		}
		if bodiesRaw != nil {
			payloads, errBodies := bodiesChild(tx, record.AuthIndex, true)
			if errBodies != nil {
				return errBodies
			}
			if err := payloads.Put([]byte(record.ID), bodiesRaw); err != nil {
				return err
			}
		}
		payloads, err := bodiesChild(tx, record.AuthIndex, false)
		if err != nil {
			return err
		}
		// Not Stats().KeyN: inside a write transaction it does not count the
		// Put just made, so the loop stopped one short and the bucket settled
		// at historyLimit+1 forever. A cursor sees pending writes.
		count := 0
		countCursor := b.Cursor()
		for k, _ := countCursor.First(); k != nil; k, _ = countCursor.Next() {
			count++
		}
		for count > limit {
			c := b.Cursor()
			k, v := c.First()
			if k == nil {
				break
			}
			// The body outlives nothing: it goes when the record it explains
			// goes, or the bucket grows without any bound at all.
			if payloads != nil {
				var evicted historyRecord
				if json.Unmarshal(v, &evicted) == nil && evicted.ID != "" {
					if err := payloads.Delete([]byte(evicted.ID)); err != nil {
						return err
					}
				}
			}
			if err := b.Delete(k); err != nil {
				return err
			}
			count--
		}
		return nil
	})
}

func (p *boltPersistence) HistoryBody(authIndex, id string) (bodyRecord, bool, error) {
	var out bodyRecord
	found := false
	err := p.db.View(func(tx *bolt.Tx) error {
		payloads, err := bodiesChild(tx, authIndex, false)
		if err != nil || payloads == nil {
			return err
		}
		raw := payloads.Get([]byte(id))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return fmt.Errorf("decode body: %w", err)
		}
		found = true
		return nil
	})
	return out, found, err
}

func (p *boltPersistence) History(authIndex string, page, size int) (historyPage, error) {
	return p.readHistory(authIndex, page, size, historyChild)
}

func (p *boltPersistence) ProbeHistory(authIndex string, page, size int) (historyPage, error) {
	return p.readHistory(authIndex, page, size, probeHistoryChild)
}

func (p *boltPersistence) readHistory(authIndex string, page, size int, child childFunc) (historyPage, error) {
	if size < 1 {
		size = pageSize
	}
	result := historyPage{AuthIndex: authIndex, Page: max(page, 1), PageSize: size, Items: []historyRecord{}}
	err := p.db.View(func(tx *bolt.Tx) error {
		b, _ := child(tx, authIndex, false)
		if b == nil {
			return nil
		}
		// The bucket is bounded, so reading it whole to order it by start time
		// costs less than the page it serves.
		records := make([]historyRecord, 0, b.Stats().KeyN)
		if err := b.ForEach(func(_, v []byte) error {
			var rec historyRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("decode history: %w", err)
			}
			records = append(records, rec)
			return nil
		}); err != nil {
			return err
		}
		sortHistoryNewestFirst(records)
		result = historyPageOf(authIndex, page, size, records)
		return nil
	})
	return result, err
}

func (p *boltPersistence) ClearProbeHistory(authIndex string) error {
	return p.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(probeHistoryBucket)
		if root == nil || root.Bucket([]byte(authIndex)) == nil {
			return nil
		}
		return root.DeleteBucket([]byte(authIndex))
	})
}

func (p *boltPersistence) SaveSession(session credentialSession) error {
	raw, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return p.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(sessionsBucket)
		if err != nil {
			return err
		}
		return bucket.Put(sessionKey(session.AuthIndex, session.Egress), raw)
	})
}

func (p *boltPersistence) Session(authIndex, egress string) (credentialSession, bool, error) {
	var out credentialSession
	found := false
	err := p.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(sessionsBucket)
		if bucket == nil {
			return nil
		}
		raw := bucket.Get(sessionKey(authIndex, egress))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return fmt.Errorf("decode session: %w", err)
		}
		found = true
		return nil
	})
	return out, found, err
}
func (p *boltPersistence) ClearHistory(authIndex string) error {
	return p.db.Update(func(tx *bolt.Tx) error {
		if bucket := tx.Bucket(sessionsBucket); bucket != nil {
			prefix := sessionKey(authIndex, "")
			cursor := bucket.Cursor()
			for k, _ := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = cursor.Next() {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		if probes := tx.Bucket(probeHistoryBucket); probes != nil && probes.Bucket([]byte(authIndex)) != nil {
			if err := probes.DeleteBucket([]byte(authIndex)); err != nil {
				return err
			}
		}
		if payloads := tx.Bucket(historyBodiesBucket); payloads != nil && payloads.Bucket([]byte(authIndex)) != nil {
			if err := payloads.DeleteBucket([]byte(authIndex)); err != nil {
				return err
			}
		}
		root := tx.Bucket(historyBucket)
		if root.Bucket([]byte(authIndex)) == nil {
			return nil
		}
		return root.DeleteBucket([]byte(authIndex))
	})
}
func (p *boltPersistence) HistoryCount(authIndex string) (int, error) {
	count := 0
	err := p.db.View(func(tx *bolt.Tx) error {
		b, _ := historyChild(tx, authIndex, false)
		if b != nil {
			count = b.Stats().KeyN
		}
		return nil
	})
	return count, err
}
func (p *boltPersistence) DeleteCredentialData(authIndex string) error {
	return p.db.Update(func(tx *bolt.Tx) error {
		_ = tx.Bucket(rulesBucket).Delete([]byte(authIndex))
		root := tx.Bucket(historyBucket)
		if root.Bucket([]byte(authIndex)) != nil {
			if err := root.DeleteBucket([]byte(authIndex)); err != nil {
				return err
			}
		}
		states := tx.Bucket(turnStatesBucket)
		cursor := states.Cursor()
		prefix := []byte(authIndex + "\x00")
		for key, _ := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, _ = cursor.Next() {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
}
func (p *boltPersistence) KnownAuthIndexes() ([]string, error) {
	seen := map[string]struct{}{}
	err := p.db.View(func(tx *bolt.Tx) error {
		if err := tx.Bucket(rulesBucket).ForEach(func(k, v []byte) error {
			if v != nil {
				seen[string(k)] = struct{}{}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.Bucket(historyBucket).ForEach(func(k, v []byte) error {
			if v == nil {
				seen[string(k)] = struct{}{}
			}
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket(turnStatesBucket).ForEach(func(_, value []byte) error {
			if value == nil {
				return nil
			}
			var record persistedTurnState
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			seen[record.AuthIndex] = struct{}{}
			return nil
		})
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, err
}

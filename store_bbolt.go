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
)

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
	root := tx.Bucket(historyBucket)
	if !create {
		return root.Bucket([]byte(authIndex)), nil
	}
	return root.CreateBucketIfNotExists([]byte(authIndex))
}

func (p *boltPersistence) AppendHistory(record historyRecord) error {
	record.BeforeHeaders = redactHeaders(record.BeforeHeaders)
	record.AfterHeaders = redactHeaders(record.AfterHeaders)
	record.ResponseHeaders = redactHeaders(record.ResponseHeaders)
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return p.db.Update(func(tx *bolt.Tx) error {
		b, err := historyChild(tx, record.AuthIndex, true)
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
		count := b.Stats().KeyN
		for count > historyLimit {
			c := b.Cursor()
			k, _ := c.First()
			if k == nil {
				break
			}
			if err := b.Delete(k); err != nil {
				return err
			}
			count--
		}
		return nil
	})
}

func (p *boltPersistence) History(authIndex string, page int) (historyPage, error) {
	if page < 1 {
		page = 1
	}
	result := historyPage{AuthIndex: authIndex, Page: page, PageSize: pageSize, Items: []historyRecord{}}
	err := p.db.View(func(tx *bolt.Tx) error {
		b, _ := historyChild(tx, authIndex, false)
		if b == nil {
			return nil
		}
		result.Total = b.Stats().KeyN
		result.TotalPages = (result.Total + pageSize - 1) / pageSize
		skip := (page - 1) * pageSize
		seen, taken := 0, 0
		c := b.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			if seen < skip {
				seen++
				continue
			}
			if taken >= pageSize {
				break
			}
			var rec historyRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("decode history: %w", err)
			}
			result.Items = append(result.Items, rec)
			taken++
			seen++
		}
		return nil
	})
	return result, err
}
func (p *boltPersistence) ClearHistory(authIndex string) error {
	return p.db.Update(func(tx *bolt.Tx) error {
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

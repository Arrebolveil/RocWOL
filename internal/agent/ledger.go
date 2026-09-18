package agent

import (
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const retention = 7 * 24 * time.Hour
const maxRecords = 4096
const cooldown = 10 * time.Second

var wakeBucket = []byte("wake-v1")
var metaBucket = []byte("meta-v1")
var lastDispatchKey = []byte("last-dispatch")
var errNotFound = errors.New("wake request not found")
var errCapacity = errors.New("wake ledger full")

type WakeResult struct {
	RequestID string     `json:"requestId"`
	Result    string     `json:"result"`
	SentAt    *time.Time `json:"sentAt,omitempty"`
	Code      string     `json:"code,omitempty"`
}

type wakeRecord struct {
	WakeResult
	CreatedAt time.Time `json:"createdAt"`
}

type ledger struct {
	db     *bolt.DB
	mu     sync.Mutex
	active map[string]bool
}

type rateError struct{ seconds int }

func (e rateError) Error() string { return "target cooldown active" }

func openLedger(path string) (*ledger, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	l := &ledger{db: db, active: make(map[string]bool)}
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(wakeBucket)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(metaBucket); err != nil {
			return err
		}
		// Dispatching records are never retried after process loss: the packet may
		// already have been transmitted before the final transaction committed.
		var expired [][]byte
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec wakeRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if time.Since(rec.CreatedAt) >= retention {
				expired = append(expired, append([]byte(nil), k...))
				continue
			}
			if rec.Result == "dispatching" {
				rec.Result, rec.Code = "unknown", "outcome_unknown"
				data, err := json.Marshal(rec)
				if err != nil {
					return err
				}
				if err := b.Put(k, data); err != nil {
					return err
				}
			}
		}
		for _, k := range expired {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return l, nil
}

func (l *ledger) publicResult(rec wakeRecord) WakeResult {
	r := rec.WakeResult
	if r.Result == "dispatching" {
		if l.active[r.RequestID] {
			r.Result = "pending"
		} else {
			r.Result, r.Code = "unknown", "outcome_unknown"
		}
	}
	return r
}

func (l *ledger) lookup(id string, now time.Time) (WakeResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var rec wakeRecord
	err := l.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(wakeBucket).Get([]byte(id))
		if v == nil {
			return errNotFound
		}
		if err := json.Unmarshal(v, &rec); err != nil {
			return err
		}
		if now.Sub(rec.CreatedAt) >= retention {
			return errNotFound
		}
		return nil
	})
	return l.publicResult(rec), err
}

// reserve commits and fsyncs dispatching before returning fresh=true. bbolt's
// default synchronous transactions must not be disabled.
func (l *ledger) reserve(id string, now time.Time) (result WakeResult, fresh bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	err = l.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(wakeBucket)
		if v := b.Get([]byte(id)); v != nil {
			var rec wakeRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if now.Sub(rec.CreatedAt) < retention {
				result = l.publicResult(rec)
				return nil
			}
		}
		m := tx.Bucket(metaBucket)
		if v := m.Get(lastDispatchKey); v != nil {
			var last time.Time
			if err := last.UnmarshalText(v); err != nil {
				return err
			}
			if remaining := cooldown - now.Sub(last); remaining > 0 {
				return rateError{seconds: int(math.Ceil(remaining.Seconds()))}
			}
		}
		count := 0
		var expired [][]byte
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec wakeRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			if now.Sub(rec.CreatedAt) >= retention {
				expired = append(expired, append([]byte(nil), k...))
			} else {
				count++
			}
		}
		if count >= maxRecords {
			return errCapacity
		}
		for _, k := range expired {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		rec := wakeRecord{WakeResult: WakeResult{RequestID: id, Result: "dispatching"}, CreatedAt: now}
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(id), data); err != nil {
			return err
		}
		stamp, err := now.MarshalText()
		if err != nil {
			return err
		}
		if err := m.Put(lastDispatchKey, stamp); err != nil {
			return err
		}
		result, fresh = WakeResult{RequestID: id, Result: "pending"}, true
		return nil
	})
	if err != nil {
		fresh = false
	}
	if fresh {
		l.active[id] = true
	}
	return
}

func (l *ledger) finish(result WakeResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer delete(l.active, result.RequestID)
	return l.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(wakeBucket)
		v := b.Get([]byte(result.RequestID))
		if v == nil {
			return errNotFound
		}
		var rec wakeRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return err
		}
		rec.WakeResult = result
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put([]byte(result.RequestID), data)
	})
}

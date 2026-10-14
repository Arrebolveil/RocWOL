package agent

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"hubu.wiki/roc-wol/internal/protocol"
)

const retention = 7 * 24 * time.Hour
const maxRecords = 4096
const cooldown = 10 * time.Second

var wakeBucket = []byte("wake-v1")
var metaBucket = []byte("meta-v1")
var lastDispatchKey = []byte("last-dispatch")
var schemaVersionKey = []byte("schemaVersion")
var errNotFound = errors.New("wake request not found")
var errCapacity = errors.New("wake ledger full")
var errTargetMismatch = errors.New("request target mismatch")
var errLedgerCorrupt = errors.New("wake ledger contains invalid data")

type WakeResult struct {
	RequestID string     `json:"requestId"`
	TargetID  string     `json:"targetId"`
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
	migrate := false
	err = db.View(func(tx *bolt.Tx) error {
		m := tx.Bucket(metaBucket)
		if m == nil || m.Get(schemaVersionKey) == nil {
			migrate = true
		} else if string(m.Get(schemaVersionKey)) != "2" {
			return errLedgerCorrupt
		}
		// A new, empty database needs no backup. For existing v1 files, use a
		// read transaction so the backup represents a consistent committed DB.
		if migrate && (m != nil || tx.Bucket(wakeBucket) != nil) {
			backupPath := path + ".schema-v1-" + protocol.ID() + ".bak"
			f, err := os.OpenFile(backupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return errors.New("cannot create wake ledger migration backup")
			}
			_, copyErr := tx.WriteTo(f)
			syncErr := f.Sync()
			closeErr := f.Close()
			if copyErr != nil || syncErr != nil || closeErr != nil {
				_ = os.Remove(f.Name())
				return errors.New("cannot complete wake ledger migration backup")
			}
		}
		return nil
	})
	if err == nil {
		err = db.Update(func(tx *bolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists(wakeBucket)
			if err != nil {
				return err
			}
			m, err := tx.CreateBucketIfNotExists(metaBucket)
			if err != nil {
				return err
			}
			if stamp := m.Get(lastDispatchKey); stamp != nil {
				var last time.Time
				if last.UnmarshalText(stamp) != nil || last.IsZero() {
					return errLedgerCorrupt
				}
			}
			// A lost dispatch is never retried: its packet may already be sent.
			var expired [][]byte
			c := b.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				var rec wakeRecord
				if strictJSON(v, &rec) != nil {
					return errLedgerCorrupt
				}
				if migrate && rec.TargetID == "" {
					rec.TargetID = "desktop"
				}
				if !validRecord(k, rec) {
					return errLedgerCorrupt
				}
				if !migrate && time.Since(rec.CreatedAt) >= retention {
					expired = append(expired, append([]byte(nil), k...))
					continue
				}
				if rec.Result == "dispatching" {
					rec.Result, rec.Code, rec.SentAt = "unknown", "outcome_unknown", nil
				}
				data, err := json.Marshal(rec)
				if err != nil {
					return errLedgerCorrupt
				}
				if err := b.Put(k, data); err != nil {
					return err
				}
			}
			for _, k := range expired {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
			return m.Put(schemaVersionKey, []byte("2"))
		})
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return l, nil
}

func validRecord(key []byte, rec wakeRecord) bool {
	if !protocol.ValidID(rec.RequestID) || string(key) != rec.RequestID || !protocol.ValidTargetID(rec.TargetID) || rec.CreatedAt.IsZero() {
		return false
	}
	switch rec.Result {
	case "sent", "pending", "dispatching", "failed", "unknown":
	default:
		return false
	}
	switch rec.Code {
	case "", "wake_failed", "outcome_unknown", "target_not_found", "request_target_mismatch":
		return true
	default:
		return false
	}
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
		if strictJSON(v, &rec) != nil || !validRecord([]byte(id), rec) {
			return errLedgerCorrupt
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
func (l *ledger) reserve(id, targetID string, now time.Time) (result WakeResult, fresh bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !protocol.ValidID(id) || !protocol.ValidTargetID(targetID) {
		return result, false, errors.New("invalid wake request")
	}
	err = l.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(wakeBucket)
		if v := b.Get([]byte(id)); v != nil {
			var rec wakeRecord
			if strictJSON(v, &rec) != nil || !validRecord([]byte(id), rec) {
				return errLedgerCorrupt
			}
			if now.Sub(rec.CreatedAt) < retention {
				if rec.TargetID != targetID {
					return errTargetMismatch
				}
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
			if strictJSON(v, &rec) != nil || !validRecord(k, rec) {
				return errLedgerCorrupt
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
		rec := wakeRecord{WakeResult: WakeResult{RequestID: id, TargetID: targetID, Result: "dispatching"}, CreatedAt: now}
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
		result, fresh = WakeResult{RequestID: id, TargetID: targetID, Result: "pending"}, true
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
		if strictJSON(v, &rec) != nil || !validRecord([]byte(result.RequestID), rec) {
			return errLedgerCorrupt
		}
		if result.TargetID != rec.TargetID {
			return errTargetMismatch
		}
		rec.WakeResult = result
		if !validRecord([]byte(result.RequestID), rec) {
			return errLedgerCorrupt
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put([]byte(result.RequestID), data)
	})
}

package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	"hubu.wiki/roc-wol/internal/protocol"
)

var ErrUnavailable = errors.New("record unavailable")
var ErrCapacity = errors.New("capacity reached")
var ErrBinding = errors.New("record binding mismatch")
var ErrInvalidCapacity = errors.New("invalid device capacity")
var ErrCorrupt = errors.New("invalid stored record")

type Device struct {
	DeviceID             string    `json:"deviceId"`
	DeviceName           string    `json:"deviceName"`
	WakeKey              string    `json:"wakePublicKey"`
	ManageKey            string    `json:"managePublicKey"`
	CreatedAt            time.Time `json:"createdAt"`
	Revoked              bool      `json:"revoked"`
	DeviceAdministration bool      `json:"deviceAdministration"`
}
type WakeRecord struct {
	protocol.Wake
	DeviceID  string    `json:"deviceId"`
	CreatedAt time.Time `json:"createdAt"`
}
type Store struct {
	DB         *bolt.DB
	instanceID string
}

func Open(path string) (*Store, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	db, e := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if e != nil {
		return nil, e
	}
	s := &Store{DB: db}
	s.instanceID, e = initialize(db, path)
	if e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }

// InstanceID is generated once and persisted independently of schema upgrades.
func (s *Store) InstanceID() string { return s.instanceID }
func put(b *bolt.Bucket, key string, v any) error {
	data, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return b.Put([]byte(key), data)
}
func (s *Store) AddCode(hash string, expires time.Time) error {
	return s.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("codes"))
		c := b.Cursor()
		now := time.Now()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t time.Time
			if json.Unmarshal(v, &t) != nil || !t.After(now) {
				if e := c.Delete(); e != nil {
					return e
				}
			}
		}
		return put(b, hash, expires)
	})
}
func (s *Store) CodeValid(hash string, now time.Time) bool {
	ok := false
	_ = s.DB.View(func(tx *bolt.Tx) error {
		var t time.Time
		b := tx.Bucket([]byte("codes")).Get([]byte(hash))
		ok = json.Unmarshal(b, &t) == nil && t.After(now)
		return nil
	})
	return ok
}
func (s *Store) Register(hash string, d Device, now time.Time, maxDevices int) error {
	if maxDevices < 1 || maxDevices > 100 {
		return ErrInvalidCapacity
	}
	// Pairing never grants device administration; only the local admin socket may.
	d.DeviceAdministration = false
	return s.DB.Update(func(tx *bolt.Tx) error {
		codes := tx.Bucket([]byte("codes"))
		var expires time.Time
		if json.Unmarshal(codes.Get([]byte(hash)), &expires) != nil || !expires.After(now) {
			return ErrUnavailable
		}
		b := tx.Bucket([]byte("devices"))
		// A pairing code must never overwrite or revive an existing identity.
		if b.Get([]byte(d.DeviceID)) != nil {
			return ErrUnavailable
		}
		count := 0
		if e := b.ForEach(func(k, v []byte) error {
			var old Device
			if e := json.Unmarshal(v, &old); e != nil {
				return e
			}
			if !old.Revoked {
				count++
			}
			return nil
		}); e != nil {
			return e
		}
		if count >= maxDevices {
			return ErrCapacity
		}
		if e := put(b, d.DeviceID, d); e != nil {
			return e
		}
		return codes.Delete([]byte(hash))
	})
}
func (s *Store) Device(id string) (Device, error) {
	var d Device
	e := s.DB.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte("devices")).Get([]byte(id))
		if v == nil {
			return ErrUnavailable
		}
		return json.Unmarshal(v, &d)
	})
	return d, e
}
func (s *Store) Devices() ([]Device, error) {
	out := []Device{}
	e := s.DB.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("devices")).ForEach(func(k, v []byte) error {
			var d Device
			if e := json.Unmarshal(v, &d); e != nil {
				return e
			}
			out = append(out, d)
			return nil
		})
	})
	return out, e
}
func (s *Store) Revoke(id string) error {
	return s.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("devices"))
		var d Device
		if v := b.Get([]byte(id)); v == nil {
			return ErrUnavailable
		} else if e := json.Unmarshal(v, &d); e != nil {
			return e
		}
		d.Revoked = true
		return put(b, id, d)
	})
}
func (s *Store) SetDeviceAdministration(id string, enabled bool) error {
	return s.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("devices"))
		var d Device
		v := b.Get([]byte(id))
		if v == nil {
			return ErrUnavailable
		}
		if e := json.Unmarshal(v, &d); e != nil {
			return e
		}
		if d.Revoked {
			return ErrUnavailable
		}
		d.DeviceAdministration = enabled
		return put(b, id, d)
	})
}
func (s *Store) Wake(id string) (WakeRecord, error) {
	var r WakeRecord
	e := s.DB.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte("wakes")).Get([]byte(id))
		if v == nil {
			return ErrUnavailable
		}
		if e := decodeWake(v, &r); e != nil {
			return e
		}
		if time.Since(r.CreatedAt) > 7*24*time.Hour {
			return ErrUnavailable
		}
		return nil
	})
	return r, e
}
func (s *Store) CreateWake(r WakeRecord) error {
	if !validWake(r) {
		return ErrCorrupt
	}
	return s.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("wakes"))
		c := b.Cursor()
		cutoff := time.Now().Add(-7 * 24 * time.Hour)
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var old WakeRecord
			if e := decodeWake(v, &old); e != nil {
				return e
			}
			if old.CreatedAt.Before(cutoff) {
				if e := c.Delete(); e != nil {
					return e
				}
			}
		}
		if b.Get([]byte(r.RequestID)) != nil {
			return ErrUnavailable
		}
		return put(b, r.RequestID, r)
	})
}
func (s *Store) UpdateWake(r WakeRecord) error {
	if !validWake(r) {
		return ErrCorrupt
	}
	return s.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("wakes"))
		v := b.Get([]byte(r.RequestID))
		if v == nil {
			return ErrUnavailable
		}
		var old WakeRecord
		if e := decodeWake(v, &old); e != nil {
			return e
		}
		if old.DeviceID != r.DeviceID || old.TargetID != r.TargetID {
			return ErrBinding
		}
		return put(b, r.RequestID, r)
	})
}
func (s *Store) LastWake(device, targetID string) (*protocol.Wake, error) {
	var latest WakeRecord
	e := s.DB.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("wakes")).ForEach(func(k, v []byte) error {
			var r WakeRecord
			if e := decodeWake(v, &r); e != nil {
				return e
			}
			if r.DeviceID == device && r.TargetID == targetID && r.CreatedAt.After(latest.CreatedAt) && time.Since(r.CreatedAt) < 7*24*time.Hour {
				latest = r
			}
			return nil
		})
	})
	if e != nil {
		return nil, e
	}
	if latest.RequestID == "" {
		return nil, nil
	}
	if latest.Result == "pending" {
		latest.Result = "unknown"
	}
	return &latest.Wake, nil
}

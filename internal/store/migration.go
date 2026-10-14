package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	"hubu.wiki/roc-wol/internal/protocol"
)

const schemaVersion = "2"

// initialize validates before exposing the store. Only migration supplies the
// legacy desktop binding; ordinary reads always require a persisted target ID.
func initialize(db *bolt.DB, path string) (string, error) {
	legacy, populated := true, false
	var instanceID string
	if e := db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Cursor().First()
		populated = k != nil
		if metadata := tx.Bucket([]byte("metadata")); metadata != nil {
			version := metadata.Get([]byte("schemaVersion"))
			switch string(version) {
			case "", "1":
			case schemaVersion:
				legacy = false
			default:
				return errors.New("unsupported store schema")
			}
			instanceID = string(metadata.Get([]byte("instanceId")))
		}
		if instanceID != "" && !protocol.ValidID(instanceID) || !legacy && instanceID == "" {
			return errors.New("invalid store metadata")
		}
		return nil
	}); e != nil {
		return "", e
	}
	if legacy && populated {
		if e := backupSchema(db, path); e != nil {
			return "", e
		}
	}
	if instanceID == "" {
		var id [16]byte
		if _, e := rand.Read(id[:]); e != nil {
			return "", e
		}
		instanceID = hex.EncodeToString(id[:])
	}
	e := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"devices", "codes", "wakes", "metadata"} {
			if _, e := tx.CreateBucketIfNotExists([]byte(name)); e != nil {
				return e
			}
		}
		if e := tx.Bucket([]byte("devices")).ForEach(func(k, v []byte) error {
			var d Device
			if protocol.DecodeJSON(bytes.NewReader(v), &d, protocol.JSONLimit) != nil || d.DeviceID != string(k) || !protocol.ValidID(d.DeviceID) || d.CreatedAt.IsZero() {
				return ErrCorrupt
			}
			return nil
		}); e != nil {
			return e
		}
		if e := tx.Bucket([]byte("codes")).ForEach(func(k, v []byte) error {
			var expires time.Time
			if json.Unmarshal(v, &expires) != nil || expires.IsZero() {
				return ErrCorrupt
			}
			return nil
		}); e != nil {
			return e
		}
		wakes := tx.Bucket([]byte("wakes"))
		var migrated []WakeRecord
		if e := wakes.ForEach(func(k, v []byte) error {
			var r WakeRecord
			if protocol.DecodeJSON(bytes.NewReader(v), &r, protocol.JSONLimit) != nil {
				return ErrCorrupt
			}
			changed := false
			if legacy {
				var fields map[string]json.RawMessage
				if json.Unmarshal(v, &fields) != nil {
					return ErrCorrupt
				}
				if _, present := fields["targetId"]; !present {
					r.TargetID = "desktop"
					changed = true
				}
			}
			if r.RequestID != string(k) || !validWake(r) {
				return ErrCorrupt
			}
			if changed {
				migrated = append(migrated, r)
			}
			return nil
		}); e != nil {
			return e
		}
		for _, r := range migrated {
			if e := put(wakes, r.RequestID, r); e != nil {
				return e
			}
		}
		metadata := tx.Bucket([]byte("metadata"))
		if e := metadata.Put([]byte("schemaVersion"), []byte(schemaVersion)); e != nil {
			return e
		}
		return metadata.Put([]byte("instanceId"), []byte(instanceID))
	})
	return instanceID, e
}

// A read transaction gives a consistent bbolt snapshot. Unique names retain all
// completed backups, including when a failed migration is retried after repair.
func backupSchema(db *bolt.DB, path string) error {
	f, e := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".schema-v1-*.tmp")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		e = db.View(func(tx *bolt.Tx) error {
			_, e := tx.WriteTo(f)
			return e
		})
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, name[:len(name)-4]+".bak")
}

func validWake(r WakeRecord) bool {
	return protocol.ValidID(r.RequestID) && protocol.ValidID(r.DeviceID) && protocol.ValidTargetID(r.TargetID) && !r.CreatedAt.IsZero()
}

func decodeWake(v []byte, r *WakeRecord) error {
	if protocol.DecodeJSON(bytes.NewReader(v), r, protocol.JSONLimit) != nil || !validWake(*r) {
		return ErrCorrupt
	}
	return nil
}

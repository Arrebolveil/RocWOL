package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"hubu.wiki/roc-wol/internal/protocol"
)

func legacyDatabase(t *testing.T, path string, records []WakeRecord) map[string][]byte {
	t.Helper()
	db, e := bolt.Open(path, 0600, nil)
	if e != nil {
		t.Fatal(e)
	}
	originals := make(map[string][]byte)
	e = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"devices", "codes", "wakes"} {
			if _, e := tx.CreateBucket([]byte(name)); e != nil {
				return e
			}
		}
		for _, r := range records {
			data, e := json.Marshal(r)
			if e != nil {
				return e
			}
			var fields map[string]json.RawMessage
			if e = json.Unmarshal(data, &fields); e != nil {
				return e
			}
			delete(fields, "targetId")
			data, e = json.Marshal(fields)
			if e != nil {
				return e
			}
			originals[r.RequestID] = data
			if e = tx.Bucket([]byte("wakes")).Put([]byte(r.RequestID), data); e != nil {
				return e
			}
		}
		return nil
	})
	closeErr := db.Close()
	if e != nil {
		t.Fatal(e)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return originals
}

func TestOpenMigratesLegacyBindingsWithConsistentBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	now := time.Now().UTC()
	device := protocol.ID()
	sentAt := now.Add(-time.Minute)
	records := []WakeRecord{
		{Wake: protocol.Wake{RequestID: protocol.ID(), Result: "sent", SentAt: &sentAt}, DeviceID: device, CreatedAt: sentAt},
		{Wake: protocol.Wake{RequestID: protocol.ID(), Result: "pending"}, DeviceID: device, CreatedAt: now},
		{Wake: protocol.Wake{RequestID: protocol.ID(), Result: "unknown", Code: "outcome_unknown"}, DeviceID: device, CreatedAt: now.Add(-8 * 24 * time.Hour)},
	}
	originals := legacyDatabase(t, path, records)
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	instance := s.InstanceID()
	if !protocol.ValidID(instance) {
		t.Fatal("invalid instance identity")
	}
	if e = s.DB.View(func(tx *bolt.Tx) error {
		if string(tx.Bucket([]byte("metadata")).Get([]byte("schemaVersion"))) != "2" {
			t.Fatal("schema version not committed")
		}
		for _, expected := range records {
			var got WakeRecord
			if e := decodeWake(tx.Bucket([]byte("wakes")).Get([]byte(expected.RequestID)), &got); e != nil {
				return e
			}
			if got.TargetID != "desktop" || got.DeviceID != device || got.Result != expected.Result || got.Code != expected.Code || !got.CreatedAt.Equal(expected.CreatedAt) {
				t.Fatalf("migration lost legacy wake: %+v", got)
			}
			if expected.SentAt != nil && (got.SentAt == nil || !got.SentAt.Equal(*expected.SentAt)) {
				t.Fatal("migration lost dispatch timestamp")
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	backups, e := filepath.Glob(path + ".schema-v1-*.bak")
	if e != nil || len(backups) != 1 {
		t.Fatalf("expected one migration backup: %v %v", backups, e)
	}
	backup, e := bolt.Open(backups[0], 0600, &bolt.Options{ReadOnly: true})
	if e != nil {
		t.Fatal(e)
	}
	e = backup.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("metadata")) != nil {
			t.Fatal("backup contains migration writes")
		}
		for id, original := range originals {
			if !bytes.Equal(original, tx.Bucket([]byte("wakes")).Get([]byte(id))) {
				t.Fatal("backup differs from original record")
			}
		}
		return nil
	})
	backup.Close()
	if e != nil {
		t.Fatal(e)
	}
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.InstanceID() != instance {
		t.Fatal("instance identity rotated on restart")
	}
	backups, e = filepath.Glob(path + ".schema-v1-*.bak")
	if e != nil || len(backups) != 1 {
		t.Fatal("completed migration ran again")
	}
}

func TestFreshInstanceIdentityPersistsWithoutMigrationBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	instance := s.InstanceID()
	if !protocol.ValidID(instance) {
		t.Fatal("invalid instance identity")
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if instance != s.InstanceID() {
		t.Fatal("fresh instance identity changed")
	}
	backups, e := filepath.Glob(path + ".schema-v1-*.bak")
	if e != nil || len(backups) != 0 {
		t.Fatal("fresh database unnecessarily backed up")
	}
}

func TestMalformedMigrationFailsSanitizedAndDoesNotCommit(t *testing.T) {
	for _, malformed := range []string{`{"private-secret":`, `{"requestId":"private-secret"}`, `null`} {
		t.Run(malformed, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway.db")
			legacyDatabase(t, path, nil)
			db, e := bolt.Open(path, 0600, nil)
			if e != nil {
				t.Fatal(e)
			}
			id := protocol.ID()
			if e = db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("wakes")).Put([]byte(id), []byte(malformed)) }); e != nil {
				t.Fatal(e)
			}
			db.Close()
			s, e := Open(path)
			if s != nil || !errors.Is(e, ErrCorrupt) || strings.Contains(e.Error(), "private-secret") {
				t.Fatalf("malformed record not rejected safely: %v", e)
			}
			db, e = bolt.Open(path, 0600, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			if e = db.View(func(tx *bolt.Tx) error {
				if tx.Bucket([]byte("metadata")) != nil || !bytes.Equal(tx.Bucket([]byte("wakes")).Get([]byte(id)), []byte(malformed)) {
					t.Fatal("failed migration changed original database")
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
			backups, e := filepath.Glob(path + ".schema-v1-*.bak")
			if e != nil || len(backups) != 1 {
				t.Fatal("failed migration did not preserve original backup")
			}
		})
	}
}

func TestV2MissingTargetFailsWithoutCompatibilityDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	r := WakeRecord{Wake: protocol.Wake{RequestID: protocol.ID(), Result: "sent"}, DeviceID: protocol.ID(), CreatedAt: time.Now().UTC()}
	if e = s.DB.Update(func(tx *bolt.Tx) error { return put(tx.Bucket([]byte("wakes")), r.RequestID, r) }); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Wake(r.RequestID); !errors.Is(e, ErrCorrupt) {
		t.Fatal("read silently supplied legacy target:", e)
	}
	s.Close()
	if s, e = Open(path); s != nil || !errors.Is(e, ErrCorrupt) {
		t.Fatal("v2 startup repaired missing target:", e)
	}
}

func TestBackupFailureLeavesLegacyDatabaseUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	legacyDatabase(t, path, nil)
	db, e := bolt.Open(path, 0600, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = initialize(db, filepath.Join(path, "unwritable.db")); e == nil {
		t.Fatal("migration continued without backup")
	}
	if e = db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("metadata")) != nil {
			t.Fatal("backup failure committed metadata")
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(path); e != nil {
		t.Fatal(e)
	}
}

func TestSchemaOneMigrationPreservesExistingInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	r := WakeRecord{Wake: protocol.Wake{RequestID: protocol.ID(), Result: "pending"}, DeviceID: protocol.ID(), CreatedAt: time.Now().UTC()}
	legacyDatabase(t, path, []WakeRecord{r})
	instance := protocol.ID()
	db, e := bolt.Open(path, 0600, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = db.Update(func(tx *bolt.Tx) error {
		b, e := tx.CreateBucket([]byte("metadata"))
		if e != nil {
			return e
		}
		if e = b.Put([]byte("schemaVersion"), []byte("1")); e != nil {
			return e
		}
		return b.Put([]byte("instanceId"), []byte(instance))
	}); e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.InstanceID() != instance {
		t.Fatal("migration rotated existing instance identity")
	}
	last, e := s.LastWake(r.DeviceID, "desktop")
	if e != nil || last == nil || last.TargetID != "desktop" || last.RequestID != r.RequestID {
		t.Fatalf("legacy history lost its explicit target: %+v %v", last, e)
	}
}

func TestStartupRejectsMalformedDevicesCodesAndMetadata(t *testing.T) {
	for _, bucket := range []string{"devices", "codes", "schema", "instance"} {
		t.Run(bucket, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gateway.db")
			s, e := Open(path)
			if e != nil {
				t.Fatal(e)
			}
			if e = s.DB.Update(func(tx *bolt.Tx) error {
				switch bucket {
				case "devices", "codes":
					return tx.Bucket([]byte(bucket)).Put([]byte(protocol.ID()), []byte(`"private-secret"`))
				case "schema":
					return tx.Bucket([]byte("metadata")).Put([]byte("schemaVersion"), []byte("private-secret"))
				default:
					return tx.Bucket([]byte("metadata")).Put([]byte("instanceId"), []byte("private-secret"))
				}
			}); e != nil {
				t.Fatal(e)
			}
			s.Close()
			s, e = Open(path)
			if s != nil || e == nil || strings.Contains(e.Error(), "private-secret") {
				t.Fatalf("corrupt %s not rejected safely: %v", bucket, e)
			}
		})
	}
}

func TestMigratedRequestCannotBeReboundToNewTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	r := WakeRecord{Wake: protocol.Wake{RequestID: protocol.ID(), Result: "pending"}, DeviceID: protocol.ID(), CreatedAt: time.Now().UTC()}
	legacyDatabase(t, path, []WakeRecord{r})
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r.TargetID = "pc-a"
	if e = s.CreateWake(r); !errors.Is(e, ErrUnavailable) {
		t.Fatal("legacy request ID reused for new target:", e)
	}
	if e = s.UpdateWake(r); !errors.Is(e, ErrBinding) {
		t.Fatal("legacy request binding overwritten:", e)
	}
	got, e := s.Wake(r.RequestID)
	if e != nil || got.TargetID != "desktop" || got.DeviceID != r.DeviceID || got.Result != "pending" {
		t.Fatalf("legacy request was changed by target replay: %+v %v", got, e)
	}
}

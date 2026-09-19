package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"hubu.wiki/roc-wol/internal/protocol"
)

func TestDeviceAdministrationDefaultsDenyAndPersistsExplicitChoice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if s != nil {
			s.Close()
		}
	})
	now := time.Now().UTC()
	id := protocol.ID()
	code := protocol.Hash("test-only-code")
	if e = s.AddCode(code, now.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e = s.Register(code, Device{DeviceID: id, DeviceName: "phone", CreatedAt: now, DeviceAdministration: true}, now); e != nil {
		t.Fatal(e)
	}
	d, e := s.Device(id)
	if e != nil || d.DeviceAdministration {
		t.Fatalf("pairing accepted caller-supplied administration: %+v %v", d, e)
	}
	// Represent a pre-capability database record, not a synthetic default value.
	if e = s.DB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("devices"))
		var legacy map[string]any
		if e := json.Unmarshal(b.Get([]byte(id)), &legacy); e != nil {
			return e
		}
		delete(legacy, "deviceAdministration")
		return put(b, id, legacy)
	}); e != nil {
		t.Fatal(e)
	}
	d, e = s.Device(id)
	if e != nil || d.DeviceAdministration {
		t.Fatalf("legacy device gained administration: %+v %v", d, e)
	}
	for _, enabled := range []bool{true, false} {
		if e = s.SetDeviceAdministration(id, enabled); e != nil {
			t.Fatal(e)
		}
		if e = s.Close(); e != nil {
			t.Fatal(e)
		}
		s, e = Open(path)
		if e != nil {
			t.Fatal(e)
		}
		d, e = s.Device(id)
		if e != nil || d.DeviceAdministration != enabled || d.Revoked {
			t.Fatalf("explicit capability lost on restart: %+v %v", d, e)
		}
	}
	if e = s.Revoke(id); e != nil {
		t.Fatal(e)
	}
	if e = s.SetDeviceAdministration(id, true); !errors.Is(e, ErrUnavailable) {
		t.Fatalf("capability grant revived revoked identity: %v", e)
	}
}

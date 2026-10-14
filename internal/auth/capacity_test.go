package auth

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
	"hubu.wiki/roc-wol/internal/store"
)

func signedDeviceList(t *testing.T, s *State, owner protocol.Credentials, k keys) protocol.DeviceList {
	t.Helper()
	c := administrationChallenge(t, s, owner.DeviceID, "list", "")
	rows, e := s.ListDevices(owner.DeviceID, c.ChallengeID, sign(t, k.manage,
		protocol.DeviceAdministrationMessage(owner.DeviceID, c.ChallengeID, c.Nonce, administrationOrigin, "list", "")), administrationOrigin)
	if e != nil {
		t.Fatal(e)
	}
	return rows
}

func TestCapacityReductionKeepsDevicesAndReportsNewLimit(t *testing.T) {
	s := state(t)
	s = New(s.db, 6)
	k := newKeys(t)
	owner := administrator(t, s, k)
	for range 5 {
		pair(t, s, newKeys(t))
	}
	rows := signedDeviceList(t, s, owner, k)
	if rows.Capacity != 6 || len(rows.Devices) != 6 {
		t.Fatalf("configured capacity missing: %+v", rows)
	}
	lowered := New(s.db, 5)
	rows = signedDeviceList(t, lowered, owner, k)
	if rows.Capacity != 5 || len(rows.Devices) != 6 {
		t.Fatalf("capacity reduction removed devices: %+v", rows)
	}
	for _, d := range rows.Devices {
		if _, e := lowered.AuthChallenge(d.DeviceID); e != nil {
			t.Fatal("capacity reduction disabled existing authentication:", e)
		}
	}
	candidate := newKeys(t)
	code, e := lowered.PairCode(time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	c, e := lowered.PairChallenge(code, "another phone", candidate.wp, candidate.mp)
	if e != nil {
		t.Fatal(e)
	}
	message := protocol.PairMessage(c.ChallengeID, c.Nonce, candidate.wp, candidate.mp)
	_, e = lowered.PairComplete(c.ChallengeID, sign(t, candidate.wake, message), sign(t, candidate.manage, message))
	wantFailure(t, e, 429)
	if !s.db.CodeValid(protocol.Hash(code), time.Now()) {
		t.Fatal("capacity refusal spent pairing code")
	}
	var target string
	for _, d := range rows.Devices {
		if !d.Current {
			target = d.DeviceID
			break
		}
	}
	c = administrationChallenge(t, lowered, owner.DeviceID, "revoke", target)
	if _, e = lowered.RevokeDevice(owner.DeviceID, c.ChallengeID, sign(t, k.manage,
		protocol.DeviceAdministrationMessage(owner.DeviceID, c.ChallengeID, c.Nonce, administrationOrigin, "revoke", target)), administrationOrigin, target); e != nil {
		t.Fatal("over-capacity device could not be revoked:", e)
	}
	rows = signedDeviceList(t, lowered, owner, k)
	if rows.Capacity != 5 || len(rows.Devices) != 5 {
		t.Fatalf("revocation did not preserve capacity: %+v", rows)
	}
}

func TestDeviceListSupportsOneHundredMaximumLengthNames(t *testing.T) {
	s := state(t)
	s = New(s.db, 100)
	k := newKeys(t)
	now := time.Now().UTC()
	owner := protocol.Credentials{DeviceID: protocol.ID()}
	for i := range 100 {
		id := protocol.ID()
		if i == 0 {
			id = owner.DeviceID
		}
		code := protocol.Hash(protocol.ID())
		if e := s.db.AddCode(code, now.Add(time.Hour)); e != nil {
			t.Fatal(e)
		}
		if e := s.db.Register(code, store.Device{DeviceID: id, DeviceName: strings.Repeat("電", 64), WakeKey: k.wp, ManageKey: k.mp, CreatedAt: now}, now, 100); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.SetDeviceAdministration(owner.DeviceID, true); e != nil {
		t.Fatal(e)
	}
	rows := signedDeviceList(t, s, owner, k)
	if rows.Capacity != 100 || len(rows.Devices) != 100 {
		t.Fatalf("maximum-capacity devices missing: %d/%d", len(rows.Devices), rows.Capacity)
	}
	data, e := json.Marshal(rows)
	if e != nil {
		t.Fatal(e)
	}
	if len(data) >= protocol.ResponseJSONLimit {
		t.Fatal("maximum device list exceeds response limit")
	}
	var decoded protocol.DeviceList
	if e = protocol.DecodeJSON(strings.NewReader(string(data)), &decoded, protocol.ResponseJSONLimit); e != nil || len(decoded.Devices) != 100 || decoded.Capacity != 100 {
		t.Fatal("maximum device list cannot be decoded:", e)
	}
}

func TestAuthDoesNotNormalizeInvalidCapacity(t *testing.T) {
	for _, capacity := range []int{-1, 0, 101} {
		s := state(t)
		s = New(s.db, capacity)
		k := newKeys(t)
		code, e := s.PairCode(time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		c, e := s.PairChallenge(code, "phone", k.wp, k.mp)
		if e != nil {
			t.Fatal(e)
		}
		message := protocol.PairMessage(c.ChallengeID, c.Nonce, k.wp, k.mp)
		_, e = s.PairComplete(c.ChallengeID, sign(t, k.wake, message), sign(t, k.manage, message))
		if !errors.Is(e, store.ErrInvalidCapacity) {
			t.Fatalf("invalid capacity %d was normalized: %v", capacity, e)
		}
		devices, e := s.db.Devices()
		if e != nil || len(devices) != 0 {
			t.Fatal("invalid capacity registered a device")
		}
	}
}

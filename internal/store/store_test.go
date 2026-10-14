package store

import (
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "store.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWakeBindingAndTargetScopedLastWake(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	device, other := protocol.ID(), protocol.ID()
	a := WakeRecord{Wake: protocol.Wake{RequestID: protocol.ID(), TargetID: "pc-a", Result: "sent"}, DeviceID: device, CreatedAt: now.Add(-time.Minute)}
	b := WakeRecord{Wake: protocol.Wake{RequestID: protocol.ID(), TargetID: "pc-b", Result: "pending"}, DeviceID: device, CreatedAt: now}
	c := WakeRecord{Wake: protocol.Wake{RequestID: protocol.ID(), TargetID: "pc-a", Result: "failed"}, DeviceID: other, CreatedAt: now.Add(time.Second)}
	for _, r := range []WakeRecord{a, b, c} {
		if e := s.CreateWake(r); e != nil {
			t.Fatal(e)
		}
	}
	for _, binding := range []string{"device", "target"} {
		changed := a
		if binding == "device" {
			changed.DeviceID = other
		} else {
			changed.TargetID = "pc-b"
		}
		if e := s.UpdateWake(changed); !errors.Is(e, ErrBinding) {
			t.Fatalf("%s binding overwritten: %v", binding, e)
		}
		if e := s.CreateWake(changed); !errors.Is(e, ErrUnavailable) {
			t.Fatalf("request ID rebound by creation: %v", e)
		}
	}
	got, e := s.Wake(a.RequestID)
	if e != nil || got.DeviceID != device || got.TargetID != "pc-a" || got.Result != "sent" {
		t.Fatalf("failed mutation changed original request: %+v %v", got, e)
	}
	a.Result, a.Code = "failed", "wake_failed"
	if e = s.UpdateWake(a); e != nil {
		t.Fatal(e)
	}
	for _, target := range []string{"pc-a", "pc-b", "removed-target"} {
		last, e := s.LastWake(device, target)
		if e != nil {
			t.Fatal(e)
		}
		switch target {
		case "pc-a":
			if last == nil || last.RequestID != a.RequestID || last.TargetID != target || last.Result != "failed" {
				t.Fatalf("wrong pc-a history: %+v", last)
			}
		case "pc-b":
			if last == nil || last.RequestID != b.RequestID || last.TargetID != target || last.Result != "unknown" {
				t.Fatalf("wrong pc-b history: %+v", last)
			}
		default:
			if last != nil {
				t.Fatal("history leaked to unrelated target")
			}
		}
	}
	got, e = s.Wake(b.RequestID)
	if e != nil || got.Result != "pending" {
		t.Fatal("LastWake mutated pending record")
	}
}

func TestRegistrationCapacityIsAtomicAndDoesNotConsumeRejectedCodes(t *testing.T) {
	for _, capacity := range []int{1, 6, 100} {
		t.Run(strconv.Itoa(capacity), func(t *testing.T) {
			s := testStore(t)
			now := time.Now().UTC()
			codes := make([]string, capacity+2)
			for i := range codes {
				codes[i] = protocol.Hash(protocol.ID())
				if e := s.AddCode(codes[i], now.Add(time.Hour)); e != nil {
					t.Fatal(e)
				}
			}
			var wg sync.WaitGroup
			results := make(chan error, len(codes))
			for _, code := range codes {
				wg.Add(1)
				go func(code string) {
					defer wg.Done()
					results <- s.Register(code, Device{DeviceID: protocol.ID(), DeviceName: "phone", CreatedAt: now}, now, capacity)
				}(code)
			}
			wg.Wait()
			close(results)
			accepted, rejected := 0, 0
			for e := range results {
				if e == nil {
					accepted++
				} else if errors.Is(e, ErrCapacity) {
					rejected++
				} else {
					t.Fatal(e)
				}
			}
			if accepted != capacity || rejected != 2 {
				t.Fatalf("capacity race: accepted=%d rejected=%d", accepted, rejected)
			}
			devices, e := s.Devices()
			if e != nil || len(devices) != capacity {
				t.Fatalf("wrong persisted capacity: %d %v", len(devices), e)
			}
			var unused string
			for _, code := range codes {
				if s.CodeValid(code, now) {
					unused = code
				}
			}
			if unused == "" {
				t.Fatal("capacity rejection consumed pairing codes")
			}
			if e = s.Revoke(devices[0].DeviceID); e != nil {
				t.Fatal(e)
			}
			if e = s.Register(unused, Device{DeviceID: protocol.ID(), CreatedAt: now}, now, capacity); e != nil {
				t.Fatal("revoked device still occupied capacity:", e)
			}
		})
	}
}

func TestInvalidOrLoweredCapacityDoesNotRevokeOrOverwrite(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	for range 6 {
		code := protocol.Hash(protocol.ID())
		if e := s.AddCode(code, now.Add(time.Hour)); e != nil {
			t.Fatal(e)
		}
		if e := s.Register(code, Device{DeviceID: protocol.ID(), DeviceName: "original", CreatedAt: now}, now, 6); e != nil {
			t.Fatal(e)
		}
	}
	code := protocol.Hash(protocol.ID())
	if e := s.AddCode(code, now.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	for _, capacity := range []int{-1, 0, 101} {
		if e := s.Register(code, Device{DeviceID: protocol.ID(), CreatedAt: now}, now, capacity); !errors.Is(e, ErrInvalidCapacity) {
			t.Fatalf("invalid capacity %d accepted: %v", capacity, e)
		}
	}
	if e := s.Register(code, Device{DeviceID: protocol.ID(), CreatedAt: now}, now, 5); !errors.Is(e, ErrCapacity) {
		t.Fatal("lowered capacity allowed another device:", e)
	}
	devices, e := s.Devices()
	if e != nil || len(devices) != 6 {
		t.Fatal("lowered capacity removed devices:", e)
	}
	for _, d := range devices {
		if d.Revoked {
			t.Fatal("lowered capacity revoked device")
		}
	}
	if e = s.Register(code, Device{DeviceID: devices[0].DeviceID, DeviceName: "replacement", CreatedAt: now}, now, 100); !errors.Is(e, ErrUnavailable) {
		t.Fatal("pairing overwrote existing identity:", e)
	}
	if !s.CodeValid(code, now) {
		t.Fatal("rejected registration consumed code")
	}
}

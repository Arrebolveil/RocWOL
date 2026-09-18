package auth

import (
	"testing"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
)

func TestDeviceCapacityRequiresExplicitRevocation(t *testing.T) {
	s := state(t)
	var first protocol.Credentials
	for i := range 5 {
		c := pair(t, s, newKeys(t))
		if i == 0 {
			first = c
		}
	}
	k := newKeys(t)
	code, e := s.PairCode(time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	complete := func() error {
		c, e := s.PairChallenge(code, "replacement", k.wp, k.mp)
		if e != nil {
			return e
		}
		msg := protocol.PairMessage(c.ChallengeID, c.Nonce, k.wp, k.mp)
		_, e = s.PairComplete(c.ChallengeID, sign(t, k.wake, msg), sign(t, k.manage, msg))
		return e
	}
	wantFailure(t, complete(), 429)
	if e = s.Revoke(first.DeviceID); e != nil {
		t.Fatal(e)
	}
	if e = complete(); e != nil {
		t.Fatalf("explicit revocation did not free capacity: %v", e)
	}
}

func TestPerDeviceChallengeRateResetsWithoutChangingIdentity(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	id := pair(t, s, k).DeviceID
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for range 10 {
		if _, e := s.AuthChallenge(id); e != nil {
			t.Fatal(e)
		}
	}
	_, e := s.AuthChallenge(id)
	wantFailure(t, e, 429)
	now = now.Add(61 * time.Second)
	c, e := s.AuthChallenge(id)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.AuthComplete(id, c.ChallengeID, sign(t, k.wake, protocol.WakeMessage(id, c.ChallengeID, c.Nonce)))
	if e != nil {
		t.Fatalf("identity failed after rate window: %v", e)
	}
}

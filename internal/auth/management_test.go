package auth

import (
	"context"
	"testing"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
)

func TestEndManagementClosesOnlyOwnersManagement(t *testing.T) {
	s := state(t)
	k, otherKeys := newKeys(t), newKeys(t)
	owner, other := pair(t, s, k), pair(t, s, otherKeys)
	a, b := "https://wol.hubu.wiki", "https://roc.hubu.wiki"
	cookie, _, err := s.Redeem(ticket(t, s, k, owner.DeviceID, a), a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, release, err := s.ProxyContext(context.Background(), cookie, a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	pending := ticket(t, s, k, owner.DeviceID, b)
	challenge, err := s.ManageChallenge(owner.DeviceID, a, 10)
	if err != nil {
		t.Fatal(err)
	}
	otherCookie, _, err := s.Redeem(ticket(t, s, otherKeys, other.DeviceID, a), a)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EndManagement(owner.DeviceID); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("active browser request survived management revocation")
	}
	_, _, err = s.ProxyContext(context.Background(), cookie, a)
	wantFailure(t, err, 401)
	_, _, err = s.Redeem(pending, b)
	wantFailure(t, err, 409)
	msg := protocol.ManageMessage(owner.DeviceID, challenge.ChallengeID, challenge.Nonce, a)
	_, _, err = s.ManageTicket(owner.DeviceID, challenge.ChallengeID, sign(t, k.manage, msg), a)
	wantFailure(t, err, 409)
	_, wakeDevice, releaseWake, err := s.TokenContext(context.Background(), owner.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseWake()
	if wakeDevice != owner.DeviceID {
		t.Fatal("wake identity changed")
	}
	_, releaseOther, err := s.ProxyContext(context.Background(), otherCookie, a)
	if err != nil {
		t.Fatal("another device was revoked:", err)
	}
	defer releaseOther()
	if err = s.EndManagement(owner.DeviceID); err != nil {
		t.Fatal("repeated revocation failed:", err)
	}
	fresh, _, err := s.Redeem(ticket(t, s, k, owner.DeviceID, a), a)
	if err != nil {
		t.Fatal("fresh management signature could not authorize:", err)
	}
	_, releaseFresh, err := s.ProxyContext(context.Background(), fresh, a)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFresh()
}

func TestManagementDurationBoundToChallengeAndExpiresAtBoundary(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	owner := pair(t, s, k)
	origin := "https://wol.hubu.wiki"
	start := s.now()
	s.now = func() time.Time { return start }
	short, err := s.ManageChallenge(owner.DeviceID, origin, 5)
	if err != nil {
		t.Fatal(err)
	}
	// Selecting a longer duration for a later challenge must not extend this one.
	if _, err := s.ManageChallenge(owner.DeviceID, origin, 30); err != nil {
		t.Fatal(err)
	}
	msg := protocol.ManageMessage(owner.DeviceID, short.ChallengeID, short.Nonce, origin)
	opaque, _, err := s.ManageTicket(owner.DeviceID, short.ChallengeID, sign(t, k.manage, msg), origin)
	if err != nil {
		t.Fatal(err)
	}
	cookie, expires, err := s.Redeem(opaque, origin)
	if err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("granted lifetime changed: %v", expires.Sub(start))
	}
	s.now = func() time.Time { return expires.Add(-time.Nanosecond) }
	_, release, err := s.ProxyContext(context.Background(), cookie, origin)
	if err != nil {
		t.Fatal("session expired early:", err)
	}
	release()
	s.now = func() time.Time { return expires }
	_, _, err = s.ProxyContext(context.Background(), cookie, origin)
	wantFailure(t, err, 401)
}

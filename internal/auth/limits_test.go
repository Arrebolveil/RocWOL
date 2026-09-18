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

func TestInvalidPublicFloodPreservesClientAdmission(t *testing.T) {
	for _, mode := range []string{"empty-pair-code", "invalid-pair-code", "missing-pair-challenge", "unknown-device", "unknown-auth-completion", "known-device-missing-challenge", "wrong-challenge-owner", "wrong-challenge-kind"} {
		t.Run(mode, func(t *testing.T) {
			s := state(t)
			now := time.Now().UTC()
			s.now = func() time.Time { return now }
			k := newKeys(t)
			owner := pair(t, s, k)
			pending, err := s.AuthChallenge(owner.DeviceID)
			if err != nil {
				t.Fatal(err)
			}
			limited := false
			for range 100 {
				var err error
				switch mode {
				case "empty-pair-code":
					_, err = s.PairChallenge("", "", "", "")
				case "invalid-pair-code":
					_, err = s.PairChallenge("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "attacker", k.wp, k.mp)
				case "missing-pair-challenge":
					_, err = s.PairComplete("missing", "", "")
				case "unknown-device":
					_, err = s.AuthChallenge("unknown")
				case "unknown-auth-completion":
					_, err = s.AuthComplete("unknown", "missing", "")
				case "known-device-missing-challenge":
					_, err = s.AuthComplete(owner.DeviceID, "missing", "")
				case "wrong-challenge-owner":
					_, err = s.AuthComplete("unknown", pending.ChallengeID, "")
				case "wrong-challenge-kind":
					_, err = s.PairComplete(pending.ChallengeID, "", "")
				}
				f, ok := err.(*Failure)
				if !ok {
					t.Fatalf("invalid public request was not rejected: %v", err)
				}
				limited = limited || f.Status == 429
			}
			if !limited {
				t.Fatal("invalid public flood was not rate limited")
			}
			other := pair(t, s, k)
			sig := sign(t, k.wake, protocol.WakeMessage(owner.DeviceID, pending.ChallengeID, pending.Nonce))
			if _, err := s.AuthComplete(owner.DeviceID, pending.ChallengeID, sig); err != nil {
				t.Fatalf("public flood blocked pending authentication: %v", err)
			}
			fresh, err := s.AuthChallenge(owner.DeviceID)
			if err != nil {
				t.Fatalf("public flood blocked new authentication: %v", err)
			}
			credentials, err := s.AuthComplete(owner.DeviceID, fresh.ChallengeID, sign(t, k.wake, protocol.WakeMessage(owner.DeviceID, fresh.ChallengeID, fresh.Nonce)))
			if err != nil {
				t.Fatal(err)
			}
			if device, err := s.Token(credentials.AccessToken); err != nil || device != owner.DeviceID {
				t.Fatalf("authentication did not issue usable credentials: %q %v", device, err)
			}
			origin := "https://wol.hubu.wiki"
			finite := authorize(t, s, k, owner.DeviceID, origin, 5, false)
			permanent := authorize(t, s, k, other.DeviceID, origin, 0, false)
			if !finite.Active || !finite.ExpiresAt.Equal(now.Add(5*time.Minute)) || !permanent.Active || !permanent.ExpiresAt.IsZero() {
				t.Fatalf("flood changed management lifetime: finite=%+v permanent=%+v", finite, permanent)
			}
			now = now.Add(time.Minute)
			_, err = s.AuthComplete(owner.DeviceID, pending.ChallengeID, sig)
			wantFailure(t, err, 409)
			now = now.Add(4 * time.Minute)
			finite, err = s.ManagementStatus(owner.DeviceID)
			if err != nil || finite.Active {
				t.Fatalf("finite management did not expire: %+v %v", finite, err)
			}
			permanent, err = s.ManagementStatus(other.DeviceID)
			if err != nil || !permanent.Active || !permanent.ExpiresAt.IsZero() {
				t.Fatalf("permanent management expired: %+v %v", permanent, err)
			}
		})
	}
}

func TestRejectedDeviceFloodPreservesOtherClientAdmission(t *testing.T) {
	s := state(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	k := newKeys(t)
	attacker, owner := pair(t, s, k), pair(t, s, k)
	for i := range 100 {
		_, err := s.AuthChallenge(attacker.DeviceID)
		if i < 10 {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			wantFailure(t, err, 429)
		}
	}
	c, err := s.AuthChallenge(owner.DeviceID)
	if err != nil {
		t.Fatalf("exhausted device spent another client's global budget: %v", err)
	}
	_, err = s.AuthComplete(owner.DeviceID, c.ChallengeID, sign(t, k.wake, protocol.WakeMessage(owner.DeviceID, c.ChallengeID, c.Nonce)))
	if err != nil {
		t.Fatal(err)
	}
}

func TestFailedAuthSignaturesKeepDeviceBound(t *testing.T) {
	s := state(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	k := newKeys(t)
	owner := pair(t, s, k)
	for range 5 {
		c, err := s.AuthChallenge(owner.DeviceID)
		if err != nil {
			t.Fatal(err)
		}
		msg := protocol.WakeMessage(owner.DeviceID, c.ChallengeID, c.Nonce)
		_, err = s.AuthComplete(owner.DeviceID, c.ChallengeID, sign(t, k.manage, msg))
		wantFailure(t, err, 401)
		_, err = s.AuthComplete(owner.DeviceID, c.ChallengeID, sign(t, k.wake, msg))
		wantFailure(t, err, 409)
	}
	_, err := s.AuthChallenge(owner.DeviceID)
	wantFailure(t, err, 429)
}

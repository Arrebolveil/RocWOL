package auth

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
)

const administrationOrigin = "https://wol.hubu.wiki"

func administrator(t *testing.T, s *State, k keys) protocol.Credentials {
	t.Helper()
	owner := pair(t, s, k)
	if e := s.SetDeviceAdministration(owner.DeviceID, true); e != nil {
		t.Fatal(e)
	}
	return owner
}

func administrationChallenge(t *testing.T, s *State, id, action, target string) protocol.Challenge {
	t.Helper()
	c, e := s.DeviceAdministrationChallenge(id, administrationOrigin, action, target)
	if e != nil {
		t.Fatal(e)
	}
	return c
}

func TestDeviceAdministrationRequiresSeparateCapability(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	owner := pair(t, s, k)
	authorize(t, s, k, owner.DeviceID, administrationOrigin, 0, true)
	access, e := s.DeviceAdministrationAccess(owner.DeviceID)
	if e != nil || access.DeviceID != owner.DeviceID || access.DeviceAdministration {
		t.Fatalf("pairing or LuCI authorization elevated administration: %+v %v", access, e)
	}
	for _, action := range []string{"list", "revoke"} {
		target := ""
		if action == "revoke" {
			target = owner.DeviceID
		}
		_, e = s.DeviceAdministrationChallenge(owner.DeviceID, administrationOrigin, action, target)
		wantFailure(t, e, 403)
	}
	_, e = s.ListDevices(owner.DeviceID, protocol.ID(), "", administrationOrigin)
	wantFailure(t, e, 403)
	_, e = s.RevokeDevice(owner.DeviceID, protocol.ID(), "", administrationOrigin, owner.DeviceID)
	wantFailure(t, e, 403)
	if _, e = s.Token(owner.AccessToken); e != nil {
		t.Fatal("denied administration revoked ordinary wake access:", e)
	}
}

func TestDeviceAdministrationSignatureBindingsConsumeAttempt(t *testing.T) {
	for _, mode := range []string{"wake-key", "other-manage-key", "domain", "service", "device", "challenge", "nonce", "signed-origin", "signed-action", "signed-target", "request-origin", "request-action", "request-target", "empty-signature"} {
		t.Run(mode, func(t *testing.T) {
			s := state(t)
			k := newKeys(t)
			owner := administrator(t, s, k)
			target := pair(t, s, newKeys(t))
			c := administrationChallenge(t, s, owner.DeviceID, "revoke", target.DeviceID)
			parts := []string{"ROCWOL-DEVICE-ADMIN-V1", "roc-wol", owner.DeviceID, c.ChallengeID, c.Nonce, administrationOrigin, "revoke", target.DeviceID}
			key := k.manage
			requestOrigin, requestTarget := administrationOrigin, target.DeviceID
			switch mode {
			case "wake-key":
				key = k.wake
			case "other-manage-key":
				key = newKeys(t).manage
			case "domain":
				parts[0] = "ROCWOL-MANAGE-V2"
			case "service":
				parts[1] = "another-service"
			case "device":
				parts[2] = target.DeviceID
			case "challenge":
				parts[3] = protocol.ID()
			case "nonce":
				parts[4] = protocol.Random(32)
			case "signed-origin":
				parts[5] = "https://roc.hubu.wiki"
			case "signed-action":
				parts[6] = "list"
			case "signed-target":
				parts[7] = owner.DeviceID
			case "request-origin":
				requestOrigin = "https://roc.hubu.wiki"
			case "request-target":
				requestTarget = owner.DeviceID
			}
			sig := sign(t, key, strings.Join(parts, "\n"))
			if mode == "empty-signature" {
				sig = ""
			}
			var e error
			if mode == "request-action" {
				_, e = s.ListDevices(owner.DeviceID, c.ChallengeID, sig, requestOrigin)
			} else {
				_, e = s.RevokeDevice(owner.DeviceID, c.ChallengeID, sig, requestOrigin, requestTarget)
			}
			wantFailure(t, e, 403)
			correct := sign(t, k.manage, protocol.DeviceAdministrationMessage(owner.DeviceID, c.ChallengeID, c.Nonce, administrationOrigin, "revoke", target.DeviceID))
			_, e = s.RevokeDevice(owner.DeviceID, c.ChallengeID, correct, administrationOrigin, target.DeviceID)
			wantFailure(t, e, 409)
			for _, token := range []string{owner.AccessToken, target.AccessToken} {
				if _, e = s.Token(token); e != nil {
					t.Fatal("invalid proof revoked a device:", e)
				}
			}
		})
	}
}

func TestDeviceAdministrationCannotStealAnotherOwnersChallenge(t *testing.T) {
	s := state(t)
	k, otherKeys := newKeys(t), newKeys(t)
	owner, other := administrator(t, s, k), administrator(t, s, otherKeys)
	c := administrationChallenge(t, s, owner.DeviceID, "list", "")
	_, e := s.ListDevices(other.DeviceID, c.ChallengeID, sign(t, otherKeys.manage,
		protocol.DeviceAdministrationMessage(other.DeviceID, c.ChallengeID, c.Nonce, administrationOrigin, "list", "")), administrationOrigin)
	wantFailure(t, e, 403)
	proof := sign(t, k.manage, protocol.DeviceAdministrationMessage(owner.DeviceID, c.ChallengeID, c.Nonce, administrationOrigin, "list", ""))
	rows, e := s.ListDevices(owner.DeviceID, c.ChallengeID, proof, administrationOrigin)
	if e != nil || len(rows.Devices) != 2 {
		t.Fatalf("other caller consumed owner's challenge: %+v %v", rows, e)
	}
	_, e = s.ListDevices(owner.DeviceID, c.ChallengeID, proof, administrationOrigin)
	wantFailure(t, e, 409)
}

func TestDeviceAdministrationRateRejectionPreservesChallenge(t *testing.T) {
	for _, bucket := range []string{"device", "global"} {
		t.Run(bucket, func(t *testing.T) {
			s := state(t)
			k := newKeys(t)
			owner := administrator(t, s, k)
			now := s.now()
			s.now = func() time.Time { return now }
			c := administrationChallenge(t, s, owner.DeviceID, "list", "")
			key, limit := "device:"+owner.DeviceID, 10
			if bucket == "global" {
				key, limit = "global", 60
			}
			s.rates[key] = window{Start: now.Add(-59 * time.Second), Count: limit}
			sig := sign(t, k.manage, protocol.DeviceAdministrationMessage(owner.DeviceID, c.ChallengeID, c.Nonce, administrationOrigin, "list", ""))
			_, e := s.ListDevices(owner.DeviceID, c.ChallengeID, sig, administrationOrigin)
			wantFailure(t, e, 429)
			now = now.Add(time.Second)
			rows, e := s.ListDevices(owner.DeviceID, c.ChallengeID, sig, administrationOrigin)
			if e != nil || len(rows.Devices) != 1 || !rows.Devices[0].Current {
				t.Fatalf("rate rejection spent challenge: %+v %v", rows, e)
			}
		})
	}
}

func TestDeviceAdministrationExpiryAndCapabilityWithdrawal(t *testing.T) {
	for _, mode := range []string{"expiry", "withdrawal", "withdrawal-and-regrant"} {
		t.Run(mode, func(t *testing.T) {
			s := state(t)
			k := newKeys(t)
			owner := administrator(t, s, k)
			authorize(t, s, k, owner.DeviceID, administrationOrigin, 0, true)
			c := administrationChallenge(t, s, owner.DeviceID, "list", "")
			status := 409
			if mode == "expiry" {
				s.now = func() time.Time { return c.ExpiresAt }
			} else {
				if e := s.SetDeviceAdministration(owner.DeviceID, false); e != nil {
					t.Fatal(e)
				}
				status = 403
				if mode == "withdrawal-and-regrant" {
					if e := s.SetDeviceAdministration(owner.DeviceID, true); e != nil {
						t.Fatal(e)
					}
					status = 409
				}
			}
			_, e := s.ListDevices(owner.DeviceID, c.ChallengeID, sign(t, k.manage,
				protocol.DeviceAdministrationMessage(owner.DeviceID, c.ChallengeID, c.Nonce, administrationOrigin, "list", "")), administrationOrigin)
			wantFailure(t, e, status)
			if _, e = s.Token(owner.AccessToken); e != nil {
				t.Fatal("capability withdrawal changed wake identity:", e)
			}
			grant, e := s.ManagementStatus(owner.DeviceID)
			if e != nil || !grant.Active {
				t.Fatalf("capability withdrawal changed LuCI grant: %+v %v", grant, e)
			}
		})
	}
}

func TestDeviceAdministrationRevocationClosesAllTargetGrants(t *testing.T) {
	for _, self := range []bool{false, true} {
		t.Run(map[bool]string{false: "other", true: "self"}[self], func(t *testing.T) {
			s := state(t)
			ownerKeys, targetKeys, survivorKeys := newKeys(t), newKeys(t), newKeys(t)
			owner := administrator(t, s, ownerKeys)
			target := owner
			if self {
				targetKeys = ownerKeys
			} else {
				target = administrator(t, s, targetKeys)
			}
			survivor := pair(t, s, survivorKeys)
			authorize(t, s, targetKeys, target.DeviceID, administrationOrigin, 0, true)
			cookie, _, e := s.Redeem(browserTicket(t, s, target.DeviceID, administrationOrigin), administrationOrigin)
			if e != nil {
				t.Fatal(e)
			}
			pendingTicket := browserTicket(t, s, target.DeviceID, administrationOrigin)
			cookieContext, releaseCookie, e := s.ProxyContext(context.Background(), cookie, administrationOrigin)
			if e != nil {
				t.Fatal(e)
			}
			defer releaseCookie()
			anonymousContext, releaseAnonymous, e := s.ProxyContext(context.Background(), "", administrationOrigin)
			if e != nil {
				t.Fatal(e)
			}
			defer releaseAnonymous()
			tokenContext, _, releaseToken, e := s.TokenContext(context.Background(), target.AccessToken)
			if e != nil {
				t.Fatal(e)
			}
			defer releaseToken()
			pendingWake, e := s.AuthChallenge(target.DeviceID)
			if e != nil {
				t.Fatal(e)
			}
			pendingManage, e := s.ManageChallenge(target.DeviceID, administrationOrigin, 10, false)
			if e != nil {
				t.Fatal(e)
			}
			c := administrationChallenge(t, s, owner.DeviceID, "revoke", target.DeviceID)
			sig := sign(t, ownerKeys.manage, protocol.DeviceAdministrationMessage(owner.DeviceID, c.ChallengeID, c.Nonce, administrationOrigin, "revoke", target.DeviceID))
			result, e := s.RevokeDevice(owner.DeviceID, c.ChallengeID, sig, administrationOrigin, target.DeviceID)
			if e != nil || !result.Revoked || result.Current != self || result.DeviceID != target.DeviceID {
				t.Fatalf("revocation receipt: %+v %v", result, e)
			}
			for _, ctx := range []context.Context{tokenContext, cookieContext, anonymousContext} {
				if ctx.Err() == nil {
					t.Fatal("target request survived revocation")
				}
			}
			_, e = s.Token(target.AccessToken)
			wantFailure(t, e, 401)
			_, _, e = s.ProxyContext(context.Background(), cookie, administrationOrigin)
			wantFailure(t, e, 401)
			_, _, e = s.ProxyContext(context.Background(), "", administrationOrigin)
			wantFailure(t, e, 401)
			_, _, e = s.Redeem(pendingTicket, administrationOrigin)
			wantFailure(t, e, 409)
			_, e = s.AuthComplete(target.DeviceID, pendingWake.ChallengeID, sign(t, targetKeys.wake,
				protocol.WakeMessage(target.DeviceID, pendingWake.ChallengeID, pendingWake.Nonce)))
			wantFailure(t, e, 409)
			_, e = s.AuthorizeManagement(target.DeviceID, pendingManage.ChallengeID, sign(t, targetKeys.manage,
				protocol.ManageMessage(target.DeviceID, pendingManage.ChallengeID, pendingManage.Nonce, administrationOrigin, 10, false)), administrationOrigin)
			wantFailure(t, e, 409)
			_, e = s.AuthChallenge(target.DeviceID)
			wantFailure(t, e, 401)
			_, e = s.DeviceAdministrationAccess(target.DeviceID)
			wantFailure(t, e, 401)
			if _, e = s.Token(survivor.AccessToken); e != nil {
				t.Fatal("unrelated device was revoked:", e)
			}
			// Local recovery remains possible even after the last administrator
			// intentionally revokes itself; a revoked identity cannot be revived.
			if e = s.SetDeviceAdministration(target.DeviceID, true); e == nil {
				t.Fatal("capability grant resurrected a revoked identity")
			}
			if e = s.SetDeviceAdministration(survivor.DeviceID, true); e != nil {
				t.Fatal(e)
			}
			fresh := administrationChallenge(t, s, survivor.DeviceID, "list", "")
			rows, e := s.ListDevices(survivor.DeviceID, fresh.ChallengeID, sign(t, survivorKeys.manage,
				protocol.DeviceAdministrationMessage(survivor.DeviceID, fresh.ChallengeID, fresh.Nonce, administrationOrigin, "list", "")), administrationOrigin)
			if e != nil {
				t.Fatal(e)
			}
			for _, d := range rows.Devices {
				if d.DeviceID == target.DeviceID || d.Current != (d.DeviceID == survivor.DeviceID) {
					t.Fatalf("revoked device or wrong current identity in list: %+v", d)
				}
			}
			encoded, e := json.Marshal(rows)
			if e != nil || strings.Contains(string(encoded), "PublicKey") || strings.Contains(string(encoded), targetKeys.mp) || strings.Contains(string(encoded), survivorKeys.wp) {
				t.Fatal("device listing leaked public keys")
			}
		})
	}
}

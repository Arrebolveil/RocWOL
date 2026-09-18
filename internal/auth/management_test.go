package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
)

func TestEndManagementClosesOnlyOwnersManagement(t *testing.T) {
	s := state(t)
	k, otherKeys := newKeys(t), newKeys(t)
	owner, other := pair(t, s, k), pair(t, s, otherKeys)
	a := "https://wol.hubu.wiki"
	cookie, _, err := s.Redeem(ticket(t, s, k, owner.DeviceID, a), a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, release, err := s.ProxyContext(context.Background(), cookie, a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	pending := browserTicket(t, s, owner.DeviceID, a)
	challenge, err := s.ManageChallenge(owner.DeviceID, a, 10, false)
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
	_, _, err = s.Redeem(pending, a)
	wantFailure(t, err, 409)
	msg := protocol.ManageMessage(owner.DeviceID, challenge.ChallengeID, challenge.Nonce, a, 10, false)
	_, err = s.AuthorizeManagement(owner.DeviceID, challenge.ChallengeID, sign(t, k.manage, msg), a)
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
	now := start
	s.now = func() time.Time { return now }
	short, err := s.ManageChallenge(owner.DeviceID, origin, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	// A later selection cannot change the signed challenge's duration or scope.
	if _, err := s.ManageChallenge(owner.DeviceID, origin, 30, false); err != nil {
		t.Fatal(err)
	}
	msg := protocol.ManageMessage(owner.DeviceID, short.ChallengeID, short.Nonce, origin, 5, true)
	grant, err := s.AuthorizeManagement(owner.DeviceID, short.ChallengeID, sign(t, k.manage, msg), origin)
	if err != nil {
		t.Fatal(err)
	}
	expires := start.Add(5 * time.Minute)
	if !grant.Active || !grant.ExpiresAt.Equal(expires) || !grant.AllowAllDevices || grant.SessionMinutes != 5 {
		t.Fatalf("unexpected authorization: %+v", grant)
	}
	anonymous, releaseAnonymous, err := s.ProxyContext(context.Background(), "", origin)
	if err != nil {
		t.Fatal("grant required browser redemption:", err)
	}
	defer releaseAnonymous()
	now = start.Add(2 * time.Minute)
	status, err := s.ManagementStatus(owner.DeviceID)
	if err != nil || !status.ExpiresAt.Equal(expires) {
		t.Fatalf("status extended expiry: %+v %v", status, err)
	}
	link, err := s.ManagementBrowser(owner.DeviceID, origin)
	if err != nil {
		t.Fatal(err)
	}
	if !link.ExpiresAt.Equal(now.Add(time.Minute)) || !link.AuthorizationExpiresAt.Equal(expires) {
		t.Fatalf("ticket lifetime changed authorization: %+v", link)
	}
	now = now.Add(30 * time.Second)
	cookie, cookieExpiry, err := s.Redeem(strings.TrimPrefix(link.URL, origin+"/_wol/manage#t="), origin)
	if err != nil || !cookieExpiry.Equal(expires) {
		t.Fatalf("redemption extended expiry: %v %v", cookieExpiry, err)
	}
	now = expires.Add(-time.Nanosecond)
	ctx, release, err := s.ProxyContext(context.Background(), cookie, origin)
	if err != nil {
		t.Fatal("session expired early:", err)
	}
	defer release()
	bounded, err := s.ManagementBrowser(owner.DeviceID, origin)
	if err != nil || !bounded.ExpiresAt.Equal(expires) {
		t.Fatalf("ticket exceeded authorization deadline: %+v %v", bounded, err)
	}
	now = expires
	status, err = s.ManagementStatus(owner.DeviceID)
	if err != nil || status.Active {
		t.Fatalf("expired status remains active: %+v %v", status, err)
	}
	if ctx.Err() == nil || anonymous.Err() == nil {
		t.Fatal("expired authorization left admitted requests open")
	}
	_, _, err = s.ProxyContext(context.Background(), cookie, origin)
	wantFailure(t, err, 401)
	_, _, err = s.ProxyContext(context.Background(), "", origin)
	wantFailure(t, err, 401)
	_, err = s.ManagementBrowser(owner.DeviceID, origin)
	wantFailure(t, err, 403)
}

func TestManagementSignatureBindsDurationScopeAndOrigin(t *testing.T) {
	for _, mode := range []string{"duration", "scope", "origin"} {
		t.Run(mode, func(t *testing.T) {
			s := state(t)
			k := newKeys(t)
			owner := pair(t, s, k)
			origin := "https://wol.hubu.wiki"
			c, err := s.ManageChallenge(owner.DeviceID, origin, 10, false)
			if err != nil {
				t.Fatal(err)
			}
			minutes, all, signedOrigin := 10, false, origin
			switch mode {
			case "duration":
				minutes = 30
			case "scope":
				all = true
			case "origin":
				signedOrigin = "https://roc.hubu.wiki"
			}
			msg := protocol.ManageMessage(owner.DeviceID, c.ChallengeID, c.Nonce, signedOrigin, minutes, all)
			_, err = s.AuthorizeManagement(owner.DeviceID, c.ChallengeID, sign(t, k.manage, msg), origin)
			wantFailure(t, err, 403)
			status, err := s.ManagementStatus(owner.DeviceID)
			if err != nil || status.Active {
				t.Fatalf("invalid signature granted access: %+v %v", status, err)
			}
		})
	}
}

func TestAnonymousAccessRequiresOptInAndExactOrigin(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	owner := pair(t, s, k)
	a, b := "https://wol.hubu.wiki", "https://roc.hubu.wiki"
	_, err := s.ManagementBrowser(owner.DeviceID, a)
	wantFailure(t, err, 403)
	authorize(t, s, k, owner.DeviceID, a, 10, false)
	_, _, err = s.ProxyContext(context.Background(), "", a)
	wantFailure(t, err, 401)
	authorize(t, s, k, owner.DeviceID, a, 10, true)
	_, release, err := s.ProxyContext(context.Background(), "", a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, _, err = s.ProxyContext(context.Background(), "", b)
	wantFailure(t, err, 401)
	_, err = s.ManagementBrowser(owner.DeviceID, b)
	wantFailure(t, err, 403)
}

func TestAnonymousRequestsCloseOnEndRevokeAndReplacement(t *testing.T) {
	for _, mode := range []string{"end", "revoke", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			s := state(t)
			k, otherK := newKeys(t), newKeys(t)
			owner, other := pair(t, s, k), pair(t, s, otherK)
			a, b := "https://wol.hubu.wiki", "https://roc.hubu.wiki"
			authorize(t, s, k, owner.DeviceID, a, 10, true)
			authorize(t, s, otherK, other.DeviceID, b, 10, true)
			ctx, release, err := s.ProxyContext(context.Background(), "", a)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			otherCtx, otherRelease, err := s.ProxyContext(context.Background(), "", b)
			if err != nil {
				t.Fatal(err)
			}
			defer otherRelease()
			wakeCtx, _, wakeRelease, err := s.TokenContext(context.Background(), owner.AccessToken)
			if err != nil {
				t.Fatal(err)
			}
			defer wakeRelease()
			switch mode {
			case "end":
				err = s.EndManagement(owner.DeviceID)
			case "revoke":
				err = s.Revoke(owner.DeviceID)
			case "replacement":
				authorize(t, s, k, owner.DeviceID, b, 10, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			if ctx.Err() == nil {
				t.Fatal("anonymous request survived owner cancellation")
			}
			if otherCtx.Err() != nil {
				t.Fatal("another owner's grant was cancelled")
			}
			if (wakeCtx.Err() != nil) != (mode == "revoke") {
				t.Fatal("wake cancellation did not match device revocation")
			}
			_, _, err = s.ProxyContext(context.Background(), "", a)
			wantFailure(t, err, 401)
		})
	}
}

func TestBrowserTicketReplacementAndRateLimit(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	owner := pair(t, s, k)
	origin := "https://wol.hubu.wiki"
	grant := authorize(t, s, k, owner.DeviceID, origin, 10, false)
	stale := browserTicket(t, s, owner.DeviceID, origin)
	fresh := browserTicket(t, s, owner.DeviceID, origin)
	_, _, err := s.Redeem(stale, origin)
	wantFailure(t, err, 409)
	_, expires, err := s.Redeem(fresh, origin)
	if err != nil || !expires.Equal(grant.ExpiresAt) {
		t.Fatalf("newest ticket failed: %v %v", expires, err)
	}
	for range 8 {
		browserTicket(t, s, owner.DeviceID, origin)
	}
	_, err = s.ManagementBrowser(owner.DeviceID, origin)
	wantFailure(t, err, 429)
	status, err := s.ManagementStatus(owner.DeviceID)
	if err != nil || !status.Active || !status.ExpiresAt.Equal(grant.ExpiresAt) {
		t.Fatalf("ticket limit changed authorization: %+v %v", status, err)
	}
}

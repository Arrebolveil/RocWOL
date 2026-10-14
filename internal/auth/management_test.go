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
	a := "https://wol.example"
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
	origin := "https://wol.example"
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
			origin := "https://wol.example"
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
				signedOrigin = "https://backup.example"
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
	a, b := "https://wol.example", "https://backup.example"
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
			a, b := "https://wol.example", "https://backup.example"
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
	origin := "https://wol.example"
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

func TestPermanentManagementLifecycle(t *testing.T) {
	for _, mode := range []string{"end", "revoke", "replacement", "close"} {
		t.Run(mode, func(t *testing.T) {
			s := state(t)
			k, otherK := newKeys(t), newKeys(t)
			owner, other := pair(t, s, k), pair(t, s, otherK)
			origin := "https://wol.example"
			now := s.now()
			s.now = func() time.Time { return now }
			grant := authorize(t, s, k, owner.DeviceID, origin, 0, true)
			if !grant.Active || grant.SessionMinutes != 0 || !grant.ExpiresAt.IsZero() {
				t.Fatalf("permanent authorization has a deadline: %+v", grant)
			}
			cookie, expires, err := s.Redeem(browserTicket(t, s, owner.DeviceID, origin), origin)
			if err != nil || !expires.IsZero() {
				t.Fatalf("permanent browser session: %v %v", expires, err)
			}
			var contexts []context.Context
			for _, session := range []string{"", cookie} {
				ctx, release, err := s.ProxyContext(context.Background(), session, origin)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				if _, hasDeadline := ctx.Deadline(); hasDeadline || ctx.Err() != nil {
					t.Fatal("permanent proxy context has an expiry")
				}
				contexts = append(contexts, ctx)
			}
			now = now.AddDate(200, 0, 0)
			status, err := s.ManagementStatus(owner.DeviceID)
			if err != nil || !status.Active || !status.ExpiresAt.IsZero() {
				t.Fatalf("cleanup expired permanent grant: %+v %v", status, err)
			}
			_, _, _, err = s.TokenContext(context.Background(), owner.AccessToken)
			wantFailure(t, err, 401)
			ctx, release, err := s.ProxyContext(context.Background(), cookie, origin)
			if err != nil || ctx.Err() != nil {
				t.Fatalf("cleanup removed permanent browser session: %v", err)
			}
			defer release()
			contexts = append(contexts, ctx)
			pending := browserTicket(t, s, owner.DeviceID, origin)
			challenge, err := s.ManageChallenge(owner.DeviceID, origin, 0, true)
			if err != nil {
				t.Fatal(err)
			}
			authorize(t, s, otherK, other.DeviceID, origin, 0, false)
			otherCookie, _, err := s.Redeem(browserTicket(t, s, other.DeviceID, origin), origin)
			if err != nil {
				t.Fatal(err)
			}
			otherCtx, releaseOther, err := s.ProxyContext(context.Background(), otherCookie, origin)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseOther()
			switch mode {
			case "end":
				err = s.EndManagement(owner.DeviceID)
			case "revoke":
				err = s.Revoke(owner.DeviceID)
			case "replacement":
				authorize(t, s, k, owner.DeviceID, "https://backup.example", 10, false)
			case "close":
				s.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, ctx := range contexts {
				if ctx.Err() != context.Canceled {
					t.Fatal("permanent request survived authorization cancellation")
				}
			}
			if (otherCtx.Err() != nil) != (mode == "close") {
				t.Fatal("cancellation affected unrelated owner")
			}
			for _, session := range []string{"", cookie} {
				_, _, err = s.ProxyContext(context.Background(), session, origin)
				wantFailure(t, err, 401)
			}
			_, _, err = s.Redeem(pending, origin)
			wantFailure(t, err, 409)
			msg := protocol.ManageMessage(owner.DeviceID, challenge.ChallengeID, challenge.Nonce, origin, 0, true)
			_, err = s.AuthorizeManagement(owner.DeviceID, challenge.ChallengeID, sign(t, k.manage, msg), origin)
			wantFailure(t, err, 409)
		})
	}
}

func TestPermanentAuthorizationStillBoundsTicketsAndChallenges(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	owner := pair(t, s, k)
	origin := "https://wol.example"
	now := s.now()
	s.now = func() time.Time { return now }
	authorize(t, s, k, owner.DeviceID, origin, 0, false)
	link, err := s.ManagementBrowser(owner.DeviceID, origin)
	if err != nil || !link.ExpiresAt.Equal(now.Add(time.Minute)) || !link.AuthorizationExpiresAt.IsZero() {
		t.Fatalf("permanent authorization made ticket permanent: %+v %v", link, err)
	}
	c, err := s.ManageChallenge(owner.DeviceID, origin, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	_, _, err = s.Redeem(strings.TrimPrefix(link.URL, origin+"/_wol/manage#t="), origin)
	wantFailure(t, err, 409)
	_, err = s.AuthorizeManagement(owner.DeviceID, c.ChallengeID, sign(t, k.manage, protocol.ManageMessage(owner.DeviceID, c.ChallengeID, c.Nonce, origin, 0, true)), origin)
	wantFailure(t, err, 409)
	cookie, _, err := s.Redeem(browserTicket(t, s, owner.DeviceID, origin), origin)
	if err != nil {
		t.Fatal(err)
	}
	now = now.AddDate(200, 0, 0)
	_, release, err := s.ProxyContext(context.Background(), cookie, origin)
	if err != nil {
		t.Fatal("expired ticket or challenge canceled existing grant:", err)
	}
	defer release()
}

func TestPermanentManagementRejectsAlteredSignature(t *testing.T) {
	for _, mode := range []string{"duration", "scope", "origin", "wake-key"} {
		t.Run(mode, func(t *testing.T) {
			s := state(t)
			k := newKeys(t)
			owner := pair(t, s, k)
			origin := "https://wol.example"
			c, err := s.ManageChallenge(owner.DeviceID, origin, 0, true)
			if err != nil {
				t.Fatal(err)
			}
			minutes, all, signedOrigin, key := 0, true, origin, k.manage
			switch mode {
			case "duration":
				minutes = 30
			case "scope":
				all = false
			case "origin":
				signedOrigin = "https://backup.example"
			case "wake-key":
				key = k.wake
			}
			msg := protocol.ManageMessage(owner.DeviceID, c.ChallengeID, c.Nonce, signedOrigin, minutes, all)
			_, err = s.AuthorizeManagement(owner.DeviceID, c.ChallengeID, sign(t, key, msg), origin)
			wantFailure(t, err, 403)
			_, _, err = s.ProxyContext(context.Background(), "", origin)
			wantFailure(t, err, 401)
		})
	}
}

func TestAnonymousOwnerPrefersFiniteDeadlineOverPermanent(t *testing.T) {
	s := state(t)
	k, otherK := newKeys(t), newKeys(t)
	owner, other := pair(t, s, k), pair(t, s, otherK)
	origin := "https://wol.example"
	authorize(t, s, k, owner.DeviceID, origin, 0, true)
	finite := authorize(t, s, otherK, other.DeviceID, origin, 5, true)
	ctx, release, err := s.ProxyContext(context.Background(), "", origin)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(finite.ExpiresAt) {
		t.Fatalf("anonymous request chose permanent owner over finite deadline: %v", deadline)
	}
	if err := s.EndManagement(other.DeviceID); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("finite owner's request survived revocation")
	}
	remaining, releaseRemaining, err := s.ProxyContext(context.Background(), "", origin)
	if err != nil {
		t.Fatal("unrelated permanent owner was revoked:", err)
	}
	defer releaseRemaining()
	if _, ok := remaining.Deadline(); ok || remaining.Err() != nil {
		t.Fatal("remaining permanent owner has a deadline")
	}
}

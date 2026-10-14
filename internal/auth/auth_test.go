package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
	"hubu.wiki/roc-wol/internal/store"
)

type keys struct {
	wake, manage *ecdsa.PrivateKey
	wp, mp       string
}

func newKeys(t *testing.T) keys {
	t.Helper()
	w, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	m, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	wb, _ := x509.MarshalPKIXPublicKey(&w.PublicKey)
	mb, _ := x509.MarshalPKIXPublicKey(&m.PublicKey)
	return keys{w, m, base64.RawURLEncoding.EncodeToString(wb), base64.RawURLEncoding.EncodeToString(mb)}
}
func sign(t *testing.T, k *ecdsa.PrivateKey, msg string) string {
	t.Helper()
	h := sha256.Sum256([]byte(msg))
	b, e := ecdsa.SignASN1(rand.Reader, k, h[:])
	if e != nil {
		t.Fatal(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func state(t *testing.T) *State {
	t.Helper()
	db, e := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, 5)
}
func pair(t *testing.T, s *State, k keys) protocol.Credentials {
	t.Helper()
	code, e := s.PairCode(time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	c, e := s.PairChallenge(code, "test phone", k.wp, k.mp)
	if e != nil {
		t.Fatal(e)
	}
	msg := protocol.PairMessage(c.ChallengeID, c.Nonce, k.wp, k.mp)
	creds, e := s.PairComplete(c.ChallengeID, sign(t, k.wake, msg), sign(t, k.manage, msg))
	if e != nil {
		t.Fatal(e)
	}
	return creds
}
func wantFailure(t *testing.T, e error, status int) {
	t.Helper()
	var f *Failure
	if !errors.As(e, &f) || f.Status != status {
		t.Fatalf("want failure %d; got %v", status, e)
	}
}
func ticket(t *testing.T, s *State, k keys, id, origin string) string {
	t.Helper()
	authorize(t, s, k, id, origin, 10, false)
	return browserTicket(t, s, id, origin)
}

func authorize(t *testing.T, s *State, k keys, id, origin string, minutes int, all bool) protocol.ManagementAuthorization {
	t.Helper()
	c, e := s.ManageChallenge(id, origin, minutes, all)
	if e != nil {
		t.Fatal(e)
	}
	v, e := s.AuthorizeManagement(id, c.ChallengeID, sign(t, k.manage, protocol.ManageMessage(id, c.ChallengeID, c.Nonce, origin, minutes, all)), origin)
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func browserTicket(t *testing.T, s *State, id, origin string) string {
	t.Helper()
	v, e := s.ManagementBrowser(id, origin)
	if e != nil {
		t.Fatal(e)
	}
	return strings.TrimPrefix(v.URL, origin+"/_wol/manage#t=")
}

func TestWakeKeyCannotGrantManagementAndConsumesChallenge(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	creds := pair(t, s, k)
	origin := "https://wol.example"
	c, e := s.ManageChallenge(creds.DeviceID, origin, 10, false)
	if e != nil {
		t.Fatal(e)
	}
	msg := protocol.ManageMessage(creds.DeviceID, c.ChallengeID, c.Nonce, origin, 10, false)
	_, e = s.AuthorizeManagement(creds.DeviceID, c.ChallengeID, sign(t, k.wake, msg), origin)
	wantFailure(t, e, 403)
	_, e = s.AuthorizeManagement(creds.DeviceID, c.ChallengeID, sign(t, k.manage, msg), origin)
	wantFailure(t, e, 409)
}
func TestPairCodeAndChallengeHaveSingleWinner(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	code, e := s.PairCode(time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	a, e := s.PairChallenge(code, "one", k.wp, k.mp)
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.PairChallenge(code, "two", k.wp, k.mp)
	if e != nil {
		t.Fatal(e)
	}
	type submission struct{ id, w, m string }
	submissions := []submission{}
	for _, c := range []protocol.Challenge{a, a, b} {
		msg := protocol.PairMessage(c.ChallengeID, c.Nonce, k.wp, k.mp)
		submissions = append(submissions, submission{c.ChallengeID, sign(t, k.wake, msg), sign(t, k.manage, msg)})
	}
	var wg sync.WaitGroup
	results := make(chan error, 3)
	for _, v := range submissions {
		wg.Add(1)
		go func(v submission) { defer wg.Done(); _, e := s.PairComplete(v.id, v.w, v.m); results <- e }(v)
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else {
			wantFailure(t, e, 409)
		}
	}
	if success != 1 {
		t.Fatalf("pair registration winners=%d", success)
	}
}
func TestBadPairSignatureBurnsOnlyChallenge(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	code, _ := s.PairCode(time.Minute)
	c, _ := s.PairChallenge(code, "phone", k.wp, k.mp)
	msg := protocol.PairMessage(c.ChallengeID, c.Nonce, k.wp, k.mp)
	_, e := s.PairComplete(c.ChallengeID, sign(t, k.wake, msg), sign(t, k.wake, msg))
	wantFailure(t, e, 403)
	_, e = s.PairChallenge(code, "phone", k.wp, k.mp)
	if e != nil {
		t.Fatalf("pair code should remain usable: %v", e)
	}
}
func TestTicketOriginReplayAndSessionReplacement(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	creds := pair(t, s, k)
	a, b := "https://wol.example", "https://backup.example"
	tk := ticket(t, s, k, creds.DeviceID, a)
	_, _, e := s.Redeem(tk, b)
	wantFailure(t, e, 403)
	cookie, _, e := s.Redeem(tk, a)
	if e != nil {
		t.Fatal(e)
	}
	_, _, e = s.Redeem(tk, a)
	wantFailure(t, e, 409)
	ctx, release, e := s.ProxyContext(context.Background(), cookie, a)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	_, _, e = s.ProxyContext(context.Background(), cookie, b)
	wantFailure(t, e, 401)
	other, _, e := s.Redeem(ticket(t, s, k, creds.DeviceID, b), b)
	if e != nil {
		t.Fatal(e)
	}
	otherCtx, otherRelease, e := s.ProxyContext(context.Background(), other, b)
	if e != nil {
		t.Fatal(e)
	}
	defer otherRelease()
	_, _, e = s.Redeem(ticket(t, s, k, creds.DeviceID, a), a)
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("replacement did not cancel old proxy")
	}
	select {
	case <-otherCtx.Done():
	default:
		t.Fatal("old authorization on another origin survived replacement")
	}
	_, _, e = s.ProxyContext(context.Background(), cookie, a)
	wantFailure(t, e, 401)
}
func TestRevokeInvalidatesEveryGrantAndActiveProxy(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	creds := pair(t, s, k)
	otherK := newKeys(t)
	other := pair(t, s, otherK)
	origin := "https://wol.example"
	cookie, _, e := s.Redeem(ticket(t, s, k, creds.DeviceID, origin), origin)
	if e != nil {
		t.Fatal(e)
	}
	ctx, release, e := s.ProxyContext(context.Background(), cookie, origin)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	tk := browserTicket(t, s, creds.DeviceID, origin)
	c, e := s.AuthChallenge(creds.DeviceID)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Revoke(creds.DeviceID); e != nil {
		t.Fatal(e)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("active proxy survived revocation")
	}
	_, e = s.Token(creds.AccessToken)
	wantFailure(t, e, 401)
	_, _, e = s.Redeem(tk, origin)
	wantFailure(t, e, 409)
	_, _, e = s.ProxyContext(context.Background(), cookie, origin)
	wantFailure(t, e, 401)
	_, e = s.AuthComplete(creds.DeviceID, c.ChallengeID, sign(t, k.wake, protocol.WakeMessage(creds.DeviceID, c.ChallengeID, c.Nonce)))
	wantFailure(t, e, 409)
	if _, e = s.Token(other.AccessToken); e != nil {
		t.Fatalf("unrelated device affected: %v", e)
	}
}
func TestAuthenticationReplayAndExpiry(t *testing.T) {
	s := state(t)
	k := newKeys(t)
	creds := pair(t, s, k)
	c, e := s.AuthChallenge(creds.DeviceID)
	if e != nil {
		t.Fatal(e)
	}
	sig := sign(t, k.wake, protocol.WakeMessage(creds.DeviceID, c.ChallengeID, c.Nonce))
	_, e = s.AuthComplete(creds.DeviceID, c.ChallengeID, sig)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.AuthComplete(creds.DeviceID, c.ChallengeID, sig)
	wantFailure(t, e, 409)
	now := time.Now().UTC()
	s.now = func() time.Time { return now.Add(6 * time.Minute) }
	_, e = s.Token(creds.AccessToken)
	wantFailure(t, e, 401)
}

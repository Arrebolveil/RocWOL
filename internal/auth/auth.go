package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"hubu.wiki/roc-wol/internal/protocol"
	"hubu.wiki/roc-wol/internal/store"
)

type Failure struct {
	Status int
	Code   string
}

func (e *Failure) Error() string         { return e.Code }
func fail(status int, code string) error { return &Failure{status, code} }

type challenge struct {
	protocol.Challenge
	Kind, Device, Origin, CodeHash, Name, Wake, Manage string
	SessionTTL                                         time.Duration
}
type grant struct {
	Device, Origin string
	Expires        time.Time
	SessionTTL     time.Duration
}
type window struct {
	Start time.Time
	Count int
}
type State struct {
	mu                        sync.Mutex
	db                        *store.Store
	challenges                map[string]challenge
	tokens, tickets, sessions map[string]grant
	rates                     map[string]window
	active                    map[string]map[string]context.CancelFunc
	now                       func() time.Time
	MaxSessionTTL             time.Duration
}

func New(db *store.Store) *State {
	return &State{db: db, challenges: map[string]challenge{}, tokens: map[string]grant{}, tickets: map[string]grant{}, sessions: map[string]grant{}, rates: map[string]window{}, active: map[string]map[string]context.CancelFunc{}, now: func() time.Time { return time.Now().UTC() }, MaxSessionTTL: 30 * time.Minute}
}
func (s *State) clean() {
	now := s.now()
	for k, v := range s.challenges {
		if !v.ExpiresAt.After(now) {
			delete(s.challenges, k)
		}
	}
	for _, m := range []map[string]grant{s.tokens, s.tickets, s.sessions} {
		for k, v := range m {
			if !v.Expires.After(now) {
				delete(m, k)
			}
		}
	}
	for k, v := range s.rates {
		if now.Sub(v.Start) >= time.Minute {
			delete(s.rates, k)
		}
	}
}
func (s *State) rate(key string, max int) bool {
	w := s.rates[key]
	now := s.now()
	if now.Sub(w.Start) >= time.Minute {
		w = window{Start: now}
	}
	if w.Count >= max {
		return false
	}
	w.Count++
	s.rates[key] = w
	return true
}
func (s *State) challengeLimit(device string) error {
	s.clean()
	if !s.rate("global", 60) {
		return fail(429, "rate_limited")
	}
	if device != "" && !s.rate("device:"+device, 10) {
		return fail(429, "rate_limited")
	}
	if len(s.challenges) >= 1024 {
		return fail(429, "rate_limited")
	}
	return nil
}
func (s *State) validDevice(id string) (store.Device, error) {
	d, e := s.db.Device(id)
	if e != nil || d.Revoked {
		return d, fail(401, "unauthorized")
	}
	return d, nil
}
func (s *State) newChallenge(c challenge, ttl time.Duration) protocol.Challenge {
	c.Challenge = protocol.Challenge{ChallengeID: protocol.ID(), Nonce: protocol.Random(32), ExpiresAt: s.now().Add(ttl)}
	if c.Kind == "pair" {
		c.ServiceID = protocol.ServiceID
	}
	s.challenges[c.ChallengeID] = c
	return c.Challenge
}
func (s *State) consume(id, kind, device string) (challenge, error) {
	c, ok := s.challenges[id]
	if !ok || !c.ExpiresAt.After(s.now()) {
		delete(s.challenges, id)
		return c, fail(409, "challenge_consumed_or_expired")
	}
	if c.Kind != kind || c.Device != device {
		return c, fail(403, "forbidden")
	}
	delete(s.challenges, id)
	return c, nil
}
func (s *State) credentials(device string) protocol.Credentials {
	token := protocol.Random(32)
	expires := s.now().Add(300 * time.Second)
	s.tokens[protocol.Hash(token)] = grant{Device: device, Expires: expires}
	return protocol.Credentials{DeviceID: device, AccessToken: token, ExpiresAt: expires}
}
func (s *State) PairCode(ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > 24*time.Hour {
		return "", errors.New("ttl must be between zero and 24h")
	}
	b := make([]byte, 20)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.db.AddCode(protocol.Hash(code), s.now().Add(ttl))
	return code, e
}
func (s *State) PairChallenge(code, name, wake, manage string) (protocol.Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.challengeLimit(""); e != nil {
		return protocol.Challenge{}, e
	}
	if len(code) != 32 || !s.db.CodeValid(protocol.Hash(code), s.now()) {
		return protocol.Challenge{}, fail(401, "unauthorized")
	}
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 64 {
		return protocol.Challenge{}, fail(400, "invalid_request")
	}
	wk, _, e := protocol.PublicKey(wake)
	if e != nil {
		return protocol.Challenge{}, fail(400, "invalid_request")
	}
	mk, _, e := protocol.PublicKey(manage)
	if e != nil || wk.Equal(mk) {
		return protocol.Challenge{}, fail(400, "invalid_request")
	}
	return s.newChallenge(challenge{Kind: "pair", CodeHash: protocol.Hash(code), Name: name, Wake: wake, Manage: manage}, 120*time.Second), nil
}
func (s *State) PairComplete(id, wakeSig, manageSig string) (protocol.Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.rate("global", 60) {
		return protocol.Credentials{}, fail(429, "rate_limited")
	}
	c, e := s.consume(id, "pair", "")
	if e != nil {
		return protocol.Credentials{}, e
	}
	msg := protocol.PairMessage(id, c.Nonce, c.Wake, c.Manage)
	if !protocol.Verify(c.Wake, wakeSig, msg) || !protocol.Verify(c.Manage, manageSig, msg) {
		return protocol.Credentials{}, fail(403, "forbidden")
	}
	d := store.Device{DeviceID: protocol.ID(), DeviceName: c.Name, WakeKey: c.Wake, ManageKey: c.Manage, CreatedAt: s.now()}
	if e = s.db.Register(c.CodeHash, d, s.now()); e != nil {
		if errors.Is(e, store.ErrCapacity) {
			return protocol.Credentials{}, fail(429, "rate_limited")
		}
		if errors.Is(e, store.ErrUnavailable) {
			return protocol.Credentials{}, fail(409, "challenge_consumed_or_expired")
		}
		return protocol.Credentials{}, e
	}
	return s.credentials(d.DeviceID), nil
}
func (s *State) AuthChallenge(device string) (protocol.Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.challengeLimit(device); e != nil {
		return protocol.Challenge{}, e
	}
	if _, e := s.validDevice(device); e != nil {
		return protocol.Challenge{}, e
	}
	return s.newChallenge(challenge{Kind: "wake", Device: device}, time.Minute), nil
}
func (s *State) AuthComplete(device, id, sig string) (protocol.Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clean()
	if !s.rate("global", 60) || !s.rate("device:"+device, 10) {
		return protocol.Credentials{}, fail(429, "rate_limited")
	}
	c, e := s.consume(id, "wake", device)
	if e != nil {
		return protocol.Credentials{}, e
	}
	d, e := s.validDevice(device)
	if e != nil {
		return protocol.Credentials{}, e
	}
	if !protocol.Verify(d.WakeKey, sig, protocol.WakeMessage(device, id, c.Nonce)) {
		return protocol.Credentials{}, fail(401, "unauthorized")
	}
	return s.credentials(device), nil
}
func (s *State) Token(token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clean()
	g, ok := s.tokens[protocol.Hash(token)]
	if !ok || !g.Expires.After(s.now()) {
		return "", fail(401, "unauthorized")
	}
	if _, e := s.validDevice(g.Device); e != nil {
		return "", e
	}
	return g.Device, nil
}

// TokenContext admits a request atomically with revocation. Revoke cancels
// requests still reading a body or waiting for their upstream operation.
func (s *State) TokenContext(parent context.Context, token string) (context.Context, string, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clean()
	key := protocol.Hash(token)
	g, ok := s.tokens[key]
	if !ok || !g.Expires.After(s.now()) {
		return nil, "", nil, fail(401, "unauthorized")
	}
	if _, e := s.validDevice(g.Device); e != nil {
		return nil, "", nil, e
	}
	ctx, release := s.grantContext(parent, key, g)
	return ctx, g.Device, release, nil
}
func (s *State) ManageChallenge(device, origin string, sessionMinutes int) (protocol.Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionMinutes != 5 && sessionMinutes != 10 && sessionMinutes != 15 && sessionMinutes != 30 {
		return protocol.Challenge{}, fail(400, "invalid_request")
	}
	if e := s.challengeLimit(device); e != nil {
		return protocol.Challenge{}, e
	}
	if _, e := s.validDevice(device); e != nil {
		return protocol.Challenge{}, e
	}
	// The signed challenge ID/nonce binds this immutable duration; ticket and
	// redemption requests cannot select a different lifetime.
	return s.newChallenge(challenge{Kind: "manage", Device: device, Origin: origin, SessionTTL: time.Duration(sessionMinutes) * time.Minute}, time.Minute), nil
}
func (s *State) ManageTicket(device, id, sig, origin string) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, e := s.consume(id, "manage", device)
	if e != nil {
		return "", time.Time{}, e
	}
	d, e := s.validDevice(device)
	if e != nil {
		return "", time.Time{}, e
	}
	if c.Origin != origin || !protocol.Verify(d.ManageKey, sig, protocol.ManageMessage(device, id, c.Nonce, c.Origin)) {
		return "", time.Time{}, fail(403, "forbidden")
	}
	ticket := protocol.Random(32)
	expires := s.now().Add(time.Minute)
	s.tickets[protocol.Hash(ticket)] = grant{Device: device, Origin: origin, Expires: expires, SessionTTL: c.SessionTTL}
	return ticket, expires, nil
}
func (s *State) Redeem(ticket, origin string) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clean()
	key := protocol.Hash(ticket)
	g, ok := s.tickets[key]
	if !ok || !g.Expires.After(s.now()) {
		return "", time.Time{}, fail(409, "challenge_consumed_or_expired")
	}
	if g.Origin != origin {
		return "", time.Time{}, fail(403, "forbidden")
	}
	delete(s.tickets, key)
	if _, e := s.validDevice(g.Device); e != nil {
		return "", time.Time{}, e
	}
	for k, old := range s.sessions {
		if old.Device == g.Device && old.Origin == origin {
			delete(s.sessions, k)
			s.cancelSession(k)
		}
	}
	cookie := protocol.Random(32)
	g.Expires = s.now().Add(min(g.SessionTTL, s.MaxSessionTTL))
	s.sessions[protocol.Hash(cookie)] = g
	return cookie, g.Expires, nil
}
func (s *State) cancelSession(key string) {
	for _, cancel := range s.active[key] {
		cancel()
	}
	delete(s.active, key)
}

// ProxyContext checks and registers atomically with revocation so a revoked
// device cannot create a new untracked request after the initial cookie check.
func (s *State) ProxyContext(parent context.Context, cookie, origin string) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := protocol.Hash(cookie)
	g, ok := s.sessions[key]
	if !ok || g.Origin != origin || !g.Expires.After(s.now()) {
		return nil, nil, fail(401, "unauthorized")
	}
	if _, e := s.validDevice(g.Device); e != nil {
		return nil, nil, e
	}
	ctx, release := s.grantContext(parent, key, g)
	return ctx, release, nil
}

// Caller holds mu; both bearer and cookie grants share revocation tracking.
func (s *State) grantContext(parent context.Context, key string, g grant) (context.Context, func()) {
	ctx, cancel := context.WithDeadline(parent, g.Expires)
	id := protocol.ID()
	if s.active[key] == nil {
		s.active[key] = map[string]context.CancelFunc{}
	}
	s.active[key][id] = cancel
	return ctx, func() {
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.active[key], id)
		if len(s.active[key]) == 0 {
			delete(s.active, key)
		}
	}
}

// EndManagement closes only this device's management grants, across both origins.
// Wake credentials stay valid; pending challenges cannot reopen a closed session.
func (s *State) EndManagement(device string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.validDevice(device); err != nil {
		return err
	}
	for key, c := range s.challenges {
		if c.Device == device && c.Kind == "manage" {
			delete(s.challenges, key)
		}
	}
	for _, grants := range []map[string]grant{s.tickets, s.sessions} {
		for key, g := range grants {
			if g.Device == device {
				delete(grants, key)
				s.cancelSession(key)
			}
		}
	}
	return nil
}
func (s *State) Revoke(device string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.db.Revoke(device); e != nil {
		return e
	}
	for k, c := range s.challenges {
		if c.Device == device {
			delete(s.challenges, k)
		}
	}
	for _, m := range []map[string]grant{s.tokens, s.tickets, s.sessions} {
		for k, g := range m {
			if g.Device == device {
				delete(m, k)
				s.cancelSession(k)
			}
		}
	}
	return nil
}
func (s *State) Sweep(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.clean()
			s.mu.Unlock()
		}
	}
}
func (s *State) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.active {
		s.cancelSession(key)
	}
	clear(s.tokens)
	clear(s.tickets)
	clear(s.sessions)
	clear(s.challenges)
}

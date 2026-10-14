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
	SessionMinutes                                     int
	AllowAllDevices                                    bool
	Action, Target                                     string
}
type grant struct {
	Device, Origin, Authorization string
	Expires                       time.Time
}
type managementGrant struct {
	protocol.ManagementAuthorization
	Key string
}
type window struct {
	Start time.Time
	Count int
}
type State struct {
	mu                        sync.Mutex
	db                        *store.Store
	maxDevices                int
	challenges                map[string]challenge
	tokens, tickets, sessions map[string]grant
	management                map[string]managementGrant
	rates                     map[string]window
	active                    map[string]map[string]context.CancelFunc
	now                       func() time.Time
	MaxSessionTTL             time.Duration
}

// New retains the configured capacity; gateway configuration owns defaults and
// validation. Store.Register independently rejects capacities outside 1–100.
func New(db *store.Store, maxDevices int) *State {
	return &State{db: db, maxDevices: maxDevices, challenges: map[string]challenge{}, tokens: map[string]grant{}, tickets: map[string]grant{}, sessions: map[string]grant{}, management: map[string]managementGrant{}, rates: map[string]window{}, active: map[string]map[string]context.CancelFunc{}, now: func() time.Time { return time.Now().UTC() }, MaxSessionTTL: 30 * time.Minute}
}
func (s *State) clean() {
	now := s.now()
	for device, g := range s.management {
		if !g.ExpiresAt.IsZero() && !g.ExpiresAt.After(now) {
			s.closeManagement(device)
		}
	}
	for k, v := range s.challenges {
		if !v.ExpiresAt.After(now) {
			delete(s.challenges, k)
		}
	}
	for _, m := range []map[string]grant{s.tokens, s.tickets} {
		for k, v := range m {
			if !v.Expires.After(now) {
				delete(m, k)
				s.cancelSession(k)
			}
		}
	}
	for key, g := range s.sessions {
		if !g.Expires.IsZero() && !g.Expires.After(now) {
			delete(s.sessions, key)
			s.cancelSession(key)
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

// Invalid public requests must not spend another client's admission budget.
// A single bucket also avoids allocating rate entries for attacker-chosen IDs.
func (s *State) rejectPublic(err error) error {
	if !s.rate("invalid", 60) {
		return fail(429, "rate_limited")
	}
	return err
}
func (s *State) challengeLimit(device string) error {
	s.clean()
	if len(s.challenges) >= 1024 {
		return fail(429, "rate_limited")
	}
	if device != "" && !s.rate("device:"+device, 10) {
		return fail(429, "rate_limited")
	}
	if !s.rate("global", 60) {
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
func (s *State) lookupChallenge(id, kind, device string) (challenge, error) {
	c, ok := s.challenges[id]
	if !ok || !c.ExpiresAt.After(s.now()) {
		delete(s.challenges, id)
		return c, fail(409, "challenge_consumed_or_expired")
	}
	if c.Kind != kind || c.Device != device {
		return c, fail(403, "forbidden")
	}
	return c, nil
}
func (s *State) consume(id, kind, device string) (challenge, error) {
	c, err := s.lookupChallenge(id, kind, device)
	if err != nil {
		return c, err
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
	if len(code) != 32 || !s.db.CodeValid(protocol.Hash(code), s.now()) {
		return protocol.Challenge{}, s.rejectPublic(fail(401, "unauthorized"))
	}
	if e := s.challengeLimit(""); e != nil {
		return protocol.Challenge{}, e
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
	c, e := s.lookupChallenge(id, "pair", "")
	if e != nil {
		return protocol.Credentials{}, s.rejectPublic(e)
	}
	if !s.rate("global", 60) {
		return protocol.Credentials{}, fail(429, "rate_limited")
	}
	delete(s.challenges, id)
	msg := protocol.PairMessage(id, c.Nonce, c.Wake, c.Manage)
	if !protocol.Verify(c.Wake, wakeSig, msg) || !protocol.Verify(c.Manage, manageSig, msg) {
		return protocol.Credentials{}, fail(403, "forbidden")
	}
	d := store.Device{DeviceID: protocol.ID(), DeviceName: c.Name, WakeKey: c.Wake, ManageKey: c.Manage, CreatedAt: s.now()}
	if e = s.db.Register(c.CodeHash, d, s.now(), s.maxDevices); e != nil {
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
	if _, e := s.validDevice(device); e != nil {
		return protocol.Challenge{}, s.rejectPublic(e)
	}
	if e := s.challengeLimit(device); e != nil {
		return protocol.Challenge{}, e
	}
	return s.newChallenge(challenge{Kind: "wake", Device: device}, time.Minute), nil
}
func (s *State) AuthComplete(device, id, sig string) (protocol.Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clean()
	c, e := s.lookupChallenge(id, "wake", device)
	if e != nil {
		return protocol.Credentials{}, s.rejectPublic(e)
	}
	d, e := s.validDevice(device)
	if e != nil {
		return protocol.Credentials{}, s.rejectPublic(e)
	}
	if !s.rate("device:"+device, 10) || !s.rate("global", 60) {
		return protocol.Credentials{}, fail(429, "rate_limited")
	}
	delete(s.challenges, id)
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
func (s *State) ManageChallenge(device, origin string, sessionMinutes int, allowAllDevices bool) (protocol.Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionMinutes != 0 && sessionMinutes != 5 && sessionMinutes != 10 && sessionMinutes != 15 && sessionMinutes != 30 {
		return protocol.Challenge{}, fail(400, "invalid_request")
	}
	if _, e := s.validDevice(device); e != nil {
		return protocol.Challenge{}, e
	}
	if e := s.challengeLimit(device); e != nil {
		return protocol.Challenge{}, e
	}
	return s.newChallenge(challenge{Kind: "manage", Device: device, Origin: origin, SessionMinutes: sessionMinutes, AllowAllDevices: allowAllDevices}, time.Minute), nil
}

func (s *State) AuthorizeManagement(device, id, sig, origin string) (protocol.ManagementAuthorization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clean()
	c, e := s.consume(id, "manage", device)
	if e != nil {
		return protocol.ManagementAuthorization{}, e
	}
	d, e := s.validDevice(device)
	if e != nil {
		return protocol.ManagementAuthorization{}, e
	}
	if c.Origin != origin || !protocol.Verify(d.ManageKey, sig, protocol.ManageMessage(device, id, c.Nonce, c.Origin, c.SessionMinutes, c.AllowAllDevices)) {
		return protocol.ManagementAuthorization{}, fail(403, "forbidden")
	}
	s.closeManagement(device)
	s.clearManageChallenges(device)
	var expires time.Time
	if c.SessionMinutes != 0 {
		expires = s.now().Add(min(time.Duration(c.SessionMinutes)*time.Minute, s.MaxSessionTTL, 30*time.Minute))
	}
	g := managementGrant{
		ManagementAuthorization: protocol.ManagementAuthorization{
			Active: true, Origin: origin, SessionMinutes: c.SessionMinutes, AllowAllDevices: c.AllowAllDevices,
			ExpiresAt: expires,
		},
		Key: "manage:" + protocol.Random(32),
	}
	s.management[device] = g
	return g.ManagementAuthorization, nil
}

func (s *State) ManagementStatus(device string) (protocol.ManagementAuthorization, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clean()
	if _, e := s.validDevice(device); e != nil {
		return protocol.ManagementAuthorization{}, e
	}
	return s.management[device].ManagementAuthorization, nil
}

func (s *State) ManagementBrowser(device, origin string) (protocol.ManagementBrowser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clean()
	if _, e := s.validDevice(device); e != nil {
		return protocol.ManagementBrowser{}, e
	}
	g, ok := s.management[device]
	if !ok || g.Origin != origin {
		return protocol.ManagementBrowser{}, fail(403, "forbidden")
	}
	if !s.rate("browser:global", 60) || !s.rate("browser:"+device, 10) {
		return protocol.ManagementBrowser{}, fail(429, "rate_limited")
	}
	// One outstanding ticket per device bounds allocation and invalidates stale links.
	for key, old := range s.tickets {
		if old.Device == device {
			delete(s.tickets, key)
		}
	}
	ticket := protocol.Random(32)
	expires := s.now().Add(time.Minute)
	if !g.ExpiresAt.IsZero() {
		expires = minTime(expires, g.ExpiresAt)
	}
	s.tickets[protocol.Hash(ticket)] = grant{Device: device, Origin: origin, Authorization: g.Key, Expires: expires}
	return protocol.ManagementBrowser{URL: origin + "/_wol/manage#t=" + ticket, ExpiresAt: expires, AuthorizationExpiresAt: g.ExpiresAt}, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
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
	authorization, ok := s.management[g.Device]
	if !ok || authorization.Key != g.Authorization || authorization.Origin != origin {
		return "", time.Time{}, fail(401, "unauthorized")
	}
	for k, old := range s.sessions {
		if old.Device == g.Device && old.Origin == origin {
			delete(s.sessions, k)
			s.cancelSession(k)
		}
	}
	cookie := protocol.Random(32)
	g.Expires = authorization.ExpiresAt
	s.sessions[protocol.Hash(cookie)] = g
	return cookie, g.Expires, nil
}
func (s *State) cancelSession(key string) {
	for _, cancel := range s.active[key] {
		cancel()
	}
	delete(s.active, key)
}

// ProxyContext admits cookie or explicitly authorized anonymous access while
// holding the same lock used by expiry, replacement and owner revocation.
func (s *State) ProxyContext(parent context.Context, cookie, origin string) (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clean()
	key := protocol.Hash(cookie)
	g, ok := s.sessions[key]
	if ok && g.Origin == origin && (g.Expires.IsZero() || g.Expires.After(s.now())) {
		authorization, exists := s.management[g.Device]
		if exists && authorization.Key == g.Authorization && authorization.Origin == origin {
			if _, e := s.validDevice(g.Device); e == nil {
				ctx, release := s.grantContext(parent, key, g)
				return ctx, release, nil
			}
		}
	}
	// Multiple owners may authorize this origin. Attribute each admitted request
	// to one owner, preferring the earliest finite expiry, so cancellation is scoped.
	var owner string
	var selected managementGrant
	for device, authorization := range s.management {
		if authorization.Origin != origin || !authorization.AllowAllDevices {
			continue
		}
		if _, e := s.validDevice(device); e != nil {
			continue
		}
		if owner == "" || !authorization.ExpiresAt.IsZero() && (selected.ExpiresAt.IsZero() || authorization.ExpiresAt.Before(selected.ExpiresAt)) || authorization.ExpiresAt.Equal(selected.ExpiresAt) && device < owner {
			owner, selected = device, authorization
		}
	}
	if owner == "" {
		return nil, nil, fail(401, "unauthorized")
	}
	ctx, release := s.grantContext(parent, selected.Key, grant{Device: owner, Origin: origin, Expires: selected.ExpiresAt})
	return ctx, release, nil
}

// Caller holds mu; both bearer and cookie grants share revocation tracking.
func (s *State) grantContext(parent context.Context, key string, g grant) (context.Context, func()) {
	var ctx context.Context
	var cancel context.CancelFunc
	if g.Expires.IsZero() {
		ctx, cancel = context.WithCancel(parent)
	} else {
		ctx, cancel = context.WithDeadline(parent, g.Expires)
	}
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

// closeManagement requires mu and leaves wake credentials untouched.
func (s *State) closeManagement(device string) {
	if g, ok := s.management[device]; ok {
		s.cancelSession(g.Key)
		delete(s.management, device)
	}
	for _, grants := range []map[string]grant{s.tickets, s.sessions} {
		for key, g := range grants {
			if g.Device == device {
				delete(grants, key)
				s.cancelSession(key)
			}
		}
	}
}

func (s *State) clearManageChallenges(device string) {
	for key, c := range s.challenges {
		if c.Device == device && c.Kind == "manage" {
			delete(s.challenges, key)
		}
	}
}

// EndManagement closes this device's authorization, including anonymous streams.
// Wake credentials stay valid; pending challenges cannot reopen a closed grant.
func (s *State) EndManagement(device string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.validDevice(device); err != nil {
		return err
	}
	s.clearManageChallenges(device)
	s.closeManagement(device)
	return nil
}
func (s *State) Revoke(device string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeLocked(device)
}

// revokeLocked is shared by local recovery and signed device administration.
// The caller holds mu so authorization and revocation are one atomic action.
func (s *State) revokeLocked(device string) error {
	if e := s.db.Revoke(device); e != nil {
		return e
	}
	s.closeManagement(device)
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
	clear(s.management)
	clear(s.challenges)
}

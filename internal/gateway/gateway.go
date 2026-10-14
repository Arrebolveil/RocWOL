package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"hubu.wiki/roc-wol/internal/auth"
	"hubu.wiki/roc-wol/internal/protocol"
	"hubu.wiki/roc-wol/internal/store"
)

type Gateway struct {
	cfg            Config
	DB             *store.Store
	Auth           *auth.State
	client         *http.Client
	proxy          *httputil.ReverseProxy
	origins        map[string]string
	wakeMu         sync.Mutex
	wakeRates      map[string][]time.Time
	freshMu        sync.Mutex
	freshRates     map[string]time.Time
	targetMu       sync.RWMutex
	targetSnapshot map[string]protocol.TargetInfo
}

func New(c Config) (*Gateway, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	tc, e := c.TLSConfig()
	if e != nil {
		return nil, e
	}
	db, e := store.Open(c.DataFile)
	if e != nil {
		return nil, e
	}
	tr := &http.Transport{TLSClientConfig: tc, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 32, ForceAttemptHTTP2: false}
	g := newGateway(c, db, tr)
	return g, nil
}
func newGateway(c Config, db *store.Store, tr http.RoundTripper) *Gateway {
	if c.MaxDevices == 0 {
		c.MaxDevices = 5
	}
	g := &Gateway{cfg: c, DB: db, Auth: auth.New(db, c.MaxDevices), client: &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, origins: map[string]string{}, wakeRates: map[string][]time.Time{}}
	for _, o := range c.PublicOrigins {
		u, _ := url.Parse(o)
		g.origins[u.Host] = o
	}
	g.proxy = g.makeProxy(tr)
	return g
}
func (g *Gateway) Close() error { g.Auth.Close(); g.client.CloseIdleConnections(); return g.DB.Close() }
func (g *Gateway) HTTPServer() *http.Server {
	return &http.Server{Addr: g.cfg.Listen, Handler: g, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: protocol.JSONLimit}
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	headerBytes := len(r.Host) + len(r.Method) + len(r.RequestURI) + 12
	for k, values := range r.Header {
		for _, v := range values {
			headerBytes += len(k) + len(v) + 4
		}
	}
	if headerBytes > protocol.JSONLimit {
		protocol.Fail(w, 431, "invalid_request")
		return
	}
	origin, ok := g.origins[r.Host]
	if !ok {
		protocol.Fail(w, 421, "invalid_request")
		return
	}
	if r.Method == http.MethodConnect || !protocol.SafePath(r.URL) {
		protocol.Fail(w, 400, "invalid_request")
		return
	}
	if !safeMethod(r.Method) || isWebSocket(r) {
		if r.Header.Get("Origin") != origin {
			protocol.Fail(w, 403, "forbidden")
			return
		}
	}
	// Never accept attacker-supplied forwarding or identity headers.
	stripForwarding(r.Header)
	p := r.URL.Path
	if p == "/_roc-agent" || strings.HasPrefix(p, "/_roc-agent/") {
		protocol.Fail(w, 404, "not_found")
		return
	}
	if p == "/_wol/manage" && r.Method == http.MethodGet {
		g.page(w, "manage.html")
		return
	}
	if p == "/_wol/assets/manage.js" && r.Method == http.MethodGet {
		g.page(w, "manage.js")
		return
	}
	if p == "/_wol/assets/style.css" && r.Method == http.MethodGet {
		g.page(w, "style.css")
		return
	}
	if p == "/_wol/v1" || strings.HasPrefix(p, "/_wol/v1/") {
		protocol.Fail(w, 426, "protocol_upgrade_required")
		return
	}
	if strings.HasPrefix(p, "/_wol/v2/") {
		g.api(w, r, origin)
		return
	}
	if p == "/_wol" || strings.HasPrefix(p, "/_wol/") {
		protocol.Fail(w, 404, "not_found")
		return
	}
	var session string
	if cookie, err := r.Cookie("__Host-roc_session"); err == nil {
		session = cookie.Value
	}
	ctx, release, e := g.Auth.ProxyContext(r.Context(), session, origin)
	if e != nil {
		g.page(w, "unauthorized.html", http.StatusUnauthorized)
		return
	}
	defer release()
	enabled, e := g.managementAvailable(r)
	if e != nil {
		protocol.Fail(w, 503, "agent_unavailable")
		return
	}
	if !enabled {
		g.page(w, "unauthorized.html", http.StatusUnauthorized)
		return
	}
	if r.ContentLength > protocol.UploadLimit {
		protocol.Fail(w, 413, "invalid_request")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, protocol.UploadLimit)
	// no-referrer makes browser form-navigation Origin opaque ("null").
	// Preserve same-origin LuCI forms without disclosing cross-origin referrers.
	w.Header().Set("Referrer-Policy", "same-origin")
	g.proxy.ServeHTTP(w, r.WithContext(context.WithValue(ctx, originKey{}, origin)))
}
func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}
func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
func stripForwarding(h http.Header) {
	for k := range h {
		lower := strings.ToLower(k)
		if lower == "forwarded" || strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "cf-") || lower == "x-real-ip" || lower == "x-remote-user" || lower == "remote-user" || lower == "x-auth-request-user" {
			h.Del(k)
		}
	}
}
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	if r.Context().Err() != nil {
		protocol.Fail(w, 401, "unauthorized")
		return false
	}
	if e := protocol.DecodeJSON(r.Body, v, protocol.JSONLimit); e != nil {
		protocol.Fail(w, 400, "invalid_request")
		return false
	}
	if r.Context().Err() != nil {
		protocol.Fail(w, 401, "unauthorized")
		return false
	}
	return true
}
func respondError(w http.ResponseWriter, e error) {
	var f *auth.Failure
	if errors.As(e, &f) {
		if f.Status == 429 {
			w.Header().Set("Retry-After", "60")
		}
		protocol.Fail(w, f.Status, f.Code)
		return
	}
	protocol.Fail(w, 503, "upstream_unavailable")
}
func (g *Gateway) api(w http.ResponseWriter, r *http.Request, origin string) {
	p := r.URL.Path
	isTargetStatus := strings.HasPrefix(p, "/_wol/v2/targets/") && r.Method == "GET"
	if r.URL.RawQuery != "" && !(isTargetStatus && r.URL.RawQuery == "fresh=1") {
		protocol.Fail(w, 400, "invalid_request")
		return
	}
	if p == "/_wol/v2/info" && r.Method == "GET" {
		protocol.JSON(w, 200, protocol.Info{ServiceID: protocol.ServiceID, APIVersion: protocol.APIVersion, InstanceID: g.DB.InstanceID()})
		return
	}
	switch {
	case p == "/_wol/v2/pair/challenge" && r.Method == "POST":
		var v struct {
			Code            string `json:"code"`
			DeviceName      string `json:"deviceName"`
			WakePublicKey   string `json:"wakePublicKey"`
			ManagePublicKey string `json:"managePublicKey"`
		}
		if !body(w, r, &v) {
			return
		}
		result, e := g.Auth.PairChallenge(v.Code, v.DeviceName, v.WakePublicKey, v.ManagePublicKey)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
		return
	case p == "/_wol/v2/pair/complete" && r.Method == "POST":
		var v struct {
			ChallengeID     string `json:"challengeId"`
			WakeSignature   string `json:"wakeSignature"`
			ManageSignature string `json:"manageSignature"`
		}
		if !body(w, r, &v) {
			return
		}
		result, e := g.Auth.PairComplete(v.ChallengeID, v.WakeSignature, v.ManageSignature)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
		return
	case p == "/_wol/v2/auth/challenge" && r.Method == "POST":
		var v struct {
			DeviceID string `json:"deviceId"`
		}
		if !body(w, r, &v) {
			return
		}
		if !protocol.ValidID(v.DeviceID) {
			protocol.Fail(w, 400, "invalid_request")
			return
		}
		result, e := g.Auth.AuthChallenge(v.DeviceID)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
		return
	case p == "/_wol/v2/auth/complete" && r.Method == "POST":
		var v struct {
			DeviceID    string `json:"deviceId"`
			ChallengeID string `json:"challengeId"`
			Signature   string `json:"signature"`
		}
		if !body(w, r, &v) {
			return
		}
		result, e := g.Auth.AuthComplete(v.DeviceID, v.ChallengeID, v.Signature)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
		return
	case p == "/_wol/v2/manage/redeem" && r.Method == "POST":
		if !g.requireManagement(w, r) {
			return
		}
		var v struct {
			Ticket string `json:"ticket"`
		}
		if !body(w, r, &v) {
			return
		}
		cookie, expires, e := g.Auth.Redeem(v.Ticket, origin)
		if e != nil {
			respondError(w, e)
			return
		}
		sessionCookie := &http.Cookie{Name: "__Host-roc_session", Value: cookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
		if !expires.IsZero() {
			sessionCookie.Expires = expires
			sessionCookie.MaxAge = int(time.Until(expires).Seconds())
		}
		http.SetCookie(w, sessionCookie)
		protocol.JSON(w, 200, map[string]string{"redirect": "/cgi-bin/luci/"})
		return
	}
	known := p == "/_wol/v2/targets" && r.Method == "GET" || p == "/_wol/v2/wake" && r.Method == "POST" || strings.HasPrefix(p, "/_wol/v2/wake/") && r.Method == "GET" || p == "/_wol/v2/manage/challenge" && r.Method == "POST" || p == "/_wol/v2/manage/authorize" && r.Method == "POST" || p == "/_wol/v2/manage/browser" && r.Method == "POST" || p == "/_wol/v2/manage/status" && r.Method == "GET" || p == "/_wol/v2/manage/end" && r.Method == "POST"
	known = known || deviceAdministrationRoute(p, r.Method)
	known = known || isTargetStatus
	if !known {
		protocol.Fail(w, 404, "not_found")
		return
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") || strings.Contains(authorization[7:], " ") {
		protocol.Fail(w, 401, "unauthorized")
		return
	}
	ctx, device, release, e := g.Auth.TokenContext(r.Context(), authorization[7:])
	if e != nil {
		respondError(w, e)
		return
	}
	defer release()
	r = r.WithContext(ctx)
	if deviceAdministrationRoute(p, r.Method) {
		g.deviceAdministration(w, r, device, origin)
		return
	}
	if strings.HasPrefix(p, "/_wol/v2/manage/") && p != "/_wol/v2/manage/end" && !g.requireManagement(w, r) {
		return
	}
	switch {
	case p == "/_wol/v2/manage/end" && r.Method == "POST":
		if !body(w, r, &struct{}{}) {
			return
		}
		if e := g.Auth.EndManagement(device); e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, map[string]bool{"ended": true})
	case p == "/_wol/v2/targets" && r.Method == "GET":
		g.targets(w, r)
	case isTargetStatus:
		g.target(w, r, device, strings.TrimPrefix(p, "/_wol/v2/targets/"))
	case p == "/_wol/v2/wake" && r.Method == "POST":
		var v struct {
			TargetID  string `json:"targetId"`
			RequestID string `json:"requestId"`
		}
		if !body(w, r, &v) {
			return
		}
		if !protocol.ValidTargetID(v.TargetID) || !protocol.ValidID(v.RequestID) {
			protocol.Fail(w, 400, "invalid_request")
			return
		}
		g.wake(w, r, device, v.RequestID, v.TargetID, true)
	case strings.HasPrefix(p, "/_wol/v2/wake/") && r.Method == "GET":
		id := strings.TrimPrefix(p, "/_wol/v2/wake/")
		if !protocol.ValidID(id) {
			protocol.Fail(w, 400, "invalid_request")
			return
		}
		g.wake(w, r, device, id, "", false)
	case p == "/_wol/v2/manage/challenge" && r.Method == "POST":
		var v struct {
			Origin          string `json:"origin"`
			SessionMinutes  *int   `json:"sessionMinutes"`
			AllowAllDevices *bool  `json:"allowAllDevices"`
		}
		if !body(w, r, &v) {
			return
		}
		if v.SessionMinutes == nil || v.AllowAllDevices == nil {
			protocol.Fail(w, 400, "invalid_request")
			return
		}
		if v.Origin != origin {
			protocol.Fail(w, 403, "forbidden")
			return
		}
		result, e := g.Auth.ManageChallenge(device, origin, *v.SessionMinutes, *v.AllowAllDevices)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
	case p == "/_wol/v2/manage/authorize" && r.Method == "POST":
		var v struct {
			ChallengeID string `json:"challengeId"`
			Signature   string `json:"signature"`
		}
		if !body(w, r, &v) {
			return
		}
		if !protocol.ValidID(v.ChallengeID) || v.Signature == "" {
			protocol.Fail(w, 400, "invalid_request")
			return
		}
		result, e := g.Auth.AuthorizeManagement(device, v.ChallengeID, v.Signature, origin)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
	case p == "/_wol/v2/manage/status" && r.Method == "GET":
		result, e := g.Auth.ManagementStatus(device)
		if e != nil {
			respondError(w, e)
			return
		}
		if !result.Active {
			protocol.JSON(w, 200, map[string]bool{"active": false})
			return
		}
		protocol.JSON(w, 200, result)
	case p == "/_wol/v2/manage/browser" && r.Method == "POST":
		if !body(w, r, &struct{}{}) {
			return
		}
		result, e := g.Auth.ManagementBrowser(device, origin)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
	default:
		protocol.Fail(w, 404, "not_found")
	}
}
func drainClose(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, protocol.JSONLimit))
	_ = r.Close()
}

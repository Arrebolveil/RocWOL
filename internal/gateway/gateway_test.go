package gateway

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
	"hubu.wiki/roc-wol/internal/store"
)

const testOrigin = "https://wol.hubu.wiki"

type testIdentity struct {
	protocol.Credentials
	manage *ecdsa.PrivateKey
}

func testSign(t *testing.T, k *ecdsa.PrivateKey, msg string) string {
	t.Helper()
	h := sha256.Sum256([]byte(msg))
	b, e := ecdsa.SignASN1(rand.Reader, k, h[:])
	if e != nil {
		t.Fatal(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func identity(t *testing.T, g *Gateway) testIdentity {
	t.Helper()
	wk, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	mk, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	wb, _ := x509.MarshalPKIXPublicKey(&wk.PublicKey)
	mb, _ := x509.MarshalPKIXPublicKey(&mk.PublicKey)
	wp, mp := base64.RawURLEncoding.EncodeToString(wb), base64.RawURLEncoding.EncodeToString(mb)
	code, e := g.Auth.PairCode(time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	c, e := g.Auth.PairChallenge(code, "test phone", wp, mp)
	if e != nil {
		t.Fatal(e)
	}
	msg := protocol.PairMessage(c.ChallengeID, c.Nonce, wp, mp)
	creds, e := g.Auth.PairComplete(c.ChallengeID, testSign(t, wk, msg), testSign(t, mk, msg))
	if e != nil {
		t.Fatal(e)
	}
	return testIdentity{creds, mk}
}
func session(t *testing.T, g *Gateway, id testIdentity, minutes int) string {
	t.Helper()
	authorizeManagement(t, g, id, minutes, false)
	link, e := g.Auth.ManagementBrowser(id.DeviceID, testOrigin)
	if e != nil {
		t.Fatal(e)
	}
	cookie, _, e := g.Auth.Redeem(strings.TrimPrefix(link.URL, testOrigin+"/_wol/manage#t="), testOrigin)
	if e != nil {
		t.Fatal(e)
	}
	return cookie
}

func authorizeManagement(t *testing.T, g *Gateway, id testIdentity, minutes int, all bool) protocol.ManagementAuthorization {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"origin": testOrigin, "sessionMinutes": minutes, "allowAllDevices": all})
	w := request(g, "POST", "/_wol/v1/manage/challenge", id.AccessToken, "", string(payload))
	if w.Code != 200 {
		t.Fatalf("challenge: %d %s", w.Code, w.Body.String())
	}
	var c protocol.Challenge
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	msg := protocol.ManageMessage(id.DeviceID, c.ChallengeID, c.Nonce, testOrigin, minutes, all)
	payload, _ = json.Marshal(map[string]string{"challengeId": c.ChallengeID, "signature": testSign(t, id.manage, msg)})
	w = request(g, "POST", "/_wol/v1/manage/authorize", id.AccessToken, "", string(payload))
	if w.Code != 200 {
		t.Fatalf("authorize: %d %s", w.Code, w.Body.String())
	}
	var result protocol.ManagementAuthorization
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func fixture(t *testing.T, h http.HandlerFunc) *Gateway {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	db, e := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if e != nil {
		t.Fatal(e)
	}
	g := newGateway(Config{PublicOrigins: []string{testOrigin, "https://roc.hubu.wiki"}, AgentURL: up.URL}, db, http.DefaultTransport)
	t.Cleanup(func() { g.Close() })
	return g
}
func request(g *Gateway, method, path, token, cookie, payload string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(payload))
	r.Host = "wol.hubu.wiki"
	r.Header.Set("Origin", testOrigin)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: "__Host-roc_session", Value: cookie})
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}
func TestNoManagementOrAgentBypass(t *testing.T) {
	var hits atomic.Int32
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) })
	id := identity(t, g)
	cookie := session(t, g, id, 10)
	for _, path := range []string{"/", "/cgi-bin/luci/", "/ubus", "/luci-static/resources/view/wol.js"} {
		w := request(g, "GET", path, id.AccessToken, "", "")
		if w.Code != 401 {
			t.Fatalf("unauthorized %s status %d", path, w.Code)
		}
	}
	w := request(g, "GET", "/_roc-agent/v1/status", "", cookie, "")
	if w.Code != 404 {
		t.Fatalf("private API reachable status %d", w.Code)
	}
	for _, path := range []string{"/%5froc-agent/v1/status", "/%2f_roc-agent/v1/status", "/foo/../_roc-agent/v1/status", "/foo%5cbar"} {
		w = request(g, "GET", path, "", cookie, "")
		if w.Code != 400 && w.Code != 404 {
			t.Fatalf("path escaped reserved guard: %s %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "wol.hubu.wiki"
	r.Header.Set("Cookie", "sysauth=pretend; __Host-roc_session=pretend")
	r.Header.Set("X-Forwarded-User", "root")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Origin", testOrigin)
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("forged session accepted: %d", w.Code)
	}
	if hits.Load() != 0 {
		t.Fatalf("unauthorized traffic reached router %d times", hits.Load())
	}
}
func TestOriginAndHostFailClosed(t *testing.T) {
	var hits atomic.Int32
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	id := identity(t, g)
	cookie := session(t, g, id, 10)
	for _, origin := range []string{"", "https://evil.example", "https://roc.hubu.wiki"} {
		r := httptest.NewRequest("POST", "/ubus", strings.NewReader("{}"))
		r.Host = "wol.hubu.wiki"
		r.Header.Set("Origin", origin)
		r.AddCookie(&http.Cookie{Name: "__Host-roc_session", Value: cookie})
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("origin %q accepted: %d", origin, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example"
	r.AddCookie(&http.Cookie{Name: "__Host-roc_session", Value: cookie})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 421 || hits.Load() != 0 {
		t.Fatal("host or origin bypassed gate")
	}
}
func TestWakeDedupAndReconciliationNeverResends(t *testing.T) {
	var sends, queries atomic.Int32
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			sends.Add(1)
			protocol.JSON(w, 504, protocol.Wake{RequestID: strings.Repeat("a", 32), Result: "unknown", Code: "outcome_unknown"})
			return
		}
		queries.Add(1)
		now := time.Now().UTC()
		protocol.JSON(w, 200, protocol.Wake{RequestID: strings.Repeat("a", 32), Result: "sent", SentAt: &now})
	})
	id := identity(t, g)
	payload := `{"targetId":"desktop","requestId":"` + strings.Repeat("a", 32) + `"}`
	w := request(g, "POST", "/_wol/v1/wake", id.AccessToken, "", payload)
	if w.Code != 504 || !strings.Contains(w.Body.String(), `"result":"unknown"`) {
		t.Fatalf("uncertain result not retained: %d %s", w.Code, w.Body.String())
	}
	w = request(g, "POST", "/_wol/v1/wake", id.AccessToken, "", payload)
	if w.Code != 202 {
		t.Fatalf("dedup failed to reconcile %d %s", w.Code, w.Body.String())
	}
	w = request(g, "GET", "/_wol/v1/wake/"+strings.Repeat("a", 32), id.AccessToken, "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if sends.Load() != 1 || queries.Load() != 1 {
		t.Fatalf("side effect repeated: sends=%d queries=%d", sends.Load(), queries.Load())
	}
	other := identity(t, g)
	w = request(g, "GET", "/_wol/v1/wake/"+strings.Repeat("a", 32), other.AccessToken, "", "")
	if w.Code != 404 {
		t.Fatal("another device read wake")
	}
	w = request(g, "GET", "/_wol/v1/wake/"+strings.Repeat("b", 32), id.AccessToken, "", "")
	if w.Code != 404 || sends.Load() != 1 {
		t.Fatal("unknown query dispatched")
	}
}
func TestPersistedPendingOnlyQueriesAfterRestart(t *testing.T) {
	var sends atomic.Int32
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			sends.Add(1)
		}
		w.WriteHeader(404)
	})
	id := identity(t, g)
	rid := strings.Repeat("b", 32)
	if e := g.DB.CreateWake(store.WakeRecord{Wake: protocol.Wake{RequestID: rid, Result: "pending"}, DeviceID: id.DeviceID, CreatedAt: time.Now().UTC()}); e != nil {
		t.Fatal(e)
	}
	restarted := newGateway(g.cfg, g.DB, http.DefaultTransport)
	restarted.Auth = g.Auth
	w := request(restarted, "POST", "/_wol/v1/wake", id.AccessToken, "", `{"targetId":"desktop","requestId":"`+rid+`"}`)
	if w.Code != 504 || sends.Load() != 0 {
		t.Fatalf("persisted pending replay sent a command: %d, %d", w.Code, sends.Load())
	}
}

type gatedBody struct {
	io.Reader
	started, release chan struct{}
	once             sync.Once
}

func (b *gatedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started); <-b.release })
	return b.Reader.Read(p)
}

func TestRevocationDuringWakeBodyPreventsDispatch(t *testing.T) {
	var sends atomic.Int32
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) { sends.Add(1); w.WriteHeader(502) })
	id := identity(t, g)
	rid := strings.Repeat("c", 32)
	body := &gatedBody{Reader: strings.NewReader(`{"targetId":"desktop","requestId":"` + rid + `"}`), started: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("POST", "/_wol/v1/wake", body)
	r.Host = "wol.hubu.wiki"
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Authorization", "Bearer "+id.AccessToken)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); g.ServeHTTP(w, r) }()
	<-body.started
	if err := g.Auth.Revoke(id.DeviceID); err != nil {
		close(body.release)
		<-done
		t.Fatal(err)
	}
	close(body.release)
	<-done
	if w.Code != 401 || sends.Load() != 0 {
		t.Fatalf("revoked request dispatched: status=%d upstream=%d", w.Code, sends.Load())
	}
	if _, err := g.DB.Wake(rid); err == nil {
		t.Fatal("revoked request was admitted to the wake ledger")
	}
}

func TestProxyRejectsBrowserNetworkPathRedirect(t *testing.T) {
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "///evil.example/")
		w.WriteHeader(http.StatusFound)
	})
	id := identity(t, g)
	w := request(g, "GET", "/cgi-bin/luci/", "", session(t, g, id, 10), "")
	if w.Code != 503 || w.Header().Get("Location") != "" {
		t.Fatalf("external browser redirect escaped gateway: %d %q", w.Code, w.Header().Get("Location"))
	}
}
func TestProxyCookieAndRedirectBoundary(t *testing.T) {
	var leaked atomic.Bool
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), "roc_session") || r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.Header().Add("Set-Cookie", "__Host-roc_session=override; Path=/")
		w.Header().Add("Set-Cookie", "sysauth=ok; Domain=127.0.0.1; Path=/; HttpOnly")
		if r.URL.Path == "/external" {
			w.Header().Set("Location", "https://evil.example/")
		} else {
			w.Header().Set("Location", "http://127.0.0.1/cgi-bin/luci/")
		}
		w.WriteHeader(302)
	})
	id := identity(t, g)
	cookie := session(t, g, id, 10)
	w := request(g, "GET", "/cgi-bin/luci/", id.AccessToken, cookie, "")
	if w.Code != 302 || w.Header().Get("Location") != testOrigin+"/cgi-bin/luci/" {
		t.Fatalf("internal redirect not translated: %d %s", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "sysauth" || !cookies[0].Secure || cookies[0].Domain != "" || leaked.Load() {
		t.Fatalf("proxy cookie isolation failed: %+v", cookies)
	}
	w = request(g, "GET", "/external", "", cookie, "")
	if w.Code != 503 || w.Header().Get("Location") != "" {
		t.Fatal("external redirect escaped proxy")
	}
}
func TestWebSocketExpiresAndRevokesAfterHandshake(t *testing.T) {
	for _, mode := range []string{"expires", "revokes", "ends", "anonymous-expires", "anonymous-revokes", "anonymous-ends", "permanent-revokes", "permanent-ends", "anonymous-permanent-revokes", "anonymous-permanent-ends"} {
		t.Run(mode, func(t *testing.T) {
			closed := make(chan struct{}, 1)
			g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, rw, e := w.(http.Hijacker).Hijack()
				if e != nil {
					return
				}
				defer conn.Close()
				_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
				_ = rw.Flush()
				_, _ = io.Copy(io.Discard, conn)
				closed <- struct{}{}
			})
			id := identity(t, g)
			if strings.HasSuffix(mode, "expires") {
				g.Auth.MaxSessionTTL = 500 * time.Millisecond
			}
			minutes := 10
			if strings.Contains(mode, "permanent") {
				minutes = 0
			}
			var cookie string
			if strings.HasPrefix(mode, "anonymous-") {
				authorizeManagement(t, g, id, minutes, true)
			} else {
				cookie = session(t, g, id, minutes)
			}
			server := httptest.NewServer(g)
			defer server.Close()
			conn, e := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
			_, _ = fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: wol.hubu.wiki\r\nOrigin: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nCookie: __Host-roc_session=%s\r\n\r\n", testOrigin, cookie)
			resp, e := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
			if e != nil {
				t.Fatal(e)
			}
			if resp.StatusCode != 101 {
				t.Fatalf("upgrade status %d", resp.StatusCode)
			}
			if strings.HasSuffix(mode, "revokes") {
				if e = g.Auth.Revoke(id.DeviceID); e != nil {
					t.Fatal(e)
				}
			}
			if strings.HasSuffix(mode, "ends") {
				w := request(g, "POST", "/_wol/v1/manage/end", id.AccessToken, "", "{}")
				if w.Code != 200 {
					t.Fatalf("end: %d %s", w.Code, w.Body.String())
				}
			}
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("upgraded upstream survived expiry/revocation")
			}
			buf := make([]byte, 1)
			_, e = conn.Read(buf)
			if e == nil {
				t.Fatal("upgraded client still open")
			}
		})
	}
}
func TestStrictJSONAndPublicRedemption(t *testing.T) {
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream request") })
	for _, payload := range []string{`{"deviceId":"a","deviceId":"b"}`, `{} {}`, `{"extra":true}`, string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'})} {
		w := request(g, "POST", "/_wol/v1/auth/challenge", "", "", payload)
		if w.Code != 400 {
			t.Fatalf("malformed JSON accepted: %d", w.Code)
		}
	}
	id := identity(t, g)
	c, e := g.Auth.ManageChallenge(id.DeviceID, testOrigin, 10, false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = g.Auth.AuthorizeManagement(id.DeviceID, c.ChallengeID, testSign(t, id.manage, protocol.ManageMessage(id.DeviceID, c.ChallengeID, c.Nonce, testOrigin, 10, false)), testOrigin)
	if e != nil {
		t.Fatal(e)
	}
	link, e := g.Auth.ManagementBrowser(id.DeviceID, testOrigin)
	if e != nil {
		t.Fatal(e)
	}
	payload, _ := json.Marshal(map[string]string{"ticket": strings.TrimPrefix(link.URL, testOrigin+"/_wol/manage#t=")})
	w := request(g, "POST", "/_wol/v1/manage/redeem", "", "", string(payload))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 || cs[0].Name != "__Host-roc_session" || !cs[0].HttpOnly || !cs[0].Secure || cs[0].SameSite != http.SameSiteStrictMode || cs[0].Path != "/" || cs[0].Domain != "" {
		t.Fatalf("unsafe management cookie: %+v", cs)
	}
	w = request(g, "POST", "/_wol/v1/manage/redeem", "", "", string(payload))
	if w.Code != 409 {
		t.Fatal("ticket replay accepted")
	}
}

func TestManagementAPIScopeAndAnonymousBoundary(t *testing.T) {
	var hits atomic.Int32
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// The outer grant must not replace LuCI's own authentication.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("LuCI login required"))
	})
	id := identity(t, g)
	if w := request(g, "GET", "/_wol/v1/manage/status", "", "", ""); w.Code != 401 {
		t.Fatalf("unprotected status: %d", w.Code)
	}
	w := request(g, "GET", "/_wol/v1/manage/status", id.AccessToken, "", "")
	var inactive struct {
		Active bool `json:"active"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &inactive); err != nil || w.Code != 200 || inactive.Active {
		t.Fatalf("inactive status: %d %s", w.Code, w.Body.String())
	}
	if w := request(g, "POST", "/_wol/v1/manage/browser", id.AccessToken, "", "{}"); w.Code != 403 {
		t.Fatalf("wake bearer created authorization: %d", w.Code)
	}
	for _, payload := range []string{
		`{"origin":"https://wol.hubu.wiki","sessionMinutes":10}`,
		`{"origin":"https://wol.hubu.wiki","sessionMinutes":10,"allowAllDevices":null}`,
		`{"origin":"https://wol.hubu.wiki","sessionMinutes":null,"allowAllDevices":false}`,
		`{"origin":"https://wol.hubu.wiki","sessionMinutes":10,"allowAllDevices":"true"}`,
		`{"origin":"https://wol.hubu.wiki","sessionMinutes":31,"allowAllDevices":true}`,
		`{"origin":"https://wol.hubu.wiki","sessionMinutes":10,"allowAllDevices":true,"extra":true}`,
	} {
		if w := request(g, "POST", "/_wol/v1/manage/challenge", id.AccessToken, "", payload); w.Code != 400 {
			t.Fatalf("malformed scope accepted: %d %s", w.Code, payload)
		}
	}
	if w := request(g, "POST", "/_wol/v1/manage/authorize", id.AccessToken, "", `{"challengeId":null,"signature":null}`); w.Code != 400 {
		t.Fatalf("missing authorization proof accepted: %d", w.Code)
	}
	authorizeManagement(t, g, id, 10, false)
	if w := request(g, "GET", "/cgi-bin/luci/", "", "", ""); w.Code != 401 || hits.Load() != 0 {
		t.Fatalf("browser-only authorization allowed anonymous access: %d", w.Code)
	}
	grant := authorizeManagement(t, g, id, 10, true)
	if !grant.Active || !grant.AllowAllDevices || grant.Origin != testOrigin {
		t.Fatalf("wrong authorization: %+v", grant)
	}
	w = request(g, "GET", "/cgi-bin/luci/", "", "", "")
	if w.Code != 401 || w.Body.String() != "LuCI login required" || hits.Load() != 1 {
		t.Fatalf("anonymous LuCI admission changed upstream login: %d %s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		method, path, host, origin string
		status                     int
	}{
		{"GET", "/cgi-bin/luci/", "roc.hubu.wiki", "https://roc.hubu.wiki", 401},
		{"POST", "/ubus", "wol.hubu.wiki", "https://evil.example", 403},
		{"GET", "/_roc-agent/v1/status", "wol.hubu.wiki", testOrigin, 404},
		{"GET", "/_wol/v1/manage/status", "wol.hubu.wiki", testOrigin, 401},
		{"POST", "/_wol/v1/manage/browser", "wol.hubu.wiki", testOrigin, 401},
		{"GET", "/_wol/v1/target", "wol.hubu.wiki", testOrigin, 401},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("boundary %s %s: %d", tc.host, tc.path, w.Code)
		}
	}
	if hits.Load() != 1 {
		t.Fatal("protected requests reached upstream")
	}
	w = request(g, "POST", "/_wol/v1/manage/browser", id.AccessToken, "", "{}")
	if w.Code != 200 {
		t.Fatalf("browser ticket: %d %s", w.Code, w.Body.String())
	}
	var link protocol.ManagementBrowser
	if err := json.Unmarshal(w.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}
	if !link.AuthorizationExpiresAt.Equal(grant.ExpiresAt) || link.ExpiresAt.After(grant.ExpiresAt) {
		t.Fatalf("browser request changed expiry: %+v", link)
	}
	for _, path := range []string{"/_wol/v1/manage/browser", "/_wol/v1/manage/end"} {
		if w := request(g, "POST", path, id.AccessToken, "", `{"allowAllDevices":false}`); w.Code != 400 {
			t.Fatalf("unexpected fields accepted by %s: %d", path, w.Code)
		}
	}
	for range 2 {
		if w := request(g, "POST", "/_wol/v1/manage/end", id.AccessToken, "", "{}"); w.Code != 200 {
			t.Fatalf("idempotent end: %d", w.Code)
		}
	}
	w = request(g, "GET", "/cgi-bin/luci/", "", "", "")
	if w.Code != 401 || hits.Load() != 1 {
		t.Fatal("anonymous access survived end")
	}
	if w := request(g, "POST", "/_wol/v1/manage/browser", id.AccessToken, "", "{}"); w.Code != 403 {
		t.Fatal("browser ticket revived ended grant")
	}
}

func TestPermanentBrowserCookieAndWireContract(t *testing.T) {
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("LuCI"))
	})
	id := identity(t, g)
	authorizeManagement(t, g, id, 0, false)
	status := request(g, "GET", "/_wol/v1/manage/status", id.AccessToken, "", "")
	var grant struct {
		Active         bool   `json:"active"`
		SessionMinutes int    `json:"sessionMinutes"`
		ExpiresAt      string `json:"expiresAt"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &grant); err != nil || status.Code != 200 || !grant.Active || grant.SessionMinutes != 0 || grant.ExpiresAt != "0001-01-01T00:00:00Z" {
		t.Fatalf("permanent grant wire contract: %d %s", status.Code, status.Body.String())
	}
	link := request(g, "POST", "/_wol/v1/manage/browser", id.AccessToken, "", "{}")
	var ticket struct {
		URL                    string    `json:"url"`
		ExpiresAt              time.Time `json:"expiresAt"`
		AuthorizationExpiresAt string    `json:"authorizationExpiresAt"`
	}
	if err := json.Unmarshal(link.Body.Bytes(), &ticket); err != nil || link.Code != 200 || ticket.AuthorizationExpiresAt != "0001-01-01T00:00:00Z" || !ticket.ExpiresAt.After(time.Now()) || ticket.ExpiresAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("permanent browser ticket wire contract: %d %s", link.Code, link.Body.String())
	}
	payload, _ := json.Marshal(map[string]string{"ticket": strings.TrimPrefix(ticket.URL, testOrigin+"/_wol/manage#t=")})
	w := request(g, "POST", "/_wol/v1/manage/redeem", "", "", string(payload))
	cookies := w.Result().Cookies()
	if w.Code != 200 || len(cookies) != 1 {
		t.Fatalf("ticket redemption failed: %d %s", w.Code, w.Body.String())
	}
	cookie := cookies[0]
	if cookie.Name != "__Host-roc_session" || cookie.Value == "" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" || !cookie.Expires.IsZero() || cookie.MaxAge != 0 {
		t.Fatalf("permanent grant did not emit a secure session cookie: %+v", cookie)
	}
	if w := request(g, "GET", "/cgi-bin/luci/", "", cookie.Value, ""); w.Code != 200 || w.Body.String() != "LuCI" {
		t.Fatalf("permanent cookie did not admit browser: %d %s", w.Code, w.Body.String())
	}
	if w := request(g, "POST", "/_wol/v1/manage/redeem", "", "", string(payload)); w.Code != 409 {
		t.Fatal("permanent ticket allowed replay")
	}
	if w := request(g, "POST", "/_wol/v1/manage/end", id.AccessToken, "", "{}"); w.Code != 200 {
		t.Fatal("permanent end failed:", w.Code)
	}
	if w := request(g, "GET", "/cgi-bin/luci/", "", cookie.Value, ""); w.Code != 401 {
		t.Fatal("permanent browser survived end:", w.Code)
	}
}

func TestTargetRefreshForwardsOnlyExplicitFreshRequest(t *testing.T) {
	queries := make(chan string, 3)
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_roc-agent/v1/status" {
			t.Error("unexpected upstream path:", r.URL.Path)
		}
		queries <- r.URL.RawQuery
		now := time.Now().UTC()
		protocol.JSON(w, 200, protocol.Status{TargetID: "desktop", Reachability: "reachable", CheckedAt: &now})
	})
	id := identity(t, g)
	for _, tc := range []struct{ path, query string }{
		{"/_wol/v1/target", ""},
		{"/_wol/v1/target?fresh=1&ignored=1", "fresh=1"},
		{"/_wol/v1/target?fresh=0", ""},
	} {
		w := request(g, "GET", tc.path, id.AccessToken, "", "")
		if w.Code != 200 {
			t.Fatalf("target failed: %s: %d %s", tc.path, w.Code, w.Body.String())
		}
		select {
		case query := <-queries:
			if query != tc.query {
				t.Fatalf("incorrect fresh forwarding: %s: %q", tc.path, query)
			}
		default:
			t.Fatal("target did not request agent status")
		}
	}
}

func TestFreshStatusAdmissionLimitsOnlyRequestingDevice(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var freshCalls, cachedCalls atomic.Int32
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fresh") == "1" {
			if freshCalls.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
		} else {
			cachedCalls.Add(1)
		}
		now := time.Now().UTC()
		protocol.JSON(w, 200, protocol.Status{TargetID: "desktop", Reachability: "reachable", CheckedAt: &now})
	})
	id, other := identity(t, g), identity(t, g)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- request(g, "GET", "/_wol/v1/target?fresh=1", id.AccessToken, "", "") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first fresh status request did not reach agent")
	}
	floodDone := make(chan *httptest.ResponseRecorder, 8)
	for range 8 {
		go func() { floodDone <- request(g, "GET", "/_wol/v1/target?fresh=1", id.AccessToken, "", "") }()
	}
	for range 8 {
		select {
		case w := <-floodDone:
			if w.Code != 429 || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), `"code":"rate_limited"`) {
				t.Errorf("fresh flood was not rejected: status=%d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatal("fresh flood waited for the active upstream request")
		}
	}
	for _, path := range []string{"/_wol/v1/target", "/_wol/v1/target?fresh=0"} {
		w := request(g, "GET", path, id.AccessToken, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"reachability":"reachable"`) {
			t.Fatalf("fresh limit blocked a cached query: %d %s", w.Code, w.Body.String())
		}
	}
	w := request(g, "GET", "/_wol/v1/target?fresh=1", other.AccessToken, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reachability":"reachable"`) {
		t.Fatalf("fresh limit blocked another device: %d %s", w.Code, w.Body.String())
	}
	once.Do(func() { close(release) })
	select {
	case w := <-firstDone:
		if w.Code != 200 {
			t.Fatalf("accepted fresh request failed: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("accepted fresh request did not complete")
	}
	if freshCalls.Load() != 2 || cachedCalls.Load() != 2 {
		t.Fatalf("incorrect upstream admissions: fresh=%d cached=%d", freshCalls.Load(), cachedCalls.Load())
	}
	time.Sleep(time.Second)
	w = request(g, "GET", "/_wol/v1/target?fresh=1", id.AccessToken, "", "")
	if w.Code != 200 || freshCalls.Load() != 3 {
		t.Fatalf("fresh admission did not reset: calls=%d status=%d body=%s", freshCalls.Load(), w.Code, w.Body.String())
	}
}

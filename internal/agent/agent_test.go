package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"hubu.wiki/roc-wol/internal/protocol"
)

type runnerFunc func(context.Context, string, ...string) error

func (f runnerFunc) Run(ctx context.Context, path string, args ...string) error {
	return f(ctx, path, args...)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixture struct {
	config        Config
	roots         *x509.CertPool
	client        tls.Certificate
	wrongIdentity tls.Certificate
	state         *tls.ConnectionState
}

func agentFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated agent test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	serial := int64(1)
	issue := func(name string, client bool) (tls.Certificate, string, string) {
		serial++
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
		if client {
			uri, err := url.Parse(name)
			if err != nil {
				t.Fatal(err)
			}
			leaf.URIs, leaf.ExtKeyUsage = []*url.URL{uri}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		} else {
			leaf.DNSNames, leaf.ExtKeyUsage = []string{name}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		certPath, keyPath := filepath.Join(dir, fmt.Sprintf("cert-%d.pem", serial)), filepath.Join(dir, fmt.Sprintf("key-%d.pem", serial))
		if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		pair.Leaf, err = x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return pair, certPath, keyPath
	}
	_, serverCert, serverKey := issue(ServerName, false)
	client, _, _ := issue(GatewayURI, true)
	wrong, _, _ := issue("urn:roc-wol:other", true)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return fixture{
		config: Config{Listen: "127.0.0.1:19991", ServerCertFile: serverCert, ServerKeyFile: serverKey, ClientCAFile: caPath, ClientURI: GatewayURI, DataFile: filepath.Join(dir, "agent.db"), LuCIOrigin: "http://127.0.0.1:80", PingPath: filepath.Join(dir, "ping"), EtherwakePath: filepath.Join(dir, "etherwake"), Targets: []Target{{ID: "desktop", Name: "Desktop", MAC: "02:00:00:00:00:10", Interface: "test-lan", Broadcast: true, IP: "192.0.2.10"}}},
		// Injected runners use host-absolute paths without requiring real tools.
		roots: roots, client: client, wrongIdentity: wrong,
		state: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{client.Leaf}, VerifiedChains: [][]*x509.Certificate{{client.Leaf, ca}}},
	}
}

func openTestAgent(t *testing.T, f fixture, runner Runner) *Agent {
	t.Helper()
	a, err := New(f.config, runner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func request(a *Agent, f fixture, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.TLS = f.state
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func wakeBody(id string) string { return `{"requestId":"` + id + `","targetId":"desktop"}` }
func decodeWake(t *testing.T, w *httptest.ResponseRecorder) WakeResult {
	t.Helper()
	var result WakeResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDuplicateWakeExecutesOnceIncludingInFlightAndCooldown(t *testing.T) {
	f := agentFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	a := openTestAgent(t, f, runnerFunc(func(ctx context.Context, path string, args ...string) error {
		if path != f.config.EtherwakePath || strings.Join(args, " ") != "-i test-lan -b 02:00:00:00:00:10" {
			return errors.New("unexpected executable or arguments")
		}
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	id := strings.Repeat("a", 32)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(a, f, "POST", apiPrefix+"wake", wakeBody(id)) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("command was not started")
	}
	for _, method := range []string{"POST", "GET"} {
		path, body := apiPrefix+"wake", wakeBody(id)
		if method == "GET" {
			path, body = path+"/"+id, ""
		}
		w := request(a, f, method, path, body)
		if result := decodeWake(t, w); result.Result != "pending" {
			t.Fatalf("in-flight duplicate returned %s: %s", result.Result, w.Body)
		}
	}
	mismatch := request(a, f, "POST", apiPrefix+"wake", targetWakeBody(id, "other"))
	if result := decodeWake(t, mismatch); mismatch.Code != 409 || result.TargetID != "other" || result.Result != "failed" || result.Code != "request_target_mismatch" || calls.Load() != 1 {
		t.Fatal("in-flight target binding was bypassed")
	}
	limited := request(a, f, "POST", apiPrefix+"wake", wakeBody(strings.Repeat("b", 32)))
	if limited.Code != 429 || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("new request bypassed target cooldown: %d", limited.Code)
	}
	once.Do(func() { close(release) })
	w := <-done
	if w.Code != 202 || decodeWake(t, w).Result != "sent" {
		t.Fatalf("first result: %d %s", w.Code, w.Body)
	}
	w = request(a, f, "POST", apiPrefix+"wake", wakeBody(id))
	if w.Code != 202 || decodeWake(t, w).Result != "sent" || calls.Load() != 1 {
		t.Fatalf("duplicate was not replay-safe: calls=%d response=%d %s", calls.Load(), w.Code, w.Body)
	}
}

func TestRestartNeverReexecutesDispatchingRecord(t *testing.T) {
	f := agentFixture(t)
	var calls atomic.Int32
	runner := runnerFunc(func(context.Context, string, ...string) error { calls.Add(1); return nil })
	a := openTestAgent(t, f, runner)
	id := strings.Repeat("c", 32)
	// Model a crash after the durable dispatch marker, before a result commit.
	if _, fresh, err := a.ledger.reserve(id, "desktop", time.Now().UTC()); err != nil || !fresh {
		t.Fatalf("reserve: fresh=%t err=%v", fresh, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = openTestAgent(t, f, runner)
	w := request(a, f, "GET", apiPrefix+"wake/"+id, "")
	if w.Code != 200 || decodeWake(t, w).Result != "unknown" {
		t.Fatalf("recovered query: %d %s", w.Code, w.Body)
	}
	w = request(a, f, "POST", apiPrefix+"wake", wakeBody(id))
	if w.Code != 504 || decodeWake(t, w).Result != "unknown" || calls.Load() != 0 {
		t.Fatalf("crash record reexecuted: calls=%d response=%d %s", calls.Load(), w.Code, w.Body)
	}
	w = request(a, f, "GET", apiPrefix+"wake/"+strings.Repeat("d", 32), "")
	if w.Code != 404 || calls.Load() != 0 {
		t.Fatal("lookup of missing ID executed a command")
	}
	w = request(a, f, "POST", apiPrefix+"wake", wakeBody(strings.Repeat("e", 32)))
	if w.Code != 429 {
		t.Fatalf("restart lost persistent cooldown: %d", w.Code)
	}
}

func TestPrivateRoutesCannotBeReachedWithCookiesOrPathTricks(t *testing.T) {
	f := agentFixture(t)
	var commands, upstream atomic.Int32
	a := openTestAgent(t, f, runnerFunc(func(context.Context, string, ...string) error { commands.Add(1); return nil }))
	a.proxy.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		upstream.Add(1)
		return nil, errors.New("unexpected LuCI access")
	})
	cases := []struct {
		path, method  string
		authenticated bool
		status        int
	}{
		{apiPrefix + "wake", "POST", false, 401},
		{apiPrefix + "targets/desktop/status", "GET", false, 401},
		{"/cgi-bin/luci/", "GET", false, 401},
		{"/_roc-agent/unknown", "GET", true, 404},
		{"/_roc-agent", "GET", true, 404},
		{"/_roc-agent%2fv2/targets/desktop/status", "GET", true, 400},
		{"/%2e%2e/_roc-agent/v2/targets/desktop/status", "GET", true, 400},
		{"/foo%5cbar", "GET", true, 400},
		{"http://outside.invalid/", "GET", true, 400},
		{"/", "CONNECT", true, 400},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(wakeBody(strings.Repeat("a", 32))))
			r.Header.Set("Cookie", "__Host-roc_session=ordinary-browser-cookie")
			if tc.authenticated {
				r.TLS = f.state
			}
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
		})
	}
	if commands.Load() != 0 || upstream.Load() != 0 {
		t.Fatalf("protected request escaped: commands=%d upstream=%d", commands.Load(), upstream.Load())
	}
}

func TestMTLSRequiresGatewayIdentityAndTrustedServer(t *testing.T) {
	f := agentFixture(t)
	a := openTestAgent(t, f, runnerFunc(func(context.Context, string, ...string) error { return nil }))
	server := httptest.NewUnstartedServer(a)
	server.TLS = a.tlsConfig.Clone()
	server.StartTLS()
	defer server.Close()
	cases := []struct {
		name       string
		roots      *x509.CertPool
		certs      []tls.Certificate
		serverName string
		success    bool
	}{
		{"gateway", f.roots, []tls.Certificate{f.client}, ServerName, true},
		{"no-client", f.roots, nil, ServerName, false},
		{"wrong-client-identity", f.roots, []tls.Certificate{f.wrongIdentity}, ServerName, false},
		{"wrong-server-name", f.roots, []tls.Certificate{f.client}, "wrong.roc-wol.internal", false},
		{"untrusted-server", x509.NewCertPool(), []tls.Certificate{f.client}, ServerName, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: tc.roots, Certificates: tc.certs, ServerName: tc.serverName}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			resp, err := client.Get(server.URL + apiPrefix + "targets/desktop/status")
			if !tc.success {
				if err == nil {
					resp.Body.Close()
					t.Fatal("invalid mTLS connection was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			var status Status
			if err := json.Unmarshal(body, &status); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 200 || status.Reachability != "reachable" {
				t.Fatalf("authenticated status: %d %s", resp.StatusCode, body)
			}
		})
	}
}

func TestManualStatusRefreshBypassesProbeCache(t *testing.T) {
	f := agentFixture(t)
	calls := 0
	var completed time.Time
	a := openTestAgent(t, f, runnerFunc(func(_ context.Context, path string, args ...string) error {
		if path != f.config.PingPath || strings.Join(args, " ") != "-c 1 -W 1 192.0.2.10" {
			t.Fatal("status probe changed command:", path, args)
		}
		calls++
		completed = time.Now().UTC()
		if calls == 1 {
			return nil
		}
		return errors.New("probe unavailable")
	}))
	readStatus := func(path string) Status {
		t.Helper()
		w := request(a, f, "GET", path, "")
		var status Status
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || w.Code != 200 {
			t.Fatalf("status: %d %s", w.Code, w.Body.String())
		}
		return status
	}
	first := readStatus(apiPrefix + "targets/desktop/status")
	if calls != 1 || first.Reachability != "reachable" || first.CheckedAt.Before(completed) {
		t.Fatalf("initial probe did not report completion: %+v", first)
	}
	cached := readStatus(apiPrefix + "targets/desktop/status")
	if calls != 1 || cached != first {
		t.Fatalf("ordinary status bypassed cache: %+v", cached)
	}
	fresh := readStatus(apiPrefix + "targets/desktop/status?fresh=1")
	if calls != 2 || fresh.Reachability != "unknown" || fresh.CheckedAt.Before(completed) {
		t.Fatalf("manual refresh returned cached result or start time: %+v", fresh)
	}
	cached = readStatus(apiPrefix + "targets/desktop/status")
	if calls != 2 || cached != fresh {
		t.Fatalf("manual refresh did not replace cache: %+v", cached)
	}
}

func TestConcurrentFreshStatusProbesAreSerialized(t *testing.T) {
	f := agentFixture(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	a := openTestAgent(t, f, runnerFunc(func(ctx context.Context, _ string, _ ...string) error {
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	done := make(chan *httptest.ResponseRecorder, 2)
	go func() { done <- request(a, f, "GET", apiPrefix+"targets/desktop/status?fresh=1", "") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first probe did not start")
	}
	go func() { done <- request(a, f, "GET", apiPrefix+"targets/desktop/status?fresh=1", "") }()
	select {
	case <-started:
		t.Fatal("manual status probes overlapped")
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	for range 2 {
		select {
		case w := <-done:
			if w.Code != 200 {
				t.Fatal("status request failed:", w.Code)
			}
		case <-time.After(time.Second):
			t.Fatal("serialized status request did not complete")
		}
	}
	select {
	case <-started:
	default:
		t.Fatal("second manual refresh reused first probe")
	}
}

func TestCanceledQueuedStatusRequestDoesNotProbe(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := agentFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var calls atomic.Int32
			a := openTestAgent(t, f, runnerFunc(func(ctx context.Context, _ string, _ ...string) error {
				if calls.Add(1) == 1 {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}))
			firstDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { firstDone <- request(a, f, "GET", apiPrefix+"targets/desktop/status?fresh=1", "") }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("first probe did not start")
			}
			timeout := time.Hour
			if mode == "deadline" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			r := httptest.NewRequest("GET", apiPrefix+"targets/desktop/status?fresh=1", nil).WithContext(ctx)
			r.TLS = f.state
			queuedDone := make(chan struct{})
			entered := make(chan struct{})
			go func() {
				close(entered)
				a.ServeHTTP(httptest.NewRecorder(), r)
				close(queuedDone)
			}()
			<-entered
			if mode == "cancel" {
				cancel()
			}
			<-ctx.Done()
			select {
			case <-queuedDone:
			case <-time.After(time.Second):
				t.Error("abandoned request waited for the active probe")
			}
			once.Do(func() { close(release) })
			select {
			case w := <-firstDone:
				if w.Code != 200 {
					t.Fatalf("live probe failed: %d %s", w.Code, w.Body.String())
				}
			case <-time.After(time.Second):
				t.Fatal("live probe did not complete")
			}
			select {
			case <-queuedDone:
			case <-time.After(time.Second):
				t.Fatal("abandoned request did not return")
			}
			if calls.Load() != 1 {
				t.Fatalf("abandoned request ran a delayed probe: %d calls", calls.Load())
			}
		})
	}
}

func TestCanceledActiveStatusRequestPreservesCache(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := agentFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var calls atomic.Int32
			a := openTestAgent(t, f, runnerFunc(func(ctx context.Context, _ string, _ ...string) error {
				if calls.Add(1) == 2 {
					close(started)
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-release:
						return errors.New("probe interrupted")
					}
				}
				return nil
			}))
			first := request(a, f, "GET", apiPrefix+"targets/desktop/status", "")
			if first.Code != 200 || !strings.Contains(first.Body.String(), `"reachability":"reachable"`) {
				t.Fatalf("initial status failed: %d %s", first.Code, first.Body.String())
			}
			timeout := time.Hour
			if mode == "deadline" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			r := httptest.NewRequest("GET", apiPrefix+"targets/desktop/status?fresh=1", nil).WithContext(ctx)
			r.TLS = f.state
			done := make(chan struct{})
			go func() {
				a.ServeHTTP(httptest.NewRecorder(), r)
				close(done)
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("manual probe did not start")
			}
			if mode == "cancel" {
				cancel()
			}
			<-ctx.Done()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("active probe ignored caller cancellation")
			}
			once.Do(func() { close(release) })
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("interrupted probe did not return")
			}
			cached := request(a, f, "GET", apiPrefix+"targets/desktop/status", "")
			if cached.Code != 200 || cached.Body.String() != first.Body.String() || calls.Load() != 2 {
				t.Fatalf("canceled probe replaced last good cache: calls=%d status=%d body=%s", calls.Load(), cached.Code, cached.Body.String())
			}
			fresh := request(a, f, "GET", apiPrefix+"targets/desktop/status?fresh=1", "")
			if fresh.Code != 200 || calls.Load() != 3 || !strings.Contains(fresh.Body.String(), `"reachability":"reachable"`) {
				t.Fatalf("live manual refresh did not run a new probe: calls=%d status=%d body=%s", calls.Load(), fresh.Code, fresh.Body.String())
			}
		})
	}
}

func TestStatusShutdownCancelsActiveAndQueuedProbes(t *testing.T) {
	f := agentFixture(t)
	started := make(chan struct{})
	var calls atomic.Int32
	a := openTestAgent(t, f, runnerFunc(func(ctx context.Context, _ string, _ ...string) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		return ctx.Err()
	}))
	done := make(chan struct{}, 2)
	go func() {
		request(a, f, "GET", apiPrefix+"targets/desktop/status?fresh=1", "")
		done <- struct{}{}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active probe did not start")
	}
	go func() {
		request(a, f, "GET", apiPrefix+"targets/desktop/status?fresh=1", "")
		done <- struct{}{}
	}()
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel status requests")
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("status request survived shutdown")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("shutdown dispatched a queued probe: %d calls", calls.Load())
	}
}

func TestLedgerCapacityDoesNotEvictLiveDeduplication(t *testing.T) {
	l, err := openLedger(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.db.Close()
	now := time.Now().UTC()
	err = l.db.Update(func(tx *bolt.Tx) error {
		for i := range maxRecords {
			id := fmt.Sprintf("%032x", i)
			rec := wakeRecord{WakeResult: WakeResult{RequestID: id, TargetID: "desktop", Result: "sent"}, CreatedAt: now.Add(-time.Minute)}
			data, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := tx.Bucket(wakeBucket).Put([]byte(id), data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	newID := strings.Repeat("f", 32)
	if _, fresh, err := l.reserve(newID, "desktop", now); !errors.Is(err, errCapacity) || fresh {
		t.Fatalf("full ledger accepted fresh command: fresh=%t err=%v", fresh, err)
	}
	oldID := fmt.Sprintf("%032x", 0)
	if result, fresh, err := l.reserve(oldID, "desktop", now); err != nil || fresh || result.Result != "sent" {
		t.Fatalf("capacity displaced duplicate: fresh=%t result=%+v err=%v", fresh, result, err)
	}
	if _, fresh, err := l.reserve(newID, "desktop", now.Add(retention)); err != nil || !fresh {
		t.Fatalf("expired history did not free capacity: fresh=%t err=%v", fresh, err)
	}
}

func TestInterruptedDispatchIsUnknownAndNeverRetried(t *testing.T) {
	f := agentFixture(t)
	started := make(chan struct{})
	var calls atomic.Int32
	a := openTestAgent(t, f, runnerFunc(func(ctx context.Context, _ string, _ ...string) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		return ctx.Err()
	}))
	id := strings.Repeat("9", 32)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(a, f, "POST", apiPrefix+"wake", wakeBody(id)) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("command did not start")
	}
	a.cancel()
	w := <-done
	if result := decodeWake(t, w); w.Code != 504 || result.Result != "unknown" || result.Code != "outcome_unknown" || result.SentAt != nil {
		t.Fatalf("interrupted command was given a definitive outcome: %d %s", w.Code, w.Body)
	}
	w = request(a, f, "POST", apiPrefix+"wake", wakeBody(id))
	if w.Code != 504 || decodeWake(t, w).Result != "unknown" || calls.Load() != 1 {
		t.Fatalf("interrupted command was retried: calls=%d response=%d %s", calls.Load(), w.Code, w.Body)
	}
}

func TestMalformedWakeRequestsCannotDispatch(t *testing.T) {
	f := agentFixture(t)
	var calls atomic.Int32
	a := openTestAgent(t, f, runnerFunc(func(context.Context, string, ...string) error { calls.Add(1); return nil }))
	id := strings.Repeat("8", 32)
	valid := wakeBody(id)
	cases := map[string]string{
		"unknown-command":    `{"requestId":"` + id + `","targetId":"desktop","command":"reboot"}`,
		"duplicate-id":       `{"requestId":"` + id + `","requestId":"` + strings.Repeat("7", 32) + `","targetId":"desktop"}`,
		"multiple-values":    valid + valid,
		"invalid-utf8":       `{"requestId":"` + id + `","targetId":"desktop` + string([]byte{0xff}) + `"}`,
		"wrong-target":       `{"requestId":"` + id + `","targetId":"Other!"}`,
		"incorrect-key-case": `{"RequestId":"` + id + `","targetId":"desktop"}`,
		"oversize":           valid + strings.Repeat(" ", jsonLimit),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := request(a, f, "POST", apiPrefix+"wake", body)
			if w.Code != 400 {
				t.Fatalf("invalid command accepted: %d %s", w.Code, w.Body)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("malformed requests dispatched %d commands", calls.Load())
	}
}

func twoTargetFixture(t *testing.T) fixture {
	f := agentFixture(t)
	f.config.Targets = []Target{
		{ID: "pc-a", Name: "Computer A", MAC: "02:00:00:00:00:0a", Interface: "lan-a", Broadcast: true, IP: "192.0.2.10"},
		{ID: "pc-b", Name: "Computer B", MAC: "02:00:00:00:00:0b", Interface: "lan-b", IP: "198.51.100.20"},
	}
	f.config.EtherwakePath = filepath.Join(filepath.Dir(f.config.DataFile), "etherwake")
	return f
}

func targetWakeBody(id, target string) string {
	return `{"requestId":"` + id + `","targetId":"` + target + `"}`
}

func TestMultiTargetExecutionBindingAndGlobalCooldown(t *testing.T) {
	f := twoTargetFixture(t)
	var commands []string
	a := openTestAgent(t, f, runnerFunc(func(_ context.Context, path string, args ...string) error {
		commands = append(commands, path+" "+strings.Join(args, " "))
		return nil
	}))
	id := strings.Repeat("a", 32)
	w := request(a, f, "POST", apiPrefix+"wake", targetWakeBody(id, "pc-a"))
	if result := decodeWake(t, w); w.Code != 202 || result.TargetID != "pc-a" || result.Result != "sent" {
		t.Fatalf("first target: %d %+v", w.Code, result)
	}
	w = request(a, f, "POST", apiPrefix+"wake", targetWakeBody(id, "pc-b"))
	if result := decodeWake(t, w); w.Code != 409 || result.TargetID != "pc-b" || result.RequestID != id || result.Result != "failed" || result.Code != "request_target_mismatch" {
		t.Fatalf("target replay was not refused: %d %+v", w.Code, result)
	}
	w = request(a, f, "POST", apiPrefix+"wake", targetWakeBody(id, "pc-a"))
	if w.Code != 202 || len(commands) != 1 {
		t.Fatal("same-target duplicate dispatched again")
	}
	secondID := strings.Repeat("b", 32)
	w = request(a, f, "POST", apiPrefix+"wake", targetWakeBody(secondID, "pc-b"))
	if w.Code != 429 || len(commands) != 1 {
		t.Fatal("target rotation bypassed global cooldown")
	}
	if err := a.ledger.db.Update(func(tx *bolt.Tx) error {
		stamp, err := time.Now().Add(-cooldown - time.Second).UTC().MarshalText()
		if err != nil {
			return err
		}
		return tx.Bucket(metaBucket).Put(lastDispatchKey, stamp)
	}); err != nil {
		t.Fatal(err)
	}
	w = request(a, f, "POST", apiPrefix+"wake", targetWakeBody(secondID, "pc-b"))
	if w.Code != 202 || decodeWake(t, w).TargetID != "pc-b" || len(commands) != 2 {
		t.Fatalf("second target was not dispatched: %d %s", w.Code, w.Body)
	}
	want := []string{
		f.config.EtherwakePath + " -i lan-a -b 02:00:00:00:00:0a",
		f.config.EtherwakePath + " -i lan-b 02:00:00:00:00:0b",
	}
	for i := range want {
		if commands[i] != want[i] {
			t.Fatalf("command %d: %q want %q", i, commands[i], want[i])
		}
	}
}

func TestDeletedTargetHistoryAndControlledMissingTarget(t *testing.T) {
	f := twoTargetFixture(t)
	var calls atomic.Int32
	runner := runnerFunc(func(context.Context, string, ...string) error { calls.Add(1); return nil })
	a := openTestAgent(t, f, runner)
	id := strings.Repeat("c", 32)
	w := request(a, f, "POST", apiPrefix+"wake", targetWakeBody(id, "pc-a"))
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	f.config.Targets = f.config.Targets[1:]
	a = openTestAgent(t, f, runner)
	w = request(a, f, "GET", apiPrefix+"wake/"+id, "")
	if w.Code != 200 || decodeWake(t, w).TargetID != "pc-a" {
		t.Fatal("deleted target history lost")
	}
	w = request(a, f, "POST", apiPrefix+"wake", targetWakeBody(id, "pc-a"))
	if w.Code != 202 || calls.Load() != 1 {
		t.Fatal("deleted target duplicate was not preserved")
	}
	w = request(a, f, "POST", apiPrefix+"wake", targetWakeBody(id, "removed"))
	if result := decodeWake(t, w); w.Code != 409 || result.TargetID != "removed" || result.Code != "request_target_mismatch" {
		t.Fatal("binding did not precede whitelist")
	}
	missingID := strings.Repeat("d", 32)
	w = request(a, f, "POST", apiPrefix+"wake", targetWakeBody(missingID, "pc-a"))
	result := decodeWake(t, w)
	if w.Code != 404 || result.TargetID != "pc-a" || result.RequestID != missingID || result.Result != "failed" || result.Code != "target_not_found" || calls.Load() != 1 {
		t.Fatalf("missing target rejection: %d %+v", w.Code, result)
	}
	if _, err := a.ledger.lookup(missingID, time.Now()); !errors.Is(err, errNotFound) {
		t.Fatal("missing target reserved a dispatch")
	}
}

func TestTargetListAndPerTargetProbeCaches(t *testing.T) {
	f := twoTargetFixture(t)
	var ips []string
	a := openTestAgent(t, f, runnerFunc(func(_ context.Context, path string, args ...string) error {
		if path != f.config.PingPath {
			t.Fatal("list or status invoked wake")
		}
		ips = append(ips, args[len(args)-1])
		return nil
	}))
	w := request(a, f, "GET", apiPrefix+"targets", "")
	var list protocol.TargetList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !list.ManagementEnabled || len(list.Targets) != 2 || len(ips) != 0 || !protocol.ValidTargetList(list) {
		t.Fatal("invalid target list or list probed")
	}
	if len(a.targets) != 2 {
		t.Fatal("target index is not bounded to config")
	}
	for _, id := range []string{"pc-a", "pc-b", "pc-a", "pc-b"} {
		w = request(a, f, "GET", apiPrefix+"targets/"+id+"/status", "")
		var status Status
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || status.TargetID != id {
			t.Fatal("wrong target status")
		}
	}
	if len(ips) != 2 || ips[0] != "192.0.2.10" || ips[1] != "198.51.100.20" {
		t.Fatalf("target probes/cache: %v", ips)
	}
	for path, code := range map[string]int{
		apiPrefix + "targets/missing/status":      404,
		apiPrefix + "targets/Bad/status":          400,
		apiPrefix + "targets?fresh=1":             400,
		apiPrefix + "targets/pc-a/status?fresh=0": 400,
	} {
		w = request(a, f, "GET", path, "")
		if w.Code != code {
			t.Fatalf("route %q returned %d", path, w.Code)
		}
	}
}

func TestDifferentTargetProbesDoNotShareGate(t *testing.T) {
	f := twoTargetFixture(t)
	started := make(chan string, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	a := openTestAgent(t, f, runnerFunc(func(ctx context.Context, _ string, args ...string) error {
		started <- args[len(args)-1]
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	done := make(chan *httptest.ResponseRecorder, 2)
	for _, id := range []string{"pc-a", "pc-b"} {
		go func(id string) { done <- request(a, f, "GET", apiPrefix+"targets/"+id+"/status?fresh=1", "") }(id)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("different target blocked on shared gate")
		}
	}
	once.Do(func() { close(release) })
	for range 2 {
		if w := <-done; w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
}

func TestManagementDisabledAndV1Refusal(t *testing.T) {
	f := agentFixture(t)
	f.config.LuCIOrigin = ""
	var calls atomic.Int32
	a := openTestAgent(t, f, runnerFunc(func(context.Context, string, ...string) error { calls.Add(1); return nil }))
	if a.proxy != nil || a.transport != nil {
		t.Fatal("disabled management built a proxy")
	}
	for _, path := range []string{"/", "/cgi-bin/luci/"} {
		w := request(a, f, "GET", path, "")
		if w.Code != 403 || !strings.Contains(w.Body.String(), `"code":"management_disabled"`) {
			t.Fatal("disabled proxy accepted request")
		}
	}
	w := request(a, f, "GET", apiPrefix+"targets", "")
	var list protocol.TargetList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || list.ManagementEnabled || w.Code != 200 {
		t.Fatal("disabled capability missing")
	}
	for _, path := range []string{"/_roc-agent/v1/status", "/_roc-agent/v1/wake", "/_roc-agent/v1"} {
		w = request(a, f, "POST", path+"?fresh=1", wakeBody(strings.Repeat("a", 32)))
		if w.Code != 426 || !strings.Contains(w.Body.String(), `"code":"protocol_upgrade_required"`) {
			t.Fatal("v1 route was not refused")
		}
	}
	w = request(a, f, "POST", apiPrefix+"wake", wakeBody(strings.Repeat("a", 32)))
	if w.Code != 202 || calls.Load() != 1 {
		t.Fatal("management shutdown disabled wake")
	}
}

func TestLuCIRedirectsUseConfiguredOrigin(t *testing.T) {
	f := agentFixture(t)
	f.config.LuCIOrigin = "http://127.0.0.1:8088"
	a := openTestAgent(t, f, runnerFunc(func(context.Context, string, ...string) error { return nil }))
	cases := []struct {
		input, want string
		reject      bool
	}{
		{"http://127.0.0.1:8088/cgi-bin/luci/?x=1#section", "/cgi-bin/luci/?x=1#section", false},
		{"/cgi-bin/luci/?x=1#section", "/cgi-bin/luci/?x=1#section", false},
		{"login?next=%2B", "login?next=%2B", false},
		{"http://127.0.0.1/cgi-bin/luci/", "", true},
		{"http://127.0.0.1:80/cgi-bin/luci/", "", true},
		{"http://127.0.0.2:8088/", "", true},
		{"https://127.0.0.1:8088/", "", true},
		{"http://user@127.0.0.1:8088/", "", true},
		{"//outside.invalid/path", "", true},
		{"http://127.0.0.1:8088//outside.invalid/path", "", true},
		{"/safe\r\nLocation: https://outside.invalid", "", true},
		{"/%1funsafe", "", true},
		{"/%2foutside.invalid", "", true},
		{"/\\outside.invalid", "", true},
		{"javascript:alert(1)", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			resp := &http.Response{StatusCode: 302, Header: make(http.Header)}
			resp.Header.Set("Location", tc.input)
			err := a.proxy.ModifyResponse(resp)
			if (err != nil) != tc.reject {
				t.Fatalf("redirect error %v, rejected=%t", err, tc.reject)
			}
			if !tc.reject && resp.Header.Get("Location") != tc.want {
				t.Fatalf("redirect %q", resp.Header.Get("Location"))
			}
		})
	}
	resp := &http.Response{StatusCode: 302, Header: http.Header{"Location": {"/safe", "//outside.invalid"}}}
	if a.proxy.ModifyResponse(resp) == nil {
		t.Fatal("ambiguous redirect accepted")
	}
	defaultOrigin, err := luciOrigin("http://127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if location, err := safeLuCILocation("http://127.0.0.1:80/path", defaultOrigin); err != nil || location != "/path" {
		t.Fatal("default port normalization failed")
	}
}

func TestConfigValidatesMultiTargetAndLoopbackManagement(t *testing.T) {
	f := twoTargetFixture(t)
	for _, origin := range []string{"", "http://127.0.0.1", "http://127.0.0.1:8088", "http://[::1]:8088"} {
		c := f.config
		c.LuCIOrigin = origin
		if err := c.Validate(); err != nil {
			t.Fatalf("valid loopback origin rejected: %v", err)
		}
	}
	for _, origin := range []string{"http://localhost:80", "http://192.0.2.1:80", "https://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://127.0.0.1/", "http://user@127.0.0.1", "http://127.0.0.1?", "http://127.0.0.1#"} {
		c := f.config
		c.LuCIOrigin = origin
		if c.Validate() == nil {
			t.Fatalf("unsafe loopback origin accepted: %q", origin)
		}
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty", func(c *Config) { c.Targets = nil }},
		{"too-many", func(c *Config) { c.Targets = make([]Target, 65) }},
		{"duplicate", func(c *Config) { c.Targets[1].ID = c.Targets[0].ID }},
		{"id", func(c *Config) { c.Targets[0].ID = "-bad" }},
		{"name", func(c *Config) { c.Targets[0].Name = "bad\nname" }},
		{"long-name", func(c *Config) { c.Targets[0].Name = strings.Repeat("界", 65) }},
		{"multicast-mac", func(c *Config) { c.Targets[0].MAC = "01:00:00:00:00:01" }},
		{"zero-mac", func(c *Config) { c.Targets[0].MAC = "00:00:00:00:00:00" }},
		{"eui64", func(c *Config) { c.Targets[0].MAC = "02:00:00:00:00:00:00:01" }},
		{"ipv6", func(c *Config) { c.Targets[0].IP = "2001:db8::1" }},
		{"unspecified", func(c *Config) { c.Targets[0].IP = "0.0.0.0" }},
		{"loopback", func(c *Config) { c.Targets[0].IP = "127.0.0.2" }},
		{"multicast", func(c *Config) { c.Targets[0].IP = "224.0.0.1" }},
		{"broadcast", func(c *Config) { c.Targets[0].IP = "255.255.255.255" }},
		{"interface", func(c *Config) { c.Targets[0].Interface = "lan;reboot" }},
		{"long-interface", func(c *Config) { c.Targets[0].Interface = strings.Repeat("x", 16) }},
		{"relative-executable", func(c *Config) { c.EtherwakePath = "etherwake" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := f.config
			c.Targets = append([]Target(nil), c.Targets...)
			tc.mutate(&c)
			if c.Validate() == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	c := f.config
	c.EtherwakePath = ""
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded.EtherwakePath != "/usr/bin/etherwake" {
		t.Fatalf("default executable: %q %v", loaded.EtherwakePath, err)
	}
}

func TestMaximumTargetListFitsResponseLimit(t *testing.T) {
	f := agentFixture(t)
	f.config.Targets = make([]Target, 64)
	for i := range f.config.Targets {
		f.config.Targets[i] = Target{ID: fmt.Sprintf("pc-%060d", i), Name: strings.Repeat("界", 64), MAC: "02:00:00:00:00:01", IP: "192.0.2.1", Interface: "test-lan"}
	}
	a := openTestAgent(t, f, runnerFunc(func(context.Context, string, ...string) error { t.Fatal("list executed a command"); return nil }))
	w := request(a, f, "GET", apiPrefix+"targets", "")
	if w.Code != 200 || w.Body.Len() > protocol.ResponseJSONLimit || len(a.targets) != 64 {
		t.Fatal("maximum target list exceeded contract")
	}
	var list protocol.TargetList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || !protocol.ValidTargetList(list) {
		t.Fatal("maximum list invalid")
	}
}

func writeLegacyLedger(t *testing.T, path string, corrupt bool) (string, string, time.Time) {
	t.Helper()
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	sentID, dispatchID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	now := time.Now().UTC()
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket(wakeBucket)
		if err != nil {
			return err
		}
		m, err := tx.CreateBucket(metaBucket)
		if err != nil {
			return err
		}
		for id, result := range map[string]string{sentID: "sent", dispatchID: "dispatching", strings.Repeat("3", 32): "pending", strings.Repeat("6", 32): "dispatching"} {
			rec := struct {
				RequestID string    `json:"requestId"`
				Result    string    `json:"result"`
				CreatedAt time.Time `json:"createdAt"`
			}{id, result, now}
			if id == strings.Repeat("6", 32) {
				rec.CreatedAt = now.Add(-retention - time.Hour)
			}
			data, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(id), data); err != nil {
				return err
			}
		}
		if corrupt {
			if err := b.Put([]byte(strings.Repeat("4", 32)), []byte(`{"privateSecret":"do-not-leak"}`)); err != nil {
				return err
			}
		}
		stamp, err := now.MarshalText()
		if err != nil {
			return err
		}
		return m.Put(lastDispatchKey, stamp)
	})
	closeErr := db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return sentID, dispatchID, now
}

func TestLedgerMigratesWithConsistentBackupAndReplayBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	sentID, dispatchID, now := writeLegacyLedger(t, path, false)
	l, err := openLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	// Migration preserves every old ID, even one already beyond retention;
	// ordinary startup/reservation cleanup may expire it only after cutover.
	if err := l.db.View(func(tx *bolt.Tx) error {
		var rec wakeRecord
		if err := json.Unmarshal(tx.Bucket(wakeBucket).Get([]byte(strings.Repeat("6", 32))), &rec); err != nil {
			return err
		}
		if rec.TargetID != "desktop" || rec.Result != "unknown" {
			return errors.New("migration discarded expired legacy ID")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		for id, want := range map[string]string{sentID: "sent", dispatchID: "unknown", strings.Repeat("3", 32): "pending"} {
			result, err := l.lookup(id, now)
			if err != nil || result.TargetID != "desktop" || result.Result != want {
				t.Fatalf("migration lookup %+v %v", result, err)
			}
		}
		if _, fresh, err := l.reserve(sentID, "pc-a", now); !errors.Is(err, errTargetMismatch) || fresh {
			t.Fatal("migration lost request binding")
		}
		if _, fresh, err := l.reserve(strings.Repeat("5", 32), "pc-a", now); err == nil || fresh {
			t.Fatal("migration lost cooldown")
		}
		if err := l.db.View(func(tx *bolt.Tx) error {
			if string(tx.Bucket(metaBucket).Get(schemaVersionKey)) != "2" {
				return errors.New("schema not migrated")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check()
	backups, err := filepath.Glob(path + ".schema-v1-*.bak")
	if err != nil || len(backups) != 1 {
		t.Fatalf("consistent backup missing: %v %v", backups, err)
	}
	backupPath := backups[0]
	backup, err := bolt.Open(backupPath, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	err = backup.View(func(tx *bolt.Tx) error {
		if tx.Bucket(metaBucket).Get(schemaVersionKey) != nil {
			return errors.New("backup was created after migration")
		}
		var rec wakeRecord
		if err := json.Unmarshal(tx.Bucket(wakeBucket).Get([]byte(dispatchID)), &rec); err != nil {
			return err
		}
		if rec.TargetID != "" || rec.Result != "dispatching" {
			return errors.New("backup no longer contains original record")
		}
		if tx.Bucket(wakeBucket).Get([]byte(strings.Repeat("6", 32))) == nil {
			return errors.New("backup discarded expired legacy ID")
		}
		return nil
	})
	if closeErr := backup.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	backupBefore, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.db.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = openLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.db.Close()
	check()
	if err := l.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(wakeBucket).Get([]byte(strings.Repeat("6", 32))) != nil {
			return errors.New("normal restart did not expire old history")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	backupAfter, err := os.ReadFile(backupPath)
	if err != nil || string(backupBefore) != string(backupAfter) {
		t.Fatal("restart changed migration backup")
	}
}

func TestLedgerCorruptionFailsStartupWithoutPrivateData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	writeLegacyLedger(t, path, true)
	l, err := openLedger(path)
	if err == nil {
		l.db.Close()
		t.Fatal("corrupt ledger was accepted")
	}
	if err.Error() != errLedgerCorrupt.Error() || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatal("corrupt ledger error leaked record")
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(metaBucket).Get(schemaVersionKey) != nil {
			return errors.New("failed migration committed schema")
		}
		if tx.Bucket(wakeBucket).Get([]byte(strings.Repeat("4", 32))) == nil {
			return errors.New("failed migration erased corruption")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaV2MissingTargetIsCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	writeLegacyLedger(t, path, false)
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket(metaBucket).Put(schemaVersionKey, []byte("2")) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := openLedger(path)
	if err == nil {
		l.db.Close()
		t.Fatal("v2 missing target silently defaulted")
	}
	if !errors.Is(err, errLedgerCorrupt) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProductionStartupRejectsUnavailableExecutables(t *testing.T) {
	f := agentFixture(t)
	f.config.EtherwakePath = ""
	a, err := New(f.config, nil)
	if err == nil {
		a.Close()
		t.Fatal("production startup accepted missing executable")
	}
	if err.Error() != "configured executable is unavailable or not executable" {
		t.Fatalf("startup error was not sanitized: %v", err)
	}
	// The same isolated config intentionally has no real interface or executable.
	// An injected runner must remain usable without touching a real LAN.
	a = openTestAgent(t, f, runnerFunc(func(context.Context, string, ...string) error { return nil }))
	if a.config.EtherwakePath != "/usr/bin/etherwake" {
		t.Fatal("New did not normalize executable default")
	}
}

func TestLedgerFinishCannotChangeTargetBinding(t *testing.T) {
	l, err := openLedger(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.db.Close()
	id := strings.Repeat("a", 32)
	now := time.Now().UTC()
	result, fresh, err := l.reserve(id, "pc-a", now)
	if err != nil || !fresh {
		t.Fatalf("reserve: %v", err)
	}
	result.TargetID, result.Result = "pc-b", "sent"
	if err := l.finish(result); !errors.Is(err, errTargetMismatch) {
		t.Fatal("finish changed persisted target")
	}
	stored, err := l.lookup(id, now)
	if err != nil || stored.TargetID != "pc-a" {
		t.Fatal("failed finish corrupted request")
	}
}

func TestProductionStartupChecksInterfacesAndExecutePermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable permission validation")
	}
	f := agentFixture(t)
	for _, name := range []string{"ping", "etherwake"} {
		path := filepath.Join(filepath.Dir(f.config.DataFile), name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if name == "ping" {
			f.config.PingPath = path
		} else {
			f.config.EtherwakePath = path
		}
	}
	missing := "wol-missing"
	if _, err := net.InterfaceByName(missing); err == nil {
		t.Skip("reserved test interface exists")
	}
	f.config.Targets[0].Interface = missing
	a, err := New(f.config, nil)
	if err == nil {
		a.Close()
		t.Fatal("missing interface accepted at startup")
	}
	if err.Error() != "configured target interface is unavailable" {
		t.Fatalf("interface error not sanitized: %v", err)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iface := range ifaces {
		f.config.Targets[0].Interface = iface.Name
		if f.config.Validate() == nil {
			found = true
			break
		}
	}
	if !found {
		t.Skip("no supported local interface")
	}
	a, err = New(f.config, nil)
	if err != nil {
		t.Fatalf("valid runtime dependencies rejected: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.config.EtherwakePath, 0600); err != nil {
		t.Fatal(err)
	}
	a, err = New(f.config, nil)
	if err == nil {
		a.Close()
		t.Fatal("non-executable etherwake accepted at startup")
	}
	if err.Error() != "configured executable is unavailable or not executable" {
		t.Fatalf("permission error not sanitized: %v", err)
	}
}

func TestConfigAcceptsLinuxExecutablePathsOnBuildHost(t *testing.T) {
	f := agentFixture(t)
	c := f.config
	c.PingPath = "/bin/ping"
	c.EtherwakePath = "/usr/bin/etherwake"
	if err := c.Validate(); err != nil {
		t.Fatalf("Linux deployment paths rejected by build host: %v", err)
	}
	for _, invalid := range []string{"etherwake", "usr/bin/etherwake", "/usr/bin/etherwake\n"} {
		c.EtherwakePath = invalid
		if c.Validate() == nil {
			t.Fatalf("invalid executable path accepted: %q", invalid)
		}
	}
}

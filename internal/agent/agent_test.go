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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
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
		config: Config{Listen: "127.0.0.1:19991", ServerCertFile: serverCert, ServerKeyFile: serverKey, ClientCAFile: caPath, ClientURI: GatewayURI, DataFile: filepath.Join(dir, "agent.db"), LuCIOrigin: "http://127.0.0.1:80", PingPath: filepath.Join(dir, "ping"), Targets: []Target{{ID: "desktop", Name: "DESKTOP-IENBTJG", MAC: "B0:25:AA:83:C6:EF", Interface: "br-lan", Broadcast: true, IP: "192.168.2.100"}}},
		roots:  roots, client: client, wrongIdentity: wrong,
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
		if path != "/usr/bin/etherwake" || strings.Join(args, " ") != "-i br-lan -b B0:25:AA:83:C6:EF" {
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
	if _, fresh, err := a.ledger.reserve(id, time.Now().UTC()); err != nil || !fresh {
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
		{apiPrefix + "status", "GET", false, 401},
		{"/cgi-bin/luci/", "GET", false, 401},
		{"/_roc-agent/unknown", "GET", true, 404},
		{"/_roc-agent", "GET", true, 404},
		{"/_roc-agent%2fv1/status", "GET", true, 400},
		{"/%2e%2e/_roc-agent/v1/status", "GET", true, 400},
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
			resp, err := client.Get(server.URL + apiPrefix + "status")
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
		if path != f.config.PingPath || strings.Join(args, " ") != "-c 1 -W 1 192.168.2.100" {
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
	first := readStatus(apiPrefix + "status")
	if calls != 1 || first.Reachability != "reachable" || first.CheckedAt.Before(completed) {
		t.Fatalf("initial probe did not report completion: %+v", first)
	}
	cached := readStatus(apiPrefix + "status")
	if calls != 1 || cached != first {
		t.Fatalf("ordinary status bypassed cache: %+v", cached)
	}
	fresh := readStatus(apiPrefix + "status?fresh=1")
	if calls != 2 || fresh.Reachability != "unknown" || fresh.CheckedAt.Before(completed) {
		t.Fatalf("manual refresh returned cached result or start time: %+v", fresh)
	}
	cached = readStatus(apiPrefix + "status")
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
	go func() { done <- request(a, f, "GET", apiPrefix+"status?fresh=1", "") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first probe did not start")
	}
	go func() { done <- request(a, f, "GET", apiPrefix+"status?fresh=1", "") }()
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
			go func() { firstDone <- request(a, f, "GET", apiPrefix+"status?fresh=1", "") }()
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
			r := httptest.NewRequest("GET", apiPrefix+"status?fresh=1", nil).WithContext(ctx)
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
			first := request(a, f, "GET", apiPrefix+"status", "")
			if first.Code != 200 || !strings.Contains(first.Body.String(), `"reachability":"reachable"`) {
				t.Fatalf("initial status failed: %d %s", first.Code, first.Body.String())
			}
			timeout := time.Hour
			if mode == "deadline" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			r := httptest.NewRequest("GET", apiPrefix+"status?fresh=1", nil).WithContext(ctx)
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
			cached := request(a, f, "GET", apiPrefix+"status", "")
			if cached.Code != 200 || cached.Body.String() != first.Body.String() || calls.Load() != 2 {
				t.Fatalf("canceled probe replaced last good cache: calls=%d status=%d body=%s", calls.Load(), cached.Code, cached.Body.String())
			}
			fresh := request(a, f, "GET", apiPrefix+"status?fresh=1", "")
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
		request(a, f, "GET", apiPrefix+"status?fresh=1", "")
		done <- struct{}{}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active probe did not start")
	}
	go func() {
		request(a, f, "GET", apiPrefix+"status?fresh=1", "")
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
			rec := wakeRecord{WakeResult: WakeResult{RequestID: id, Result: "sent"}, CreatedAt: now.Add(-time.Minute)}
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
	if _, fresh, err := l.reserve(newID, now); !errors.Is(err, errCapacity) || fresh {
		t.Fatalf("full ledger accepted fresh command: fresh=%t err=%v", fresh, err)
	}
	oldID := fmt.Sprintf("%032x", 0)
	if result, fresh, err := l.reserve(oldID, now); err != nil || fresh || result.Result != "sent" {
		t.Fatalf("capacity displaced duplicate: fresh=%t result=%+v err=%v", fresh, result, err)
	}
	if _, fresh, err := l.reserve(newID, now.Add(retention)); err != nil || !fresh {
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
		"wrong-target":       `{"requestId":"` + id + `","targetId":"other"}`,
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

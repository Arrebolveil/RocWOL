package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiPrefix = "/_roc-agent/v1/"
const jsonLimit = 16 * 1024
const uploadLimit = 128 * 1024 * 1024

// Runner executes a fixed executable directly, without a shell. Implementations
// must honor cancellation. Its injectable boundary permits safe isolated tests.
type Runner interface {
	Run(context.Context, string, ...string) error
}
type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, path string, args ...string) error {
	return exec.CommandContext(ctx, path, args...).Run()
}

type Status struct {
	TargetID     string    `json:"targetId"`
	Reachability string    `json:"reachability"`
	CheckedAt    time.Time `json:"checkedAt"`
}

type Agent struct {
	config    Config
	ledger    *ledger
	runner    Runner
	tlsConfig *tls.Config
	proxy     *httputil.ReverseProxy
	transport *http.Transport
	ctx       context.Context
	cancel    context.CancelFunc
	lifeMu    sync.Mutex
	closed    bool
	requests  sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
	statusMu  sync.Mutex
	status    Status
}

func New(c Config, runner Runner) (*Agent, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	tlsConfig, err := loadTLS(c)
	if err != nil {
		return nil, err
	}
	l, err := openLedger(c.DataFile)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		runner = commandRunner{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{config: c, ledger: l, runner: runner, tlsConfig: tlsConfig, ctx: ctx, cancel: cancel}
	origin, _ := url.Parse(c.LuCIOrigin)
	a.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   16,
	}
	a.proxy = &httputil.ReverseProxy{
		Transport: a.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = origin.Scheme, origin.Host
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = pr.In.Host
			// Never trust client-supplied forwarding identities.
			for name := range pr.Out.Header {
				lower := strings.ToLower(name)
				if lower == "forwarded" || strings.HasPrefix(lower, "x-forwarded-") || lower == "cf-connecting-ip" {
					pr.Out.Header.Del(name)
				}
			}
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("Cache-Control", "no-store")
			if resp.StatusCode == http.StatusSwitchingProtocols {
				body, ok := resp.Body.(io.ReadWriteCloser)
				if !ok {
					return errors.New("upstream upgrade has no bidirectional body")
				}
				stop := context.AfterFunc(resp.Request.Context(), func() { _ = body.Close() })
				resp.Body = &upgradeBody{ReadWriteCloser: body, stop: stop}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusServiceUnavailable, "upstream_unavailable", "Router management is unavailable.", "")
		},
	}
	return a, nil
}

type upgradeBody struct {
	io.ReadWriteCloser
	stop func() bool
}

func (b *upgradeBody) Close() error { b.stop(); return b.ReadWriteCloser.Close() }

func loadTLS(c Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.ServerCertFile, c.ServerKeyFile)
	if err != nil {
		return nil, err
	}
	pem, err := os.ReadFile(c.ClientCAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("client CA contains no certificates")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		intermediate, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		intermediates.AddCert(intermediate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, DNSName: ServerName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		VerifyConnection: func(state tls.ConnectionState) error {
			if !gatewayIdentity(&state, c.ClientURI) {
				return errors.New("gateway client identity required")
			}
			return nil
		},
	}, nil
}

func gatewayIdentity(state *tls.ConnectionState, identity string) bool {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return false
	}
	leaf := state.PeerCertificates[0]
	for _, uri := range leaf.URIs {
		if uri.String() == identity {
			return true
		}
	}
	return false
}

func (a *Agent) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              a.config.Listen,
		Handler:           a,
		TLSConfig:         a.tlsConfig.Clone(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 * 1024,
		BaseContext:       func(net.Listener) context.Context { return a.ctx },
	}
}

func (a *Agent) Close() error {
	a.closeOnce.Do(func() {
		a.lifeMu.Lock()
		a.closed = true
		a.cancel()
		a.lifeMu.Unlock()
		a.requests.Wait()
		a.transport.CloseIdleConnections()
		a.closeErr = a.ledger.db.Close()
	})
	return a.closeErr
}

func (a *Agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a.lifeMu.Lock()
	if a.closed {
		a.lifeMu.Unlock()
		writeError(w, 503, "upstream_unavailable", "Agent is stopping.", "")
		return
	}
	a.requests.Add(1)
	a.lifeMu.Unlock()
	defer a.requests.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(a.ctx, cancel)
	defer func() { stop(); cancel() }()
	r = r.WithContext(ctx)
	if !gatewayIdentity(r.TLS, a.config.ClientURI) {
		writeError(w, 401, "unauthorized", "Gateway client authentication required.", "")
		return
	}
	if !validPath(r) {
		writeError(w, 400, "invalid_request", "Invalid request path.", "")
		return
	}
	if r.Method == http.MethodConnect {
		writeError(w, 400, "invalid_request", "CONNECT is not supported.", "")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_roc-agent/") || r.URL.Path == "/_roc-agent" {
		fresh := r.URL.Path == apiPrefix+"status" && r.Method == http.MethodGet && r.URL.RawQuery == "fresh=1"
		if r.URL.RawQuery != "" && !fresh {
			writeError(w, 400, "invalid_request", "Unsupported agent API query parameters.", "")
			return
		}
		switch {
		case r.URL.Path == apiPrefix+"wake" && r.Method == http.MethodPost:
			a.wake(w, r)
		case strings.HasPrefix(r.URL.Path, apiPrefix+"wake/") && r.Method == http.MethodGet:
			a.query(w, r, strings.TrimPrefix(r.URL.Path, apiPrefix+"wake/"))
		case r.URL.Path == apiPrefix+"status" && r.Method == http.MethodGet:
			writeJSON(w, 200, a.probe(fresh))
		default:
			writeError(w, 404, "not_found", "Resource not found.", "")
		}
		return
	}
	if r.ContentLength > uploadLimit {
		writeError(w, 413, "invalid_request", "Upload exceeds the allowed size.", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)
	a.proxy.ServeHTTP(w, r)
}

func validPath(r *http.Request) bool {
	if r.URL.IsAbs() || r.URL.Host != "" || r.URL.Opaque != "" || !strings.HasPrefix(r.URL.Path, "/") || strings.Contains(r.URL.Path, "\\") {
		return false
	}
	if r.URL.RawPath != "" {
		decoded, err := url.PathUnescape(r.URL.RawPath)
		if err != nil || decoded != r.URL.Path {
			return false
		}
	}
	escaped := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return false
	}
	for _, part := range strings.Split(r.URL.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (a *Agent) wake(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID string `json:"requestId"`
		TargetID  string `json:"targetId"`
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, jsonLimit))
	if err != nil || strictJSON(data, &req) != nil || !validID(req.RequestID) || req.TargetID != "desktop" {
		writeError(w, 400, "invalid_request", "Invalid wake request.", "")
		return
	}
	result, fresh, err := a.ledger.reserve(req.RequestID, time.Now().UTC())
	if err != nil {
		var limit rateError
		switch {
		case errors.As(err, &limit):
			w.Header().Set("Retry-After", strconv.Itoa(limit.seconds))
			writeError(w, 429, "rate_limited", "Wait before another wake request.", req.RequestID)
		case errors.Is(err, errCapacity):
			w.Header().Set("Retry-After", "60")
			writeError(w, 429, "rate_limited", "Wake history is at capacity.", req.RequestID)
		default:
			writeError(w, 503, "upstream_unavailable", "Cannot persist wake request.", req.RequestID)
		}
		return
	}
	if fresh {
		// Acceptance is durable. Client disconnects do not cancel or retry the
		// command; agent shutdown and the three-second bound still cancel it.
		ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
		err := a.runner.Run(ctx, "/usr/bin/etherwake", "-i", "br-lan", "-b", "B0:25:AA:83:C6:EF")
		ctxErr := ctx.Err()
		cancel()
		result.Result, result.Code = commandOutcome(err, ctxErr)
		if result.Result == "sent" {
			now := time.Now().UTC()
			result.SentAt = &now
		}
		if err := a.ledger.finish(result); err != nil {
			result.Result, result.Code, result.SentAt = "unknown", "outcome_unknown", nil
		}
	}
	status := http.StatusAccepted
	if result.Result == "failed" {
		status = http.StatusBadGateway
	}
	if result.Result == "unknown" {
		status = http.StatusGatewayTimeout
	}
	writeJSON(w, status, result)
}

func commandOutcome(err, ctxErr error) (string, string) {
	if ctxErr != nil {
		return "unknown", "outcome_unknown"
	}
	if err == nil {
		return "sent", ""
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() < 0 {
		return "unknown", "outcome_unknown"
	}
	return "failed", "wake_failed"
}

func (a *Agent) query(w http.ResponseWriter, r *http.Request, id string) {
	if !validID(id) {
		writeError(w, 400, "invalid_request", "Invalid request ID.", "")
		return
	}
	result, err := a.ledger.lookup(id, time.Now().UTC())
	if errors.Is(err, errNotFound) {
		writeError(w, 404, "not_found", "Wake request not found.", id)
		return
	}
	if err != nil {
		writeError(w, 503, "upstream_unavailable", "Cannot read wake history.", id)
		return
	}
	writeJSON(w, 200, result)
}

func (a *Agent) probe(fresh bool) Status {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if !fresh && !a.status.CheckedAt.IsZero() && time.Since(a.status.CheckedAt) < 3*time.Second {
		return a.status
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	defer cancel()
	err := a.runner.Run(ctx, a.config.PingPath, "-c", "1", "-W", "1", "192.168.2.100")
	state := "unknown"
	if err == nil && ctx.Err() == nil {
		state = "reachable"
	} else if ctx.Err() == nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			state = "unreachable"
		}
	}
	a.status = Status{TargetID: "desktop", Reachability: state, CheckedAt: time.Now().UTC()}
	return a.status
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func writeError(w http.ResponseWriter, status int, code, message, id string) {
	writeJSON(w, status, struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"requestId,omitempty"`
	}{code, message, id})
}

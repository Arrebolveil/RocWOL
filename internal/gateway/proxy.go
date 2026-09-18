package gateway

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"hubu.wiki/roc-wol/internal/protocol"
)

type originKey struct{}
type upgradeBody struct {
	io.ReadWriteCloser
	once sync.Once
	done chan struct{}
}

func (b *upgradeBody) Close() error {
	var e error
	b.once.Do(func() { close(b.done); e = b.ReadWriteCloser.Close() })
	return e
}
func (g *Gateway) makeProxy(tr http.RoundTripper) *httputil.ReverseProxy {
	upstream, _ := url.Parse(g.cfg.AgentURL)
	return &httputil.ReverseProxy{
		Transport: tr, FlushInterval: -1, ErrorLog: log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = upstream.Scheme
			pr.Out.URL.Host = upstream.Host
			pr.Out.Host = pr.In.Host
			stripForwarding(pr.Out.Header)
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Cookie")
			for _, c := range pr.In.Cookies() {
				if c.Name != "__Host-roc_session" {
					pr.Out.AddCookie(c)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("Cache-Control", "no-store")
			resp.Header.Set("Referrer-Policy", "same-origin")
			cookies := resp.Cookies()
			resp.Header.Del("Set-Cookie")
			for _, c := range cookies {
				if c.Name == "__Host-roc_session" {
					continue
				}
				c.Secure = true
				c.Domain = ""
				resp.Header.Add("Set-Cookie", c.String())
			}
			if location := resp.Header.Get("Location"); location != "" {
				u, e := url.Parse(location)
				if e != nil || strings.ContainsAny(location, "\\\r\n\t") || strings.TrimSpace(location) != location || strings.HasPrefix(location, "//") {
					return errors.New("invalid upstream redirect")
				}
				origin, _ := resp.Request.Context().Value(originKey{}).(string)
				if u.IsAbs() || u.Host != "" {
					public, _ := url.Parse(origin)
					if u.User != nil {
						return errors.New("upstream redirect userinfo refused")
					}
					if u.Scheme == public.Scheme && u.Host == public.Host {
						// A same-origin absolute redirect is already safe; only the known
						// internal LuCI origin below may be rewritten.
					} else if u.Scheme == "http" && (u.Host == "127.0.0.1:80" || u.Host == "127.0.0.1") {
						u.Scheme = public.Scheme
						u.Host = public.Host
						resp.Header.Set("Location", u.String())
					} else {
						return errors.New("external upstream redirect refused")
					}
				}
			}
			if resp.StatusCode == http.StatusSwitchingProtocols {
				rw, ok := resp.Body.(io.ReadWriteCloser)
				if !ok {
					return errors.New("upstream upgrade is not bidirectional")
				}
				b := &upgradeBody{ReadWriteCloser: rw, done: make(chan struct{})}
				resp.Body = b
				go func() {
					select {
					case <-resp.Request.Context().Done():
						_ = b.Close()
					case <-b.done:
					}
				}()
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) { protocol.Fail(w, 503, "upstream_unavailable") },
	}
}

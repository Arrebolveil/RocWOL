package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"hubu.wiki/roc-wol/internal/protocol"
)

type Config struct {
	Listen          string   `json:"listen"`
	PublicOrigins   []string `json:"publicOrigins"`
	AgentURL        string   `json:"agentURL"`
	AgentServerName string   `json:"agentServerName"`
	CAFile          string   `json:"caFile"`
	ClientCertFile  string   `json:"clientCertFile"`
	ClientKeyFile   string   `json:"clientKeyFile"`
	DataFile        string   `json:"dataFile"`
	AdminSocket     string   `json:"adminSocket"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer f.Close()
	if e = protocol.DecodeJSON(f, &c, 64<<10); e != nil {
		return c, e
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	host, port, e := net.SplitHostPort(c.Listen)
	ip := net.ParseIP(host)
	if e != nil || ip == nil || !ip.IsLoopback() || port == "" || port == "0" {
		return errors.New("listen must be a loopback address with a nonzero port")
	}
	if len(c.PublicOrigins) == 0 {
		return errors.New("publicOrigins is required")
	}
	seen := map[string]bool{}
	for _, origin := range c.PublicOrigins {
		u, e := url.Parse(origin)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Port() != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Host != strings.ToLower(u.Host) || seen[origin] {
			return errors.New("publicOrigins must contain unique HTTPS origins")
		}
		seen[origin] = true
	}
	u, e := url.Parse(c.AgentURL)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Port() == "" {
		return errors.New("agentURL must be a loopback HTTPS origin with explicit port")
	}
	if c.AgentServerName != "router.roc-wol.internal" {
		return errors.New("invalid agentServerName")
	}
	for _, p := range []string{c.CAFile, c.ClientCertFile, c.ClientKeyFile, c.DataFile, c.AdminSocket} {
		if p == "" || !filepath.IsAbs(p) {
			return errors.New("all file paths must be absolute")
		}
	}
	return nil
}
func (c Config) TLSConfig() (*tls.Config, error) {
	roots := x509.NewCertPool()
	pem, e := os.ReadFile(c.CAFile)
	if e != nil {
		return nil, e
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid CA")
	}
	cert, e := tls.LoadX509KeyPair(c.ClientCertFile, c.ClientKeyFile)
	if e != nil {
		return nil, e
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.AgentServerName, RootCAs: roots, Certificates: []tls.Certificate{cert}}, nil
}

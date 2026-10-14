package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"hubu.wiki/roc-wol/internal/protocol"
)

type Config struct {
	Listen            string   `json:"listen"`
	PublicOrigins     []string `json:"publicOrigins"`
	AgentURL          string   `json:"agentURL"`
	AgentServerName   string   `json:"agentServerName"`
	CAFile            string   `json:"caFile"`
	ClientCertFile    string   `json:"clientCertFile"`
	ClientKeyFile     string   `json:"clientKeyFile"`
	DataFile          string   `json:"dataFile"`
	AdminSocket       string   `json:"adminSocket"`
	MaxDevices        int      `json:"maxDevices"`
	ManagementEnabled *bool    `json:"managementEnabled"`
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
	if e = c.Validate(); e != nil {
		return c, e
	}
	return c, nil
}
func (c *Config) Validate() error {
	host, port, e := net.SplitHostPort(c.Listen)
	ip := net.ParseIP(host)
	n, portErr := strconv.Atoi(port)
	if e != nil || ip == nil || !ip.IsLoopback() || portErr != nil || n < 1 || n > 65535 {
		return errors.New("listen must be a loopback address with a nonzero port")
	}
	if len(c.PublicOrigins) < 1 || len(c.PublicOrigins) > 8 {
		return errors.New("publicOrigins must contain 1–8 origins")
	}
	seen := map[string]bool{}
	for i, origin := range c.PublicOrigins {
		normalized, e := protocol.NormalizeOrigin(origin)
		if e != nil || seen[normalized] {
			return errors.New("publicOrigins must contain unique HTTPS origins")
		}
		c.PublicOrigins[i] = normalized
		seen[normalized] = true
	}
	if c.MaxDevices == 0 {
		c.MaxDevices = 5
	}
	if c.MaxDevices < 1 || c.MaxDevices > 100 {
		return errors.New("maxDevices must be between 1 and 100")
	}
	u, e := url.Parse(c.AgentURL)
	if e != nil {
		return errors.New("agentURL must be a loopback HTTPS origin with explicit port")
	}
	_, normalizeErr := protocol.NormalizeOrigin(c.AgentURL)
	agentPort, agentPortErr := strconv.Atoi(u.Port())
	if normalizeErr != nil || u.Scheme != "https" || u.Path != "" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || agentPortErr != nil || agentPort < 1 || agentPort > 65535 {
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

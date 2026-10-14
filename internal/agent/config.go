package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"hubu.wiki/roc-wol/internal/protocol"
)

const GatewayURI = "urn:roc-wol:gateway"
const ServerName = "router.roc-wol.internal"

type Target struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MAC       string `json:"mac"`
	Interface string `json:"interface"`
	Broadcast bool   `json:"broadcast"`
	IP        string `json:"ip"`
}

type Config struct {
	Listen         string   `json:"listen"`
	ServerCertFile string   `json:"serverCertFile"`
	ServerKeyFile  string   `json:"serverKeyFile"`
	ClientCAFile   string   `json:"clientCAFile"`
	ClientURI      string   `json:"clientURI"`
	DataFile       string   `json:"dataFile"`
	LuCIOrigin     string   `json:"luciOrigin"`
	PingPath       string   `json:"pingPath"`
	EtherwakePath  string   `json:"etherwakePath"`
	Targets        []Target `json:"targets"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if len(data) > 64*1024 {
		return c, errors.New("configuration exceeds 64 KiB")
	}
	if err := strictJSON(data, &c); err != nil {
		return c, fmt.Errorf("configuration: %w", err)
	}
	c = c.normalized()
	return c, c.Validate()
}

func (c Config) normalized() Config {
	if c.EtherwakePath == "" {
		c.EtherwakePath = "/usr/bin/etherwake"
	}
	return c
}

func (c Config) Validate() error {
	c = c.normalized()
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return errors.New("listen must be a loopback IP and port")
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || n < 1 || n > 65535 {
		return errors.New("listen must be a loopback IP and port")
	}
	if c.ClientURI != GatewayURI {
		return errors.New("clientURI must identify the roc-wol gateway")
	}
	for _, p := range []string{c.ServerCertFile, c.ServerKeyFile, c.ClientCAFile, c.DataFile, c.PingPath, c.EtherwakePath} {
		if p == "" || !(filepath.IsAbs(p) || path.IsAbs(p)) || strings.ContainsAny(p, "\x00\r\n") {
			return errors.New("certificate, key, database and executable paths must be absolute")
		}
	}
	if c.LuCIOrigin != "" {
		if _, err := luciOrigin(c.LuCIOrigin); err != nil {
			return err
		}
	}
	if len(c.Targets) < 1 || len(c.Targets) > 64 {
		return errors.New("between 1 and 64 targets are required")
	}
	seen := make(map[string]bool, len(c.Targets))
	for _, t := range c.Targets {
		if !protocol.ValidTargetInfo(protocol.TargetInfo{TargetID: t.ID, Name: t.Name, MAC: t.MAC, IP: t.IP}) || seen[t.ID] {
			return errors.New("invalid or duplicate target")
		}
		seen[t.ID] = true
		if len(t.Interface) < 1 || len(t.Interface) > 15 {
			return errors.New("invalid target interface")
		}
		for _, ch := range t.Interface {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("_.:-", ch)) {
				return errors.New("invalid target interface")
			}
		}
	}
	return nil
}

// luciOrigin deliberately accepts only an IP literal: DNS resolution must not
// turn a configured loopback management endpoint into a remote destination.
func luciOrigin(value string) (*url.URL, error) {
	invalid := errors.New("luciOrigin must be a loopback HTTP origin")
	if strings.ContainsAny(value, "\\%?# \t\r\n") {
		return nil, invalid
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, invalid
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || strings.HasSuffix(u.Host, ":") {
		return nil, invalid
	}
	port := 80
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, invalid
		}
	}
	u.Host = net.JoinHostPort(ip.String(), strconv.Itoa(port))
	return u, nil
}

func (c Config) validateRuntime() error {
	for _, path := range []string{c.PingPath, c.EtherwakePath} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return errors.New("configured executable is unavailable or not executable")
		}
	}
	seen := make(map[string]bool, len(c.Targets))
	for _, target := range c.Targets {
		if seen[target.Interface] {
			continue
		}
		if _, err := net.InterfaceByName(target.Interface); err != nil {
			return errors.New("configured target interface is unavailable")
		}
		seen[target.Interface] = true
	}
	return nil
}

// Reject duplicate keys as well as unknown fields and trailing JSON values.
func strictJSON(data []byte, dst any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON must be valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var jsonField func(reflect.Type, string) reflect.Type
	jsonField = func(typ reflect.Type, key string) reflect.Type {
		for i := range typ.NumField() {
			field := typ.Field(i)
			if strings.Split(field.Tag.Get("json"), ",")[0] == key {
				return field.Type
			}
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				if nested := jsonField(field.Type, key); nested != nil {
					return nested
				}
			}
		}
		return nil
	}
	var value func(reflect.Type) error
	value = func(typ reflect.Type) error {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			if typ.Kind() != reflect.Struct {
				return errors.New("unexpected JSON object")
			}
			keys := make(map[string]bool)
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				k, ok := key.(string)
				if !ok || keys[k] {
					return errors.New("duplicate or invalid JSON key")
				}
				keys[k] = true
				fieldType := jsonField(typ, k)
				if fieldType == nil {
					return errors.New("unknown JSON field")
				}
				if err := value(fieldType); err != nil {
					return err
				}
			}
		case '[':
			if typ.Kind() != reflect.Slice {
				return errors.New("unexpected JSON array")
			}
			for dec.More() {
				if err := value(typ.Elem()); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = dec.Token()
		return err
	}
	if err := value(reflect.TypeOf(dst)); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

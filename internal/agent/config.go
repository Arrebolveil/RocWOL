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
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
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
	return c, c.Validate()
}

func (c Config) Validate() error {
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
	for _, p := range []string{c.ServerCertFile, c.ServerKeyFile, c.ClientCAFile, c.DataFile, c.PingPath} {
		if p == "" || !filepath.IsAbs(p) {
			return errors.New("certificate, key, database and ping paths must be absolute")
		}
	}
	u, err := url.Parse(c.LuCIOrigin)
	if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:80" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return errors.New("luciOrigin must be http://127.0.0.1:80")
	}
	if len(c.Targets) != 1 {
		return errors.New("exactly one configured desktop target is required")
	}
	t := c.Targets[0]
	if t.ID != "desktop" || t.Name != "DESKTOP-IENBTJG" || t.MAC != "B0:25:AA:83:C6:EF" || t.Interface != "br-lan" || !t.Broadcast || t.IP != "192.168.2.100" {
		return errors.New("target does not match the approved fixed desktop")
	}
	return nil
}

// Reject duplicate keys as well as unknown fields and trailing JSON values.
func strictJSON(data []byte, dst any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON must be valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
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
				var fieldType reflect.Type
				for i := range typ.NumField() {
					field := typ.Field(i)
					if strings.Split(field.Tag.Get("json"), ",")[0] == k {
						fieldType = field.Type
						break
					}
				}
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

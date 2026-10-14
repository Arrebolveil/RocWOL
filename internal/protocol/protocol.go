package protocol

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const ServiceID = "roc-wol"
const JSONLimit = 16 << 10
const ResponseJSONLimit = 64 << 10
const APIVersion = 2
const UploadLimit = 128 << 20

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
}
type Wake struct {
	TargetID  string     `json:"targetId"`
	RequestID string     `json:"requestId"`
	Result    string     `json:"result"`
	SentAt    *time.Time `json:"sentAt,omitempty"`
	Code      string     `json:"code,omitempty"`
}

type Info struct {
	ServiceID  string `json:"serviceId"`
	APIVersion int    `json:"apiVersion"`
	InstanceID string `json:"instanceId"`
}
type TargetInfo struct {
	TargetID string `json:"targetId"`
	Name     string `json:"name"`
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
}
type TargetList struct {
	Targets           []TargetInfo `json:"targets"`
	ManagementEnabled bool         `json:"managementEnabled"`
}
type Status struct {
	TargetID     string     `json:"targetId"`
	Reachability string     `json:"reachability"`
	CheckedAt    *time.Time `json:"checkedAt"`
}
type Challenge struct {
	ChallengeID string    `json:"challengeId"`
	Nonce       string    `json:"nonce"`
	ExpiresAt   time.Time `json:"expiresAt"`
	ServiceID   string    `json:"serviceId,omitempty"`
}
type Credentials struct {
	DeviceID    string    `json:"deviceId,omitempty"`
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// ManagementAuthorization uses SessionMinutes == 0 and a zero ExpiresAt for
// revocable, non-expiring grants. Challenges and browser tickets remain finite.
type ManagementAuthorization struct {
	Active          bool      `json:"active"`
	ExpiresAt       time.Time `json:"expiresAt"`
	SessionMinutes  int       `json:"sessionMinutes"`
	AllowAllDevices bool      `json:"allowAllDevices"`
	Origin          string    `json:"origin"`
}

type ManagementBrowser struct {
	URL                    string    `json:"url"`
	ExpiresAt              time.Time `json:"expiresAt"`
	AuthorizationExpiresAt time.Time `json:"authorizationExpiresAt"`
}

type DeviceAdministrationAccess struct {
	DeviceID             string `json:"deviceId"`
	DeviceAdministration bool   `json:"deviceAdministration"`
}

type AdministeredDevice struct {
	DeviceID             string    `json:"deviceId"`
	DeviceName           string    `json:"deviceName"`
	CreatedAt            time.Time `json:"createdAt"`
	Current              bool      `json:"current"`
	DeviceAdministration bool      `json:"deviceAdministration"`
}

type DeviceList struct {
	Devices  []AdministeredDevice `json:"devices"`
	Capacity int                  `json:"capacity"`
}

type DeviceRevocation struct {
	DeviceID string `json:"deviceId"`
	Revoked  bool   `json:"revoked"`
	Current  bool   `json:"current"`
}

func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func ID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func ValidID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func ValidTargetID(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-')) {
			return false
		}
	}
	return true
}
func ValidName(s string) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) < 1 || utf8.RuneCountInString(s) > 64 {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func ValidTargetInfo(t TargetInfo) bool {
	mac, err := net.ParseMAC(t.MAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 {
		return false
	}
	nonzero := false
	for _, b := range mac {
		nonzero = nonzero || b != 0
	}
	ip, err := netip.ParseAddr(t.IP)
	return ValidTargetID(t.TargetID) && ValidName(t.Name) && nonzero && err == nil &&
		ip.Is4() && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLoopback() && ip != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}
func ValidTargetList(v TargetList) bool {
	if len(v.Targets) < 1 || len(v.Targets) > 64 {
		return false
	}
	seen := make(map[string]bool, len(v.Targets))
	for _, t := range v.Targets {
		if !ValidTargetInfo(t) || seen[t.TargetID] {
			return false
		}
		seen[t.TargetID] = true
	}
	return true
}

// NormalizeOrigin accepts only a bare HTTPS origin, optionally ending in "/".
func NormalizeOrigin(value string) (string, error) {
	invalid := errors.New("invalid HTTPS origin")
	if len(value) == 0 || len(value) > 512 {
		return "", invalid
	}
	for _, c := range value {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return "", invalid
		}
	}
	u, err := url.Parse(value)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil || u.Opaque != "" || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(value, "\\%#?") {
		return "", invalid
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", invalid
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Is6() {
			if !strings.HasPrefix(u.Host, "[") {
				return "", invalid
			}
			host = "[" + ip.String() + "]"
		} else {
			if strings.HasPrefix(u.Host, "[") {
				return "", invalid
			}
			host = ip.String()
		}
	} else {
		if len(host) > 253 || strings.HasSuffix(host, ".") {
			return "", invalid
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", invalid
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return "", invalid
				}
			}
		}
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", invalid
		}
		if n != 443 {
			host += ":" + strconv.Itoa(n)
		}
	}
	return "https://" + host, nil
}
func Hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func Decode(s string) ([]byte, error) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(s)
	if e != nil || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, errors.New("invalid encoding")
	}
	return b, nil
}
func PublicKey(s string) (*ecdsa.PublicKey, []byte, error) {
	b, e := Decode(s)
	if e != nil {
		return nil, nil, e
	}
	v, e := x509.ParsePKIXPublicKey(b)
	if e != nil {
		return nil, nil, e
	}
	k, ok := v.(*ecdsa.PublicKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, nil, errors.New("P-256 required")
	}
	return k, b, nil
}
func Verify(key, signature, message string) bool {
	k, _, e := PublicKey(key)
	if e != nil {
		return false
	}
	sig, e := Decode(signature)
	if e != nil {
		return false
	}
	h := sha256.Sum256([]byte(message))
	return ecdsa.VerifyASN1(k, h[:], sig)
}
func PairMessage(id, nonce, wake, manage string) string {
	_, w, _ := PublicKey(wake)
	_, m, _ := PublicKey(manage)
	return strings.Join([]string{"ROCWOL-PAIR-V1", ServiceID, id, nonce, Hash(string(w)), Hash(string(m))}, "\n")
}
func WakeMessage(device, id, nonce string) string {
	return strings.Join([]string{"ROCWOL-WAKE-AUTH-V1", ServiceID, device, id, nonce}, "\n")
}
func ManageMessage(device, id, nonce, origin string, sessionMinutes int, allowAllDevices bool) string {
	scope := "browser-only"
	if allowAllDevices {
		scope = "all-devices"
	}
	return strings.Join([]string{"ROCWOL-MANAGE-V2", ServiceID, device, id, nonce, origin, strconv.Itoa(sessionMinutes), scope}, "\n")
}

func DeviceAdministrationMessage(device, id, nonce, origin, action, target string) string {
	return strings.Join([]string{"ROCWOL-DEVICE-ADMIN-V1", ServiceID, device, id, nonce, origin, action, target}, "\n")
}

// DecodeJSON rejects duplicate keys as well as unknown fields, malformed UTF-8,
// trailing values and non-object top-level payloads.
func DecodeJSON(r io.Reader, dst any, limit int64) error {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil {
		return e
	}
	if int64(len(b)) > limit || !utf8.Valid(b) {
		return errors.New("invalid JSON size or encoding")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return errors.New("JSON object required")
	}
	if e = uniqueObject(d); e != nil {
		return e
	}
	if _, e = d.Token(); e != io.EOF {
		return errors.New("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}
func uniqueObject(d *json.Decoder) error {
	seen := map[string]bool{}
	for d.More() {
		t, e := d.Token()
		if e != nil {
			return e
		}
		s, ok := t.(string)
		if !ok || seen[s] {
			return errors.New("duplicate JSON key")
		}
		seen[s] = true
		if e = uniqueValue(d); e != nil {
			return e
		}
	}
	t, e := d.Token()
	if e != nil || t != json.Delim('}') {
		return errors.New("invalid object")
	}
	return nil
}
func uniqueValue(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	switch t {
	case json.Delim('{'):
		return uniqueObject(d)
	case json.Delim('['):
		for d.More() {
			if e = uniqueValue(d); e != nil {
				return e
			}
		}
		_, e = d.Token()
		return e
	}
	return nil
}
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func Fail(w http.ResponseWriter, status int, code string) {
	JSON(w, status, Error{Code: code, Message: code})
}
func SafePath(u *url.URL) bool {
	if u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.Contains(u.Path, "\\") {
		return false
	}
	if u.RawPath != "" {
		decoded, e := url.PathUnescape(u.RawPath)
		if e != nil || decoded != u.Path {
			return false
		}
	}
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") {
		return false
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
	"hubu.wiki/roc-wol/internal/store"
)

func v2Fixture(t *testing.T, handler http.HandlerFunc) *Gateway {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway(Config{PublicOrigins: []string{testOrigin}, AgentURL: up.URL}, db, http.DefaultTransport)
	t.Cleanup(func() { g.Close() })
	return g
}
func testTargets() protocol.TargetList {
	return protocol.TargetList{Targets: []protocol.TargetInfo{
		{TargetID: "pc-a", Name: "Computer A", MAC: "02:00:00:00:00:01", IP: "192.0.2.10"},
		{TargetID: "pc-b", Name: "Computer B", MAC: "02:00:00:00:00:02", IP: "192.0.2.11"},
	}, ManagementEnabled: true}
}
func TestV2InfoAndUpgradeRefusal(t *testing.T) {
	var calls atomic.Int32
	g := v2Fixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
	w := request(g, "GET", "/_wol/v2/info", "", "", "")
	var info protocol.Info
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &info) != nil || info.ServiceID != protocol.ServiceID || info.APIVersion != 2 || !protocol.ValidID(info.InstanceID) {
		t.Fatalf("info: %d %s", w.Code, w.Body.String())
	}
	var fields map[string]any
	json.Unmarshal(w.Body.Bytes(), &fields)
	if len(fields) != 3 || calls.Load() != 0 {
		t.Fatal("info disclosed extra data or queried private agent")
	}
	for _, path := range []string{"/_wol/v1/info", "/_wol/v1/target", "/_wol/v1/wake", "/_wol/v1/manage/redeem"} {
		for _, method := range []string{"GET", "POST"} {
			w = request(g, method, path, "", "", "{}")
			if w.Code != 426 || !strings.Contains(w.Body.String(), "protocol_upgrade_required") {
				t.Fatalf("upgrade refusal %s %s: %d", method, path, w.Code)
			}
		}
	}
	if w = request(g, "GET", "/_wol/v2/info?fresh=1", "", "", ""); w.Code != 400 {
		t.Fatal("info admitted fresh query")
	}
}
func TestV2TargetsSnapshotAndRemoval(t *testing.T) {
	var offline, removed atomic.Bool
	var probes atomic.Int32
	g := v2Fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/_roc-agent/v2/targets" {
			list := testTargets()
			if removed.Load() {
				list.Targets = list.Targets[1:]
			}
			protocol.JSON(w, 200, list)
			return
		}
		probes.Add(1)
		now := time.Now().UTC()
		protocol.JSON(w, 200, protocol.Status{TargetID: "pc-a", Reachability: "reachable", CheckedAt: &now})
	})
	id := identity(t, g)
	if w := request(g, "GET", "/_wol/v2/targets", id.AccessToken, "", ""); w.Code != 200 || probes.Load() != 0 {
		t.Fatalf("list probed targets: %d", w.Code)
	}
	w := request(g, "GET", "/_wol/v2/targets/pc-a", id.AccessToken, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"Computer A"`) || !strings.Contains(w.Body.String(), `"router":"available"`) {
		t.Fatalf("target metadata: %d %s", w.Code, w.Body.String())
	}
	offline.Store(true)
	if w := request(g, "GET", "/_wol/v2/targets", id.AccessToken, "", ""); w.Code != 503 {
		t.Fatal("offline list served cached authority")
	}
	w = request(g, "GET", "/_wol/v2/targets/pc-a", id.AccessToken, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"router":"unavailable"`) || !strings.Contains(w.Body.String(), `"checkedAt":null`) || !strings.Contains(w.Body.String(), `"name":"Computer A"`) {
		t.Fatalf("cached metadata: %d %s", w.Code, w.Body.String())
	}
	if w := request(g, "POST", "/_wol/v2/wake", id.AccessToken, "", `{"targetId":"pc-a","requestId":"`+strings.Repeat("a", 32)+`"}`); w.Code != 503 {
		t.Fatal("offline snapshot permitted wake")
	}
	if w := request(g, "GET", "/_wol/v2/targets/missing", id.AccessToken, "", ""); w.Code != 503 {
		t.Fatal("unknown offline metadata fabricated")
	}
	offline.Store(false)
	removed.Store(true)
	if w := request(g, "GET", "/_wol/v2/targets/pc-a", id.AccessToken, "", ""); w.Code != 404 {
		t.Fatal("removed target survived fresh list")
	}
	if w := request(g, "GET", "/_wol/v2/targets/INVALID", id.AccessToken, "", ""); w.Code != 400 {
		t.Fatal("malformed target was not invalid_request")
	}
}
func TestV2WakeBindingsAndSelectedTarget(t *testing.T) {
	var sends atomic.Int32
	var selected atomic.Value
	g := v2Fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_roc-agent/v2/targets" {
			protocol.JSON(w, 200, testTargets())
			return
		}
		if r.Method != "POST" {
			t.Error("terminal wake queried agent")
			w.WriteHeader(500)
			return
		}
		var input struct {
			RequestID string `json:"requestId"`
			TargetID  string `json:"targetId"`
		}
		if err := protocol.DecodeJSON(r.Body, &input, protocol.JSONLimit); err != nil {
			t.Error(err)
			return
		}
		sends.Add(1)
		selected.Store(input.TargetID)
		now := time.Now().UTC()
		protocol.JSON(w, 202, protocol.Wake{TargetID: input.TargetID, RequestID: input.RequestID, Result: "sent", SentAt: &now})
	})
	id := identity(t, g)
	rid := strings.Repeat("b", 32)
	wake := func(target string) *httptest.ResponseRecorder {
		return request(g, "POST", "/_wol/v2/wake", id.AccessToken, "", fmt.Sprintf(`{"targetId":%q,"requestId":%q}`, target, rid))
	}
	if w := wake("pc-b"); w.Code != 202 || selected.Load() != "pc-b" {
		t.Fatalf("wrong target sent: %d", w.Code)
	}
	if w := wake("pc-a"); w.Code != 409 || !strings.Contains(w.Body.String(), "request_target_mismatch") {
		t.Fatalf("binding mismatch: %d %s", w.Code, w.Body.String())
	}
	if w := wake("pc-b"); w.Code != 202 || sends.Load() != 1 {
		t.Fatal("same-target replay dispatched")
	}
	other := identity(t, g)
	if w := request(g, "GET", "/_wol/v2/wake/"+rid, other.AccessToken, "", ""); w.Code != 404 {
		t.Fatal("device binding failed")
	}
	last, err := g.DB.LastWake(id.DeviceID, "pc-a")
	if err != nil || last != nil {
		t.Fatal("last wake crossed targets")
	}
	last, err = g.DB.LastWake(id.DeviceID, "pc-b")
	if err != nil || last == nil || last.TargetID != "pc-b" {
		t.Fatal("target last wake missing")
	}
}
func TestV2AgentDefiniteRejectionsAndMalformedRejection(t *testing.T) {
	for _, tc := range []struct {
		status   int
		code     string
		bad      bool
		expected int
	}{{404, "target_not_found", false, 404}, {409, "request_target_mismatch", false, 409}, {404, "target_not_found", true, 504}} {
		t.Run(fmt.Sprintf("%s-bad-%t", tc.code, tc.bad), func(t *testing.T) {
			var sends atomic.Int32
			g := v2Fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_roc-agent/v2/targets" {
					protocol.JSON(w, 200, testTargets())
					return
				}
				if r.Method == "GET" {
					w.WriteHeader(404)
					return
				}
				sends.Add(1)
				target := "pc-a"
				if tc.bad {
					target = "pc-b"
				}
				protocol.JSON(w, tc.status, protocol.Wake{RequestID: strings.Repeat("c", 32), TargetID: target, Result: "failed", Code: tc.code})
			})
			id := identity(t, g)
			payload := `{"targetId":"pc-a","requestId":"` + strings.Repeat("c", 32) + `"}`
			w := request(g, "POST", "/_wol/v2/wake", id.AccessToken, "", payload)
			if w.Code != tc.expected {
				t.Fatalf("upstream rejection: %d %s", w.Code, w.Body.String())
			}
			w = request(g, "POST", "/_wol/v2/wake", id.AccessToken, "", payload)
			if w.Code != tc.expected || sends.Load() != 1 {
				t.Fatalf("rejected replay: %d sends=%d", w.Code, sends.Load())
			}
			w = request(g, "GET", "/_wol/v2/wake/"+strings.Repeat("c", 32), id.AccessToken, "", "")
			if w.Code != tc.expected {
				t.Fatalf("terminal query mapping: %d", w.Code)
			}
			if !tc.bad && !strings.Contains(w.Body.String(), tc.code) {
				t.Fatal("controlled rejection code lost")
			}
		})
	}
}
func TestV2ManagementDisabledAndUnknown(t *testing.T) {
	for _, mode := range []string{"gateway", "agent", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			var disabled, unavailable atomic.Bool
			var proxyHits atomic.Int32
			g := v2Fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_roc-agent/v2/targets" {
					if unavailable.Load() {
						w.WriteHeader(503)
						return
					}
					list := testTargets()
					list.ManagementEnabled = !disabled.Load()
					protocol.JSON(w, 200, list)
					return
				}
				proxyHits.Add(1)
				w.WriteHeader(200)
			})
			id := identity(t, g)
			cookie := session(t, g, id, 0)
			link, err := g.Auth.ManagementBrowser(id.DeviceID, testOrigin)
			if err != nil {
				t.Fatal(err)
			}
			expected := 403
			switch mode {
			case "gateway":
				flag := false
				g.cfg.ManagementEnabled = &flag
			case "agent":
				disabled.Store(true)
			case "unavailable":
				unavailable.Store(true)
				expected = 503
			}
			for _, path := range []string{"challenge", "authorize", "status", "browser", "redeem"} {
				method := "POST"
				if path == "status" {
					method = "GET"
				}
				payload := "{}"
				if path == "redeem" {
					payload = fmt.Sprintf(`{"ticket":%q}`, strings.TrimPrefix(link.URL, testOrigin+"/_wol/manage#t="))
				}
				w := request(g, method, "/_wol/v2/manage/"+path, id.AccessToken, "", payload)
				if w.Code != expected {
					t.Fatalf("%s while %s: %d %s", path, mode, w.Code, w.Body.String())
				}
			}
			for _, upgrade := range []bool{false, true} {
				r := httptest.NewRequest("GET", "/cgi-bin/luci/", nil)
				r.Host = strings.TrimPrefix(testOrigin, "https://")
				r.Header.Set("Origin", testOrigin)
				r.AddCookie(&http.Cookie{Name: "__Host-roc_session", Value: cookie})
				if upgrade {
					r.Header.Set("Connection", "Upgrade")
					r.Header.Set("Upgrade", "websocket")
				}
				w := httptest.NewRecorder()
				g.ServeHTTP(w, r)
				if w.Code != 401 && w.Code != 503 {
					t.Fatalf("cookie bypassed disabled management: %d", w.Code)
				}
			}
			if proxyHits.Load() != 0 {
				t.Fatal("disabled management reached proxy")
			}
			if w := request(g, "POST", "/_wol/v2/manage/end", id.AccessToken, "", "{}"); w.Code != 200 {
				t.Fatal("disabled management prevented revocation")
			}
			if w := request(g, "GET", "/_wol/v2/devices/access", id.AccessToken, "", ""); w.Code != 200 {
				t.Fatal("management switch disabled device administration")
			}
		})
	}
}
func TestV2ListRejectsInvalidAndAcceptsMaximum(t *testing.T) {
	list := testTargets()
	for _, kind := range []string{"duplicate", "invalid", "too-many", "maximum"} {
		t.Run(kind, func(t *testing.T) {
			current := list
			current.Targets = append([]protocol.TargetInfo(nil), list.Targets...)
			switch kind {
			case "duplicate":
				current.Targets[1].TargetID = current.Targets[0].TargetID
			case "invalid":
				current.Targets[0].MAC = "ff:ff:ff:ff:ff:ff"
			case "too-many", "maximum":
				count := 64
				if kind == "too-many" {
					count = 65
				}
				current.Targets = make([]protocol.TargetInfo, count)
				for i := range current.Targets {
					current.Targets[i] = protocol.TargetInfo{TargetID: fmt.Sprintf("pc-%d", i), Name: strings.Repeat("电", 64), MAC: "02:00:00:00:00:01", IP: "192.0.2.1"}
				}
			}
			g := v2Fixture(t, func(w http.ResponseWriter, r *http.Request) { protocol.JSON(w, 200, current) })
			id := identity(t, g)
			w := request(g, "GET", "/_wol/v2/targets", id.AccessToken, "", "")
			expected := 503
			if kind == "maximum" {
				expected = 200
			}
			if w.Code != expected {
				t.Fatalf("list %s: %d %s", kind, w.Code, w.Body.String())
			}
		})
	}
}

func TestV2ConfigCapacityAndOriginNormalization(t *testing.T) {
	base := Config{Listen: "127.0.0.1:18081", PublicOrigins: []string{"https://WOL.EXAMPLE:8443/"}, AgentURL: "https://127.0.0.1:18082", AgentServerName: "router.roc-wol.internal"}
	path := filepath.Join(t.TempDir(), "file")
	base.CAFile, base.ClientCertFile, base.ClientKeyFile, base.DataFile, base.AdminSocket = path, path, path, path, path
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "gateway.json")
	if err = os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(configPath)
	if err != nil || loaded.MaxDevices != 5 || loaded.PublicOrigins[0] != "https://wol.example:8443" || loaded.ManagementEnabled != nil {
		t.Fatalf("configuration defaults: %+v %v", loaded, err)
	}
	for _, capacity := range []int{-1, 101} {
		c := base
		c.MaxDevices = capacity
		if c.Validate() == nil {
			t.Fatalf("invalid capacity accepted: %d", capacity)
		}
	}
	for _, origin := range []string{"http://wol.example", "https://wol.example/path", "https://wol.example:65536", "https://user@wol.example"} {
		c := base
		c.PublicOrigins = []string{origin}
		if c.Validate() == nil {
			t.Fatalf("invalid origin accepted: %s", origin)
		}
	}
	for _, agentURL := range []string{"https://127.0.0.1:65536", "https://127.0.0.1", "https://192.0.2.1:18082", "https://user@127.0.0.1:18082", "%"} {
		c := base
		c.AgentURL = agentURL
		if c.Validate() == nil {
			t.Fatalf("invalid agent origin accepted: %s", agentURL)
		}
	}
	c := base
	c.PublicOrigins = []string{"https://wol.example", "https://WOL.EXAMPLE:443/"}
	if c.Validate() == nil {
		t.Fatal("normalized duplicate origin accepted")
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway(loaded, db, http.DefaultTransport)
	defer g.Close()
	r := httptest.NewRequest("GET", "/_wol/v2/info", nil)
	r.Host = "wol.example:8443"
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("non-default public port rejected:", w.Code)
	}
	r = httptest.NewRequest("GET", "/_wol/v2/info", nil)
	r.Host = "wol.example"
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 421 {
		t.Fatal("host port binding bypassed")
	}
}

func TestV2ProxyRejectsInternalOriginAndAllowsPublicOrigin(t *testing.T) {
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal":
			w.Header().Set("Location", "http://127.0.0.1:80/cgi-bin/luci/")
		case "/userinfo":
			w.Header().Set("Location", testOrigin[:8]+"user@"+testOrigin[8:]+"/cgi-bin/luci/")
		case "/control":
			w.Header().Set("Location", "/cgi-bin/luci/\u0085")
		case "/escaped-control":
			w.Header().Set("Location", "/cgi-bin/luci/%1f")
		case "/escaped-network":
			w.Header().Set("Location", "/%2foutside.invalid")
		case "/ambiguous":
			w.Header().Add("Location", "/safe")
			w.Header().Add("Location", "//outside.invalid")
		default:
			w.Header().Set("Location", testOrigin+"/cgi-bin/luci/?a=1#section")
		}
		w.WriteHeader(302)
	})
	id := identity(t, g)
	cookie := session(t, g, id, 10)
	for _, path := range []string{"/internal", "/userinfo", "/control", "/escaped-control", "/escaped-network", "/ambiguous"} {
		if w := request(g, "GET", path, "", cookie, ""); w.Code != 503 {
			t.Fatalf("unsafe redirect survived: %s %d", path, w.Code)
		}
	}
	if w := request(g, "GET", "/public", "", cookie, ""); w.Code != 302 || w.Header().Get("Location") != testOrigin+"/cgi-bin/luci/?a=1#section" {
		t.Fatalf("same-origin redirect altered: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestV2DeviceLimitsCannotBeBypassedByTargetRotation(t *testing.T) {
	var sends atomic.Int32
	g := v2Fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_roc-agent/v2/targets" {
			protocol.JSON(w, 200, testTargets())
			return
		}
		if r.Method == "POST" {
			var input struct {
				RequestID string `json:"requestId"`
				TargetID  string `json:"targetId"`
			}
			if err := protocol.DecodeJSON(r.Body, &input, protocol.JSONLimit); err != nil {
				t.Error(err)
				return
			}
			sends.Add(1)
			now := time.Now().UTC()
			protocol.JSON(w, 202, protocol.Wake{RequestID: input.RequestID, TargetID: input.TargetID, Result: "sent", SentAt: &now})
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/_roc-agent/v2/targets/"), "/status")
		now := time.Now().UTC()
		protocol.JSON(w, 200, protocol.Status{TargetID: id, Reachability: "reachable", CheckedAt: &now})
	})
	id := identity(t, g)
	for i := 0; i < 7; i++ {
		target := "pc-a"
		if i%2 == 1 {
			target = "pc-b"
		}
		w := request(g, "POST", "/_wol/v2/wake", id.AccessToken, "", fmt.Sprintf(`{"targetId":%q,"requestId":"%032x"}`, target, i+1))
		expected := 202
		if i == 6 {
			expected = 429
		}
		if w.Code != expected {
			t.Fatalf("wake %d target %s: %d", i, target, w.Code)
		}
	}
	if sends.Load() != 6 {
		t.Fatal("rotation bypassed device wake limit")
	}
	if w := request(g, "GET", "/_wol/v2/targets/pc-a?fresh=1", id.AccessToken, "", ""); w.Code != 200 {
		t.Fatal("fresh request failed")
	}
	if w := request(g, "GET", "/_wol/v2/targets/pc-b?fresh=1", id.AccessToken, "", ""); w.Code != 429 {
		t.Fatal("target rotation bypassed fresh limit")
	}
}

func TestV2HistoricalWakeSurvivesTargetRemoval(t *testing.T) {
	var gone atomic.Bool
	var sends atomic.Int32
	g := v2Fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if gone.Load() {
			t.Error("historical request consulted removed target")
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/_roc-agent/v2/targets" {
			protocol.JSON(w, 200, testTargets())
			return
		}
		sends.Add(1)
		now := time.Now().UTC()
		protocol.JSON(w, 202, protocol.Wake{RequestID: strings.Repeat("d", 32), TargetID: "pc-a", Result: "sent", SentAt: &now})
	})
	id := identity(t, g)
	payload := `{"requestId":"` + strings.Repeat("d", 32) + `","targetId":"pc-a"}`
	if w := request(g, "POST", "/_wol/v2/wake", id.AccessToken, "", payload); w.Code != 202 {
		t.Fatal("initial wake failed")
	}
	gone.Store(true)
	if w := request(g, "GET", "/_wol/v2/wake/"+strings.Repeat("d", 32), id.AccessToken, "", ""); w.Code != 200 {
		t.Fatal("historical query depends on current target")
	}
	if w := request(g, "POST", "/_wol/v2/wake", id.AccessToken, "", payload); w.Code != 202 || sends.Load() != 1 {
		t.Fatal("historical replay dispatched")
	}
}

func TestV2AgentListRequiresCapabilityAndBoundedResponse(t *testing.T) {
	for _, payload := range []string{`{"targets":[{"targetId":"pc-a","name":"A","mac":"02:00:00:00:00:01","ip":"192.0.2.1"}]}`, `{"targets":[],"managementEnabled":true,"padding":"` + strings.Repeat("x", protocol.ResponseJSONLimit) + `"}`} {
		g := v2Fixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, payload)
		})
		id := identity(t, g)
		if w := request(g, "GET", "/_wol/v2/targets", id.AccessToken, "", ""); w.Code != 503 {
			t.Fatal("malformed or oversized list admitted")
		}
		if w := request(g, "GET", "/_wol/v2/targets/pc-a", id.AccessToken, "", ""); w.Code != 503 {
			t.Fatal("metadata fabricated without a snapshot")
		}
	}
}

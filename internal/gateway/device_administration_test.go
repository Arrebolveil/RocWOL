package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hubu.wiki/roc-wol/internal/protocol"
)

func deviceProof(t *testing.T, g *Gateway, id testIdentity, action, target string) string {
	t.Helper()
	payload, e := json.Marshal(map[string]string{"origin": testOrigin, "action": action, "targetDeviceId": target})
	if e != nil {
		t.Fatal(e)
	}
	w := request(g, "POST", "/_wol/v1/devices/challenge", id.AccessToken, "", string(payload))
	if w.Code != 200 {
		t.Fatalf("administration challenge: %d %s", w.Code, w.Body.String())
	}
	var c protocol.Challenge
	if e = json.Unmarshal(w.Body.Bytes(), &c); e != nil {
		t.Fatal(e)
	}
	proof := map[string]string{"challengeId": c.ChallengeID, "signature": testSign(t, id.manage,
		protocol.DeviceAdministrationMessage(id.DeviceID, c.ChallengeID, c.Nonce, testOrigin, action, target))}
	if action == "revoke" {
		proof["targetDeviceId"] = target
	}
	payload, e = json.Marshal(proof)
	if e != nil {
		t.Fatal(e)
	}
	return string(payload)
}

func TestDeviceAdministrationHTTPPrivilegeAndSafeListing(t *testing.T) {
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("device administration reached router") })
	owner, other := identity(t, g), identity(t, g)
	cookie := session(t, g, owner, 0)
	w := request(g, "GET", "/_wol/v1/devices/access", owner.AccessToken, "", "")
	var access map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &access) != nil || len(access) != 2 || access["deviceId"] != owner.DeviceID || access["deviceAdministration"] != false {
		t.Fatalf("own capability leaked devices or elevated caller: %d %s", w.Code, w.Body.String())
	}
	challenge := `{"origin":"` + testOrigin + `","action":"list","targetDeviceId":""}`
	w = request(g, "POST", "/_wol/v1/devices/challenge", owner.AccessToken, cookie, challenge)
	if w.Code != 403 {
		t.Fatalf("wake plus LuCI credentials authorized device administration: %d", w.Code)
	}
	for _, path := range []string{"/_wol/v1/devices/admin", "/_wol/v1/devices/access"} {
		w = request(g, "POST", path, owner.AccessToken, cookie, `{"enabled":true}`)
		if w.Code != 404 {
			t.Fatalf("public API exposes capability grant: %s %d", path, w.Code)
		}
	}
	if e := g.Auth.SetDeviceAdministration(owner.DeviceID, true); e != nil {
		t.Fatal(e)
	}
	for _, endpoint := range []struct{ method, path, body string }{
		{"GET", "/_wol/v1/devices/access", ""},
		{"POST", "/_wol/v1/devices/challenge", challenge},
		{"POST", "/_wol/v1/devices/list", `{}`},
		{"POST", "/_wol/v1/devices/revoke", `{}`},
	} {
		w = request(g, endpoint.method, endpoint.path, "", cookie, endpoint.body)
		if w.Code != 401 {
			t.Fatalf("browser session substituted for bearer: %s %d", endpoint.path, w.Code)
		}
	}
	// Unsigned requests still fail after the administrator capability is granted.
	w = request(g, "POST", "/_wol/v1/devices/list", owner.AccessToken, "", `{}`)
	if w.Code != 409 {
		t.Fatalf("bearer alone listed devices: %d", w.Code)
	}
	proof := deviceProof(t, g, owner, "list", "")
	w = request(g, "POST", "/_wol/v1/devices/list", owner.AccessToken, "", proof)
	var listing struct {
		Devices []map[string]any `json:"devices"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil || len(listing.Devices) != 2 {
		t.Fatalf("signed list failed: %d %s", w.Code, w.Body.String())
	}
	for _, row := range listing.Devices {
		if len(row) != 5 || row["deviceName"] != "test phone" || row["createdAt"] == nil || row["current"] != (row["deviceId"] == owner.DeviceID) || row["deviceAdministration"] != (row["deviceId"] == owner.DeviceID) {
			t.Fatalf("unsafe device response: %+v", row)
		}
		if row["deviceId"] != owner.DeviceID && row["deviceId"] != other.DeviceID {
			t.Fatalf("unexpected device: %+v", row)
		}
	}
	w = request(g, "POST", "/_wol/v1/devices/list", owner.AccessToken, "", proof)
	if w.Code != 409 {
		t.Fatalf("HTTP signature replay accepted: %d", w.Code)
	}
}

func TestDeviceAdministrationSelfRevocationReturnsReceipt(t *testing.T) {
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected router request") })
	owner := identity(t, g)
	if e := g.Auth.SetDeviceAdministration(owner.DeviceID, true); e != nil {
		t.Fatal(e)
	}
	proof := deviceProof(t, g, owner, "revoke", owner.DeviceID)
	w := request(g, "POST", "/_wol/v1/devices/revoke", owner.AccessToken, "", proof)
	var result protocol.DeviceRevocation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Current || !result.Revoked || result.DeviceID != owner.DeviceID {
		t.Fatalf("self cancellation swallowed receipt: %d %s", w.Code, w.Body.String())
	}
	w = request(g, "GET", "/_wol/v1/devices/access", owner.AccessToken, "", "")
	if w.Code != 401 {
		t.Fatalf("revoked token retained access: %d", w.Code)
	}
}

func TestDeviceAdministrationRevocationWhileReadingBody(t *testing.T) {
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected router request") })
	owner, revoker, target := identity(t, g), identity(t, g), identity(t, g)
	for _, id := range []string{owner.DeviceID, revoker.DeviceID} {
		if e := g.Auth.SetDeviceAdministration(id, true); e != nil {
			t.Fatal(e)
		}
	}
	pendingProof := deviceProof(t, g, owner, "revoke", target.DeviceID)
	proof := deviceProof(t, g, revoker, "revoke", owner.DeviceID)
	body := &gatedBody{Reader: strings.NewReader(pendingProof), started: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("POST", "/_wol/v1/devices/revoke", body)
	r.Host = "wol.hubu.wiki"
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Authorization", "Bearer "+owner.AccessToken)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); g.ServeHTTP(w, r) }()
	<-body.started
	revoked := request(g, "POST", "/_wol/v1/devices/revoke", revoker.AccessToken, "", proof)
	close(body.release)
	<-done
	if revoked.Code != 200 || w.Code != 401 {
		t.Fatalf("revoked request was admitted: revocation=%d pending=%d", revoked.Code, w.Code)
	}
	if _, e := g.Auth.Token(target.AccessToken); e != nil {
		t.Fatal("revoked request still revoked another device:", e)
	}
}

func TestLocalDeviceAdministrationRequiresExplicitCapabilityChoice(t *testing.T) {
	g := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected router request") })
	owner := identity(t, g)
	for _, body := range []string{`{"deviceId":"` + owner.DeviceID + `"}`, `{"deviceId":"` + owner.DeviceID + `","enabled":null}`} {
		w := httptest.NewRecorder()
		g.admin(w, httptest.NewRequest("POST", "/devices/admin", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("implicit capability choice accepted: %d", w.Code)
		}
	}
	for _, enabled := range []bool{true, false} {
		payload, e := json.Marshal(map[string]any{"deviceId": owner.DeviceID, "enabled": enabled})
		if e != nil {
			t.Fatal(e)
		}
		w := httptest.NewRecorder()
		g.admin(w, httptest.NewRequest("POST", "/devices/admin", strings.NewReader(string(payload))))
		if w.Code != 200 {
			t.Fatalf("local capability change failed: %d %s", w.Code, w.Body.String())
		}
		access, e := g.Auth.DeviceAdministrationAccess(owner.DeviceID)
		if e != nil || access.DeviceAdministration != enabled {
			t.Fatalf("local capability did not persist: %+v %v", access, e)
		}
	}
}

package gateway

import (
	"net/http"

	"hubu.wiki/roc-wol/internal/protocol"
)

func deviceAdministrationRoute(path, method string) bool {
	return path == "/_wol/v1/devices/access" && method == "GET" ||
		(path == "/_wol/v1/devices/challenge" || path == "/_wol/v1/devices/list" || path == "/_wol/v1/devices/revoke") && method == "POST"
}

// The shared API entrypoint has already admitted a revocation-aware bearer
// context. Browser cookies and LuCI authorization never reach this handler.
func (g *Gateway) deviceAdministration(w http.ResponseWriter, r *http.Request, device, origin string) {
	switch r.URL.Path {
	case "/_wol/v1/devices/access":
		result, e := g.Auth.DeviceAdministrationAccess(device)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
	case "/_wol/v1/devices/challenge":
		var v struct {
			Origin         string `json:"origin"`
			Action         string `json:"action"`
			TargetDeviceID string `json:"targetDeviceId"`
		}
		if !body(w, r, &v) {
			return
		}
		if v.Origin != origin {
			protocol.Fail(w, 403, "forbidden")
			return
		}
		result, e := g.Auth.DeviceAdministrationChallenge(device, origin, v.Action, v.TargetDeviceID)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
	case "/_wol/v1/devices/list":
		var v struct {
			ChallengeID string `json:"challengeId"`
			Signature   string `json:"signature"`
		}
		if !body(w, r, &v) {
			return
		}
		result, e := g.Auth.ListDevices(device, v.ChallengeID, v.Signature, origin)
		if e != nil {
			respondError(w, e)
			return
		}
		protocol.JSON(w, 200, result)
	case "/_wol/v1/devices/revoke":
		var v struct {
			ChallengeID    string `json:"challengeId"`
			Signature      string `json:"signature"`
			TargetDeviceID string `json:"targetDeviceId"`
		}
		if !body(w, r, &v) {
			return
		}
		result, e := g.Auth.RevokeDevice(device, v.ChallengeID, v.Signature, origin, v.TargetDeviceID)
		if e != nil {
			respondError(w, e)
			return
		}
		// Do not re-check the request context: self-revocation cancels it.
		protocol.JSON(w, 200, result)
	}
}

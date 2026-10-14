package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
	"hubu.wiki/roc-wol/internal/store"
)

func (g *Gateway) AdminServer() (*http.Server, net.Listener, error) {
	dir := filepath.Dir(g.cfg.AdminSocket)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, nil, e
	}
	info, e := os.Lstat(dir)
	if e != nil {
		return nil, nil, e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("admin directory must not be a symlink")
	}
	if e = os.Chmod(dir, 0700); e != nil {
		return nil, nil, e
	}
	if _, e = os.Lstat(g.cfg.AdminSocket); e == nil {
		return nil, nil, errors.New("admin socket already exists; refuse to replace it")
	}
	listener, e := net.Listen("unix", g.cfg.AdminSocket)
	if e != nil {
		return nil, nil, e
	}
	if e = os.Chmod(g.cfg.AdminSocket, 0600); e != nil {
		listener.Close()
		return nil, nil, e
	}
	server := &http.Server{Handler: http.HandlerFunc(g.admin), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: protocol.JSONLimit}
	return server, listener, nil
}
func (g *Gateway) admin(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && r.URL.Path == "/pair-code":
		var v struct {
			TTL string `json:"ttl"`
		}
		if !body(w, r, &v) {
			return
		}
		ttl, e := time.ParseDuration(v.TTL)
		if e != nil {
			protocol.Fail(w, 400, "invalid_request")
			return
		}
		code, e := g.Auth.PairCode(ttl)
		if e != nil {
			protocol.Fail(w, 400, "invalid_request")
			return
		}
		protocol.JSON(w, 200, map[string]string{"code": code})
	case r.Method == "GET" && r.URL.Path == "/devices":
		devices, e := g.DB.Devices()
		if e != nil {
			respondError(w, e)
			return
		}
		out := make([]map[string]any, 0, len(devices))
		for _, d := range devices {
			wakeDER, wakeErr := protocol.Decode(d.WakeKey)
			manageDER, manageErr := protocol.Decode(d.ManageKey)
			if wakeErr != nil || manageErr != nil {
				protocol.Fail(w, 503, "upstream_unavailable")
				return
			}
			wakeHash, manageHash := sha256.Sum256(wakeDER), sha256.Sum256(manageDER)
			out = append(out, map[string]any{"deviceId": d.DeviceID, "deviceName": d.DeviceName, "createdAt": d.CreatedAt, "revoked": d.Revoked, "deviceAdministration": d.DeviceAdministration, "wakeKeySHA256": hex.EncodeToString(wakeHash[:]), "manageKeySHA256": hex.EncodeToString(manageHash[:])})
		}
		protocol.JSON(w, 200, out)
	case r.Method == "POST" && r.URL.Path == "/devices/revoke":
		var v struct {
			DeviceID string `json:"deviceId"`
		}
		if !body(w, r, &v) {
			return
		}
		if !protocol.ValidID(v.DeviceID) {
			protocol.Fail(w, 400, "invalid_request")
			return
		}
		if e := g.Auth.Revoke(v.DeviceID); e != nil {
			if errors.Is(e, store.ErrUnavailable) {
				protocol.Fail(w, 404, "not_found")
			} else {
				respondError(w, e)
			}
			return
		}
		protocol.JSON(w, 200, map[string]bool{"revoked": true})
	case r.Method == "POST" && r.URL.Path == "/devices/admin":
		var v struct {
			DeviceID string `json:"deviceId"`
			Enabled  *bool  `json:"enabled"`
		}
		if !body(w, r, &v) {
			return
		}
		if !protocol.ValidID(v.DeviceID) || v.Enabled == nil {
			protocol.Fail(w, 400, "invalid_request")
			return
		}
		if e := g.Auth.SetDeviceAdministration(v.DeviceID, *v.Enabled); e != nil {
			if errors.Is(e, store.ErrUnavailable) {
				protocol.Fail(w, 404, "not_found")
			} else {
				respondError(w, e)
			}
			return
		}
		protocol.JSON(w, 200, protocol.DeviceAdministrationAccess{DeviceID: v.DeviceID, DeviceAdministration: *v.Enabled})
	default:
		protocol.Fail(w, 404, "not_found")
	}
}

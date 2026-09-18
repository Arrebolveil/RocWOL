package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
	"hubu.wiki/roc-wol/internal/store"
)

func (g *Gateway) agentRequest(r *http.Request, method, path string, payload any) (*http.Response, error) {
	var b bytes.Buffer
	if payload != nil {
		if e := json.NewEncoder(&b).Encode(payload); e != nil {
			return nil, e
		}
	}
	req, e := http.NewRequestWithContext(r.Context(), method, g.cfg.AgentURL+path, &b)
	if e != nil {
		return nil, e
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return g.client.Do(req)
}
func (g *Gateway) target(w http.ResponseWriter, r *http.Request, device string) {
	status := protocol.Status{TargetID: "desktop", Reachability: "unknown"}
	router := "unavailable"
	resp, e := g.agentRequest(r, "GET", "/_roc-agent/v1/status", nil)
	if e == nil {
		defer drainClose(resp.Body)
		var received protocol.Status
		if resp.StatusCode == 200 && protocol.DecodeJSON(resp.Body, &received, protocol.JSONLimit) == nil && received.TargetID == "desktop" && (received.Reachability == "reachable" || received.Reachability == "unreachable" || received.Reachability == "unknown") && received.CheckedAt != nil {
			status = received
			router = "available"
		}
	}
	last, e := g.DB.LastWake(device)
	if e != nil {
		protocol.Fail(w, 503, "upstream_unavailable")
		return
	}
	protocol.JSON(w, 200, map[string]any{"targetId": "desktop", "name": "DESKTOP-IENBTJG", "mac": "B0:25:AA:83:C6:EF", "ip": "192.168.2.100", "router": router, "reachability": status.Reachability, "checkedAt": status.CheckedAt, "lastWake": last})
}
func (g *Gateway) wake(w http.ResponseWriter, r *http.Request, device, id string, post bool) {
	g.wakeMu.Lock()
	record, e := g.DB.Wake(id)
	fresh := false
	if e == nil && record.DeviceID != device {
		g.wakeMu.Unlock()
		protocol.Fail(w, 404, "not_found")
		return
	}
	if e != nil && !errors.Is(e, store.ErrUnavailable) {
		g.wakeMu.Unlock()
		protocol.Fail(w, 503, "upstream_unavailable")
		return
	}
	if errors.Is(e, store.ErrUnavailable) {
		if !post {
			g.wakeMu.Unlock()
			protocol.Fail(w, 404, "not_found")
			return
		}
		now := time.Now().UTC()
		for key, times := range g.wakeRates {
			if len(times) == 0 || now.Sub(times[len(times)-1]) >= time.Minute {
				delete(g.wakeRates, key)
			}
		}
		times := g.wakeRates[device]
		start := 0
		for start < len(times) && now.Sub(times[start]) >= time.Minute {
			start++
		}
		times = times[start:]
		if len(times) >= 6 {
			g.wakeMu.Unlock()
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(time.Until(times[0].Add(time.Minute)).Seconds())+1)))
			protocol.Fail(w, 429, "rate_limited")
			return
		}
		record = store.WakeRecord{Wake: protocol.Wake{RequestID: id, Result: "pending"}, DeviceID: device, CreatedAt: now}
		if e = g.DB.CreateWake(record); e != nil {
			g.wakeMu.Unlock()
			protocol.Fail(w, 503, "upstream_unavailable")
			return
		}
		g.wakeRates[device] = append(times, now)
		fresh = true
	}
	g.wakeMu.Unlock()
	if fresh || record.Result == "pending" || record.Result == "unknown" {
		record = g.reconcile(r, record, fresh)
	}
	status := 200
	switch record.Result {
	case "sent":
		if post {
			status = 202
		}
	case "failed":
		status = 502
		if record.Code == "rate_limited" {
			status = 429
			w.Header().Set("Retry-After", "10")
		}
	case "unknown":
		status = 504
		record.Code = "outcome_unknown"
	case "pending":
		status = 202
	}
	protocol.JSON(w, status, record.Wake)
}
func (g *Gateway) reconcile(r *http.Request, record store.WakeRecord, dispatch bool) store.WakeRecord {
	method, path := "GET", "/_roc-agent/v1/wake/"+record.RequestID
	var payload any
	if dispatch {
		method = "POST"
		path = "/_roc-agent/v1/wake"
		payload = map[string]string{"requestId": record.RequestID, "targetId": "desktop"}
	}
	result := protocol.Wake{RequestID: record.RequestID, Result: "unknown", Code: "outcome_unknown"}
	resp, e := g.agentRequest(r, method, path, payload)
	if e == nil {
		defer drainClose(resp.Body)
		var received protocol.Wake
		if (resp.StatusCode == 200 || resp.StatusCode == 202 || resp.StatusCode == 502 || resp.StatusCode == 504) && protocol.DecodeJSON(resp.Body, &received, protocol.JSONLimit) == nil && received.RequestID == record.RequestID && (received.Result == "sent" || received.Result == "failed" || received.Result == "unknown" || received.Result == "pending") {
			result = received
			switch result.Result {
			case "sent":
				if result.SentAt == nil {
					result = protocol.Wake{RequestID: record.RequestID, Result: "unknown", Code: "outcome_unknown"}
				} else {
					result.Code = ""
				}
			case "failed":
				result.Code = "wake_failed"
			case "unknown":
				result.Code = "outcome_unknown"
			default:
				result.Code = ""
			}
		} else if dispatch && resp.StatusCode == 429 {
			result = protocol.Wake{RequestID: record.RequestID, Result: "failed", Code: "rate_limited"}
		}
	}
	// Concurrent queries may complete out of order; never overwrite a terminal
	// result with an earlier observation of pending/unknown.
	g.wakeMu.Lock()
	defer g.wakeMu.Unlock()
	current, e := g.DB.Wake(record.RequestID)
	if e != nil {
		record.Wake = protocol.Wake{RequestID: record.RequestID, Result: "unknown", Code: "outcome_unknown"}
		return record
	}
	if current.Result == "sent" || current.Result == "failed" {
		return current
	}
	if !dispatch && result.Result == "pending" {
		result.Result = "unknown"
		result.Code = "outcome_unknown"
	}
	current.Wake = result
	if e = g.DB.UpdateWake(current); e != nil {
		current.Wake = protocol.Wake{RequestID: record.RequestID, Result: "unknown", Code: "outcome_unknown"}
	}
	return current
}

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

// readTargets trusts only a validated response from the authenticated agent.
// A successful read replaces, rather than merges, the bounded snapshot.
func (g *Gateway) readTargets(r *http.Request) (protocol.TargetList, error) {
	resp, err := g.agentRequest(r, "GET", "/_roc-agent/v2/targets", nil)
	if err != nil {
		return protocol.TargetList{}, err
	}
	defer drainClose(resp.Body)
	var wire struct {
		Targets           []protocol.TargetInfo `json:"targets"`
		ManagementEnabled *bool                 `json:"managementEnabled"`
	}
	if resp.StatusCode != 200 || protocol.DecodeJSON(resp.Body, &wire, protocol.ResponseJSONLimit) != nil || wire.ManagementEnabled == nil {
		return protocol.TargetList{}, errors.New("invalid agent target list")
	}
	list := protocol.TargetList{Targets: wire.Targets, ManagementEnabled: *wire.ManagementEnabled}
	if !protocol.ValidTargetList(list) {
		return protocol.TargetList{}, errors.New("invalid agent target list")
	}
	snapshot := make(map[string]protocol.TargetInfo, len(list.Targets))
	for _, target := range list.Targets {
		snapshot[target.TargetID] = target
	}
	g.targetMu.Lock()
	g.targetSnapshot = snapshot
	g.targetMu.Unlock()
	list.ManagementEnabled = list.ManagementEnabled && (g.cfg.ManagementEnabled == nil || *g.cfg.ManagementEnabled)
	return list, nil
}
func (g *Gateway) managementAvailable(r *http.Request) (bool, error) {
	if g.cfg.ManagementEnabled != nil && !*g.cfg.ManagementEnabled {
		return false, nil
	}
	list, err := g.readTargets(r)
	return list.ManagementEnabled, err
}
func (g *Gateway) requireManagement(w http.ResponseWriter, r *http.Request) bool {
	enabled, err := g.managementAvailable(r)
	if err != nil {
		protocol.Fail(w, 503, "agent_unavailable")
		return false
	}
	if !enabled {
		protocol.Fail(w, 403, "management_disabled")
		return false
	}
	return true
}
func (g *Gateway) targets(w http.ResponseWriter, r *http.Request) {
	list, err := g.readTargets(r)
	if err != nil {
		protocol.Fail(w, 503, "agent_unavailable")
		return
	}
	protocol.JSON(w, 200, list)
}
func (g *Gateway) target(w http.ResponseWriter, r *http.Request, device, id string) {
	if !protocol.ValidTargetID(id) {
		protocol.Fail(w, 400, "invalid_request")
		return
	}
	fresh := r.URL.RawQuery == "fresh=1"
	if fresh && !g.admitFresh(device) {
		w.Header().Set("Retry-After", "1")
		protocol.Fail(w, 429, "rate_limited")
		return
	}
	_, listErr := g.readTargets(r)
	g.targetMu.RLock()
	target, exists := g.targetSnapshot[id]
	g.targetMu.RUnlock()
	if !exists {
		if listErr != nil {
			protocol.Fail(w, 503, "agent_unavailable")
		} else {
			protocol.Fail(w, 404, "target_not_found")
		}
		return
	}
	status := protocol.Status{TargetID: id, Reachability: "unknown"}
	router := "unavailable"
	if listErr == nil {
		path := "/_roc-agent/v2/targets/" + id + "/status"
		if fresh {
			path += "?fresh=1"
		}
		resp, err := g.agentRequest(r, "GET", path, nil)
		if err == nil {
			defer drainClose(resp.Body)
			var received protocol.Status
			if resp.StatusCode == 404 {
				protocol.Fail(w, 404, "target_not_found")
				return
			}
			if resp.StatusCode == 200 && protocol.DecodeJSON(resp.Body, &received, protocol.ResponseJSONLimit) == nil && received.TargetID == id && (received.Reachability == "reachable" || received.Reachability == "unreachable" || received.Reachability == "unknown") && received.CheckedAt != nil && !received.CheckedAt.IsZero() {
				status, router = received, "available"
			}
		}
	}
	last, err := g.DB.LastWake(device, id)
	if err != nil {
		protocol.Fail(w, 503, "upstream_unavailable")
		return
	}
	protocol.JSON(w, 200, map[string]any{"targetId": id, "name": target.Name, "mac": target.MAC, "ip": target.IP, "router": router, "reachability": status.Reachability, "checkedAt": status.CheckedAt, "lastWake": last})
}
func (g *Gateway) admitFresh(device string) bool {
	g.freshMu.Lock()
	defer g.freshMu.Unlock()
	now := time.Now()
	for key, last := range g.freshRates {
		if now.Sub(last) >= time.Second {
			delete(g.freshRates, key)
		}
	}
	if _, limited := g.freshRates[device]; limited || len(g.freshRates) >= 1024 {
		return false
	}
	if g.freshRates == nil {
		g.freshRates = make(map[string]time.Time)
	}
	g.freshRates[device] = now
	return true
}
func (g *Gateway) wake(w http.ResponseWriter, r *http.Request, device, id, targetID string, post bool) {
	g.wakeMu.Lock()
	record, e := g.DB.Wake(id)
	fresh := false
	if e == nil && record.DeviceID != device {
		g.wakeMu.Unlock()
		protocol.Fail(w, 404, "not_found")
		return
	}
	if e == nil && post && record.TargetID != targetID {
		g.wakeMu.Unlock()
		protocol.Fail(w, 409, "request_target_mismatch")
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
		// Existing bindings above are checked before consulting today's list.
		// Only new requests require a current authenticated whitelist.
		list, listErr := g.readTargets(r)
		if listErr != nil {
			g.wakeMu.Unlock()
			protocol.Fail(w, 503, "agent_unavailable")
			return
		}
		found := false
		for _, target := range list.Targets {
			if target.TargetID == targetID {
				found = true
				break
			}
		}
		if !found {
			g.wakeMu.Unlock()
			protocol.Fail(w, 404, "target_not_found")
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
		record = store.WakeRecord{Wake: protocol.Wake{TargetID: targetID, RequestID: id, Result: "pending"}, DeviceID: device, CreatedAt: now}
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
		if record.Code == "target_not_found" {
			status = 404
		}
		if record.Code == "request_target_mismatch" {
			status = 409
		}
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
	method, path := "GET", "/_roc-agent/v2/wake/"+record.RequestID
	var payload any
	if dispatch {
		method = "POST"
		path = "/_roc-agent/v2/wake"
		payload = map[string]string{"requestId": record.RequestID, "targetId": record.TargetID}
	}
	unknown := protocol.Wake{TargetID: record.TargetID, RequestID: record.RequestID, Result: "unknown", Code: "outcome_unknown"}
	result := unknown
	resp, e := g.agentRequest(r, method, path, payload)
	if e == nil {
		defer drainClose(resp.Body)
		var received protocol.Wake
		decoded := protocol.DecodeJSON(resp.Body, &received, protocol.ResponseJSONLimit) == nil && received.RequestID == record.RequestID && received.TargetID == record.TargetID
		// Only a strictly matching authenticated rejection proves no execution.
		if decoded && received.Result == "failed" && received.SentAt == nil && ((resp.StatusCode == 404 && received.Code == "target_not_found") || (resp.StatusCode == 409 && received.Code == "request_target_mismatch")) {
			result = received
		} else if decoded && (resp.StatusCode == 200 || resp.StatusCode == 202 || resp.StatusCode == 502 || resp.StatusCode == 504) {
			switch received.Result {
			case "sent":
				if received.SentAt != nil && !received.SentAt.IsZero() {
					received.Code = ""
					result = received
				}
			case "failed":
				received.SentAt, received.Code = nil, "wake_failed"
				result = received
			case "pending":
				received.SentAt, received.Code = nil, ""
				result = received
			case "unknown":
				result = unknown
			}
		} else if dispatch && resp.StatusCode == 429 {
			result = protocol.Wake{TargetID: record.TargetID, RequestID: record.RequestID, Result: "failed", Code: "rate_limited"}
		}
	}
	// Concurrent queries may complete out of order; never overwrite a terminal
	// result with an earlier observation of pending/unknown.
	g.wakeMu.Lock()
	defer g.wakeMu.Unlock()
	current, e := g.DB.Wake(record.RequestID)
	if e != nil {
		record.Wake = unknown
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
		current.Wake = unknown
	}
	return current
}

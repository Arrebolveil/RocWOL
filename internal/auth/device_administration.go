package auth

import (
	"time"

	"hubu.wiki/roc-wol/internal/protocol"
	"hubu.wiki/roc-wol/internal/store"
)

// SetDeviceAdministration is exposed only through the filesystem-protected
// local admin socket. Removing this capability does not revoke wake or LuCI access.
func (s *State) SetDeviceAdministration(device string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.db.SetDeviceAdministration(device, enabled); e != nil {
		return e
	}
	if !enabled {
		for id, c := range s.challenges {
			if c.Kind == "device-admin" && c.Device == device {
				delete(s.challenges, id)
			}
		}
	}
	return nil
}

// DeviceAdministrationAccess reveals only the authenticated caller's capability.
func (s *State) DeviceAdministrationAccess(device string) (protocol.DeviceAdministrationAccess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, e := s.validDevice(device)
	if e != nil {
		return protocol.DeviceAdministrationAccess{}, e
	}
	return protocol.DeviceAdministrationAccess{DeviceID: d.DeviceID, DeviceAdministration: d.DeviceAdministration}, nil
}

func (s *State) deviceAdministrator(device string) (store.Device, error) {
	d, e := s.validDevice(device)
	if e != nil {
		return d, e
	}
	if !d.DeviceAdministration {
		return d, fail(403, "device_administration_denied")
	}
	return d, nil
}

func (s *State) DeviceAdministrationChallenge(device, origin, action, target string) (protocol.Challenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, e := s.deviceAdministrator(device); e != nil {
		return protocol.Challenge{}, e
	}
	if !(action == "list" && target == "" || action == "revoke" && protocol.ValidID(target)) {
		return protocol.Challenge{}, fail(400, "invalid_request")
	}
	if e := s.challengeLimit(device); e != nil {
		return protocol.Challenge{}, e
	}
	// Target existence is not disclosed until a fresh signature is verified.
	return s.newChallenge(challenge{Kind: "device-admin", Device: device, Origin: origin, Action: action, Target: target}, time.Minute), nil
}

// verifyDeviceAdministration requires mu. Admission limits precede consumption;
// a rejected signature (including binding mismatch) consumes the challenge.
func (s *State) verifyDeviceAdministration(device, id, signature, origin, action, target string) error {
	s.clean()
	d, e := s.deviceAdministrator(device)
	if e != nil {
		return e
	}
	c, e := s.lookupChallenge(id, "device-admin", device)
	if e != nil {
		return e
	}
	if !s.rate("device:"+device, 10) || !s.rate("global", 60) {
		return fail(429, "rate_limited")
	}
	delete(s.challenges, id)
	if c.Origin != origin || c.Action != action || c.Target != target ||
		!protocol.Verify(d.ManageKey, signature, protocol.DeviceAdministrationMessage(device, id, c.Nonce, c.Origin, c.Action, c.Target)) {
		return fail(403, "forbidden")
	}
	return nil
}

func (s *State) ListDevices(device, id, signature, origin string) (protocol.DeviceList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.verifyDeviceAdministration(device, id, signature, origin, "list", ""); e != nil {
		return protocol.DeviceList{}, e
	}
	devices, e := s.db.Devices()
	if e != nil {
		return protocol.DeviceList{}, e
	}
	out := protocol.DeviceList{Devices: make([]protocol.AdministeredDevice, 0, 5)}
	for _, d := range devices {
		if !d.Revoked {
			out.Devices = append(out.Devices, protocol.AdministeredDevice{
				DeviceID: d.DeviceID, DeviceName: d.DeviceName, CreatedAt: d.CreatedAt,
				Current: d.DeviceID == device, DeviceAdministration: d.DeviceAdministration,
			})
		}
	}
	return out, nil
}

func (s *State) RevokeDevice(device, id, signature, origin, target string) (protocol.DeviceRevocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.verifyDeviceAdministration(device, id, signature, origin, "revoke", target); e != nil {
		return protocol.DeviceRevocation{}, e
	}
	d, e := s.db.Device(target)
	if e == store.ErrUnavailable || e == nil && d.Revoked {
		return protocol.DeviceRevocation{}, fail(404, "not_found")
	}
	if e != nil {
		return protocol.DeviceRevocation{}, e
	}
	// Self-revocation is intentional: cancel every admitted request, including
	// this request's bearer context, but still return its successful receipt.
	if e = s.revokeLocked(target); e != nil {
		return protocol.DeviceRevocation{}, e
	}
	return protocol.DeviceRevocation{DeviceID: target, Revoked: true, Current: target == device}, nil
}

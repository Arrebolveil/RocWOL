package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeOrigin(t *testing.T) {
	valid := map[string]string{
		"https://WOL.EXAMPLE/":        "https://wol.example",
		"HTTPS://wol.example:0443":    "https://wol.example",
		"https://wol.example:8443/":   "https://wol.example:8443",
		"https://192.0.2.1":           "https://192.0.2.1",
		"https://[2001:DB8::1]:8443/": "https://[2001:db8::1]:8443",
	}
	for input, expected := range valid {
		actual, err := NormalizeOrigin(input)
		if err != nil || actual != expected {
			t.Errorf("normalize %q: %q %v", input, actual, err)
		}
	}
	for _, input := range []string{"", "http://wol.example", "https://user@wol.example", "https://wol.example/path", "https://wol.example?", "https://wol.example#", "https://wol.example:", "https://wol.example:0", "https://wol.example:65536", "https://wol.example.", "https://-wol.example", "https://wol..example", "https://wöl.example", "https://wol%2eexample", "https://[127.0.0.1]", "https://2001:db8::1", "https://wol.example\\evil", " https://wol.example", "https://wol.example\n", "https://wol.example\u0085", "https://" + strings.Repeat("x", 64) + ".example", "https://" + strings.Repeat("a", 513)} {
		if actual, err := NormalizeOrigin(input); err == nil {
			t.Errorf("invalid origin accepted %q as %q", input, actual)
		}
	}
}

func TestTargetContractAndMaximumResponse(t *testing.T) {
	for _, id := range []string{"pc-a", "0", "a_b", strings.Repeat("a", 64)} {
		if !ValidTargetID(id) {
			t.Errorf("valid target rejected: %s", id)
		}
	}
	for _, id := range []string{"", "-pc", "_pc", "Pc", "a.b", strings.Repeat("a", 65), "电脑"} {
		if ValidTargetID(id) {
			t.Errorf("invalid target accepted: %s", id)
		}
	}
	list := TargetList{Targets: make([]TargetInfo, 64), ManagementEnabled: true}
	for i := range list.Targets {
		list.Targets[i] = TargetInfo{TargetID: fmt.Sprintf("pc-%d", i), Name: strings.Repeat("电", 64), MAC: "02:00:00:00:00:01", IP: "192.0.2.10"}
	}
	if !ValidTargetList(list) {
		t.Fatal("maximum list rejected")
	}
	data, err := json.Marshal(list)
	if err != nil || len(data) > ResponseJSONLimit {
		t.Fatalf("maximum response size: %d %v", len(data), err)
	}
	var decoded TargetList
	if err := DecodeJSON(bytes.NewReader(data), &decoded, ResponseJSONLimit); err != nil || !ValidTargetList(decoded) {
		t.Fatalf("maximum response decode: %v", err)
	}
	list.Targets[1].TargetID = list.Targets[0].TargetID
	if ValidTargetList(list) {
		t.Fatal("duplicate target accepted")
	}
	for _, mac := range []string{"00:00:00:00:00:00", "01:00:00:00:00:01", "02:00:00:00:00:00:00:01", "invalid"} {
		if ValidTargetInfo(TargetInfo{TargetID: "pc", Name: "PC", MAC: mac, IP: "192.0.2.1"}) {
			t.Errorf("bad MAC accepted: %s", mac)
		}
	}
	for _, ip := range []string{"0.0.0.0", "127.0.0.1", "224.0.0.1", "255.255.255.255", "::1", "::ffff:192.0.2.1"} {
		if ValidTargetInfo(TargetInfo{TargetID: "pc", Name: "PC", MAC: "02:00:00:00:00:01", IP: ip}) {
			t.Errorf("bad IP accepted: %s", ip)
		}
	}
	for _, name := range []string{"", strings.Repeat("电", 65), "PC\n", "PC\u0085", string([]byte{0xff})} {
		if ValidName(name) {
			t.Errorf("invalid name accepted: %q", name)
		}
	}
}

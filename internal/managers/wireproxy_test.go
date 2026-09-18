package managers

import (
	"strings"
	"testing"
)

const testBindAddr = "127.0.0.1:1081"

const validWG = `[Interface]
PrivateKey = aGVsbG8td29ybGQtcHJpdmF0ZS1rZXktMzJieXRlcw==
Address = 10.64.0.2/32, fc00:bbbb::2/128
DNS = 10.64.0.1

[Peer]
PublicKey = c2VydmVyLXB1YmxpYy1rZXktMzJieXRlcy1oZWxsbw==
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = 193.32.1.1:51820
`

func TestNormalizeWireguardConf_OK(t *testing.T) {
	out, err := normalizeWireguardConf(validWG, testBindAddr)
	if err != nil {
		t.Fatalf("valid conf rejected: %v", err)
	}
	if !strings.Contains(out, "[Interface]") || !strings.Contains(out, "Endpoint = 193.32.1.1:51820") {
		t.Fatalf("interface/peer not preserved:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "BindAddress = "+testBindAddr) {
		t.Fatalf("our [Socks5] not appended last:\n%s", out)
	}
	if strings.Count(out, "[Socks5]") != 1 {
		t.Fatalf("want exactly one [Socks5], got %d", strings.Count(out, "[Socks5]"))
	}
}

func TestNormalizeWireguardConf_ForcesMTU(t *testing.T) {
	out, err := normalizeWireguardConf(validWG, testBindAddr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "MTU = 1280") {
		t.Fatalf("MTU not injected:\n%s", out)
	}
	// must land inside [Interface], before [Peer]
	if strings.Index(out, "MTU = 1280") > strings.Index(out, "[Peer]") {
		t.Fatalf("MTU landed outside [Interface]:\n%s", out)
	}

	// an explicit MTU is left alone
	withMTU := strings.Replace(validWG, "DNS = 10.64.0.1", "DNS = 10.64.0.1\nMTU = 1412", 1)
	out2, err := normalizeWireguardConf(withMTU, testBindAddr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "1280") || !strings.Contains(out2, "MTU = 1412") {
		t.Fatalf("explicit MTU not preserved:\n%s", out2)
	}
}

func TestNormalizeWireguardConf_StripsUploadedProxySections(t *testing.T) {
	in := validWG + "\n[Socks5]\nBindAddress = 127.0.0.1:9999\n\n[http]\nBindAddress = 127.0.0.1:8888\n"
	out, err := normalizeWireguardConf(in, testBindAddr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "9999") || strings.Contains(out, "[http]") {
		t.Fatalf("uploaded proxy sections not stripped:\n%s", out)
	}
	if strings.Count(out, "[Socks5]") != 1 || !strings.Contains(out, testBindAddr) {
		t.Fatalf("our [Socks5] not authoritative:\n%s", out)
	}
}

func TestAssignWireproxyPort(t *testing.T) {
	// fresh: first port in the window
	if p, err := assignWireproxyPort(nil, "mullvad"); err != nil || p != wireproxyPortBase {
		t.Fatalf("fresh assign = %d,%v; want %d", p, err, wireproxyPortBase)
	}
	list := []EgressProfile{
		{Name: "warp", Kind: "warp"},
		{Name: "mullvad", Kind: "wireproxy", Port: wireproxyPortBase},
		{Name: "proton", Kind: "wireproxy", Port: wireproxyPortBase + 1},
	}
	// existing profile keeps its port
	if p, _ := assignWireproxyPort(list, "proton"); p != wireproxyPortBase+1 {
		t.Fatalf("existing proton got %d, want %d", p, wireproxyPortBase+1)
	}
	// new profile gets the next free one, skipping taken ports
	if p, _ := assignWireproxyPort(list, "hetzner"); p != wireproxyPortBase+2 {
		t.Fatalf("new hetzner got %d, want %d", p, wireproxyPortBase+2)
	}
	// window exhausted
	full := make([]EgressProfile, 0)
	for i := wireproxyPortBase; i <= wireproxyPortMax; i++ {
		full = append(full, EgressProfile{Name: "wg" + string(rune(i)), Kind: "wireproxy", Port: i})
	}
	if _, err := assignWireproxyPort(full, "onemore"); err == nil {
		t.Fatal("exhausted window should error")
	}
}

func TestSanitizeEgressName(t *testing.T) {
	cases := map[string]string{
		"Mullvad":       "mullvad",
		"  proton vpn ": "protonvpn",
		"he/tz..ner":    "hetzner",
		"a_b-1":         "a_b-1",
	}
	for in, want := range cases {
		if got := sanitizeEgressName(in); got != want {
			t.Errorf("sanitizeEgressName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeWireguardConf_Rejects(t *testing.T) {
	cases := map[string]string{
		"no interface":  "[Peer]\nPublicKey = x\nEndpoint = h:1\n",
		"no privatekey": "[Interface]\nAddress = 10.0.0.2/32\n[Peer]\nPublicKey = x\nEndpoint = h:1\n",
		"no endpoint":   "[Interface]\nPrivateKey = x\n[Peer]\nPublicKey = x\n",
		"empty":         "",
	}
	for name, conf := range cases {
		if _, err := normalizeWireguardConf(conf, testBindAddr); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

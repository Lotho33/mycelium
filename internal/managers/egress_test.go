package managers

import (
	"path/filepath"
	"testing"
)

// withTempSettings points the Settings manager at a throwaway config file and
// an empty in-memory map for the duration of one test.
func withTempSettings(t *testing.T) {
	t.Helper()
	oldPath, oldData := configPath, Settings.data
	configPath = filepath.Join(t.TempDir(), "config.json")
	Settings.data = map[string]any{}
	t.Cleanup(func() { configPath, Settings.data = oldPath, oldData })
}

func TestEgressRegistry(t *testing.T) {
	withTempSettings(t)

	if got := EgressProfiles(); len(got) != 1 || got[0].Name != EgressDirect {
		t.Fatalf("fresh registry: want [direct], got %+v", got)
	}
	// direct / empty / unknown → go direct, ok=true
	for _, name := range []string{"direct", "", "nope"} {
		if u, ok := ResolveEgressProxy(name); u != "" || !ok {
			t.Fatalf("ResolveEgressProxy(%q) = %q,%v; want \"\",true", name, u, ok)
		}
	}

	if err := UpsertEgressProfile(EgressProfile{Name: "Hetzner", ProxyURL: "1.2.3.4:1080", Enabled: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got := EgressProfiles()
	if len(got) != 2 || got[0].Name != EgressDirect || got[1].Name != "hetzner" {
		t.Fatalf("after add: %+v", got)
	}
	if got[1].ProxyURL != "socks5://1.2.3.4:1080" {
		t.Errorf("scheme not defaulted: %q", got[1].ProxyURL)
	}
	if u, ok := ResolveEgressProxy("hetzner"); u != "socks5://1.2.3.4:1080" || !ok {
		t.Fatalf("resolve enabled: %q,%v", u, ok)
	}

	// disabled → fail-closed ("",false), NOT a silent direct fallback
	if err := SetEgressEnabled("hetzner", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if u, ok := ResolveEgressProxy("hetzner"); u != "" || ok {
		t.Fatalf("resolve disabled: want \"\",false; got %q,%v", u, ok)
	}

	// validation
	if err := UpsertEgressProfile(EgressProfile{Name: "direct", ProxyURL: "x:1"}); err == nil {
		t.Error("upsert 'direct' should be rejected")
	}
	if err := UpsertEgressProfile(EgressProfile{Name: "x"}); err == nil {
		t.Error("upsert without proxy_url should be rejected")
	}
	if err := DeleteEgressProfile("direct"); err == nil {
		t.Error("deleting 'direct' should be rejected")
	}

	if err := DeleteEgressProfile("hetzner"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := EgressProfiles(); len(got) != 1 {
		t.Fatalf("after delete: %+v", got)
	}
}

func TestInitEgressDefaults(t *testing.T) {
	withTempSettings(t)

	InitEgressDefaults("socks5://127.0.0.1:1080")
	got := EgressProfiles()
	if len(got) != 2 || got[1].Name != "warp" || got[1].ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("seed: %+v", got)
	}
	// second call is a no-op even with a different URL
	InitEgressDefaults("socks5://other:1080")
	if p := EgressProfiles(); len(p) != 2 || p[1].ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("second call must not change the seeded profile: %+v", p)
	}

	// empty url and no persisted vpn_proxy_url → nothing seeded
	withTempSettings(t)
	InitEgressDefaults("")
	if p := EgressProfiles(); len(p) != 1 {
		t.Fatalf("empty seed should add nothing: %+v", p)
	}
}

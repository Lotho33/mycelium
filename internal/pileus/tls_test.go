package pileus

import (
	"crypto/x509"
	"testing"
)

func fakeSettingsStore() (get func(key, def string) string, save func(map[string]any) error, store map[string]string) {
	store = map[string]string{}
	get = func(key, def string) string {
		if v, ok := store[key]; ok {
			return v
		}
		return def
	}
	save = func(kv map[string]any) error {
		for k, v := range kv {
			store[k] = v.(string)
		}
		return nil
	}
	return get, save, store
}

func TestCurrentCertPEM_EmptyWhenNeverGenerated(t *testing.T) {
	get := func(key, def string) string { return def }
	if got := CurrentCertPEM(get); got != "" {
		t.Fatalf("want empty string when no cert has been persisted, got %q", got)
	}
}

func TestCurrentCertPEM_ReturnsPersistedValueUnchanged(t *testing.T) {
	const fakePEM = "-----BEGIN CERTIFICATE-----\nMII...\n-----END CERTIFICATE-----\n"
	get := func(key, def string) string {
		if key == "pileus_grpc_tls_cert" {
			return fakePEM
		}
		return def
	}
	if got := CurrentCertPEM(get); got != fakePEM {
		t.Fatalf("want the exact persisted PEM back, got %q", got)
	}
}

// TestGenerateOrLoadTLSCert_RoundTripsThroughCurrentCertPEM is the real
// regression test: CurrentCertPEM must read the exact same setting key
// GenerateOrLoadTLSCert writes, so a cert generated once is downloadable via
// GET /cert without ever being regenerated (which would change its
// fingerprint and break already-paired native clients' TOFU pinning).
func TestGenerateOrLoadTLSCert_RoundTripsThroughCurrentCertPEM(t *testing.T) {
	store := map[string]string{}
	get := func(key, def string) string {
		if v, ok := store[key]; ok {
			return v
		}
		return def
	}
	save := func(kv map[string]any) error {
		for k, v := range kv {
			store[k] = v.(string)
		}
		return nil
	}

	cert, err := GenerateOrLoadTLSCert(get, save)
	if err != nil {
		t.Fatalf("GenerateOrLoadTLSCert: %v", err)
	}
	pem := CurrentCertPEM(get)
	if pem == "" {
		t.Fatal("CurrentCertPEM returned empty right after GenerateOrLoadTLSCert persisted a cert")
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("generated tls.Certificate has no DER bytes")
	}

	// A second call must reuse the persisted cert, not regenerate — same PEM.
	if _, err := GenerateOrLoadTLSCert(get, save); err != nil {
		t.Fatalf("second GenerateOrLoadTLSCert: %v", err)
	}
	if got := CurrentCertPEM(get); got != pem {
		t.Fatalf("cert changed across a second GenerateOrLoadTLSCert call — this would break every already-paired client's TOFU pinning")
	}
}

// TestGenerateOrLoadWebTLSCert_CoversTheConfiguredHostname is the actual bug
// this function exists to fix: a browser rejects a certificate whose SAN
// doesn't include the hostname it navigated to, and trusting the issuer (the
// /trust flow) never bypasses that check. localhost alone (what the gRPC
// cert this used to reuse carries) was never enough.
func TestGenerateOrLoadWebTLSCert_CoversTheConfiguredHostname(t *testing.T) {
	get, save, _ := fakeSettingsStore()

	cert, err := GenerateOrLoadWebTLSCert("mycelium", get, save)
	if err != nil {
		t.Fatalf("GenerateOrLoadWebTLSCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parsing generated leaf: %v", err)
	}
	if !containsFold(leaf.DNSNames, "mycelium.local") {
		t.Fatalf("DNSNames = %v, want it to include %q", leaf.DNSNames, "mycelium.local")
	}
	if !containsFold(leaf.DNSNames, "localhost") {
		t.Fatalf("DNSNames = %v, want it to still include %q", leaf.DNSNames, "localhost")
	}
}

// TestGenerateOrLoadWebTLSCert_StableAcrossRepeatedCalls: unlike the gRPC
// cert, nothing pins this one's fingerprint, but it should still avoid
// needlessly regenerating (and thus forcing every browser to re-trust it) on
// every single call when the configured hostname hasn't changed.
func TestGenerateOrLoadWebTLSCert_StableAcrossRepeatedCalls(t *testing.T) {
	get, save, _ := fakeSettingsStore()

	first, err := GenerateOrLoadWebTLSCert("mycelium", get, save)
	if err != nil {
		t.Fatalf("first GenerateOrLoadWebTLSCert: %v", err)
	}
	second, err := GenerateOrLoadWebTLSCert("mycelium", get, save)
	if err != nil {
		t.Fatalf("second GenerateOrLoadWebTLSCert: %v", err)
	}
	if CurrentWebCertPEM(get) == "" {
		t.Fatal("CurrentWebCertPEM returned empty after generation")
	}
	if string(first.Certificate[0]) != string(second.Certificate[0]) {
		t.Fatal("cert regenerated on a second call with the same hostname — should have reused the persisted one")
	}
}

// TestGenerateOrLoadWebTLSCert_RegeneratesWhenHostnameChanges: an operator
// renaming mdns_hostname must get a cert that actually covers the new name
// on the next boot, not keep serving one valid only for the old one.
func TestGenerateOrLoadWebTLSCert_RegeneratesWhenHostnameChanges(t *testing.T) {
	get, save, _ := fakeSettingsStore()

	if _, err := GenerateOrLoadWebTLSCert("mycelium", get, save); err != nil {
		t.Fatalf("first GenerateOrLoadWebTLSCert: %v", err)
	}
	cert, err := GenerateOrLoadWebTLSCert("salotto", get, save)
	if err != nil {
		t.Fatalf("second GenerateOrLoadWebTLSCert (renamed): %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parsing regenerated leaf: %v", err)
	}
	if containsFold(leaf.DNSNames, "mycelium.local") {
		t.Fatalf("DNSNames = %v, still covers the old hostname after a rename", leaf.DNSNames)
	}
	if !containsFold(leaf.DNSNames, "salotto.local") {
		t.Fatalf("DNSNames = %v, want it to cover the new hostname %q", leaf.DNSNames, "salotto.local")
	}
}

package pileus

import "testing"

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

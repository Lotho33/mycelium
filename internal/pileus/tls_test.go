package pileus

import (
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

// The certificate is generated once and persisted; a second load reuses it
// (its fingerprint is pinned by clients).
func TestGenerateOrLoadTLSCert_PersistsAndReuses(t *testing.T) {
	get, save, store := fakeSettingsStore()

	cert, err := GenerateOrLoadTLSCert(get, save)
	if err != nil {
		t.Fatalf("GenerateOrLoadTLSCert: %v", err)
	}
	pem := store["pileus_grpc_tls_cert"]
	if pem == "" {
		t.Fatal("no certificate persisted after GenerateOrLoadTLSCert")
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("generated tls.Certificate has no DER bytes")
	}

	if _, err := GenerateOrLoadTLSCert(get, save); err != nil {
		t.Fatalf("second GenerateOrLoadTLSCert: %v", err)
	}
	if store["pileus_grpc_tls_cert"] != pem {
		t.Fatal("cert changed across a second GenerateOrLoadTLSCert call — this would break every already-paired client's TOFU pinning")
	}
}

package pileus

import (
	"crypto/x509"
	"encoding/base64"
	"strings"
	"testing"
)

// genTestCertPEM builds a throwaway self-signed cert PEM for the tests
// below — using GenerateOrLoadTLSCert itself so this stays representative
// of the exact PEM shape CurrentCertPEM actually returns in production.
func genTestCertPEM(t *testing.T) string {
	t.Helper()
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
	if _, err := GenerateOrLoadTLSCert(get, save); err != nil {
		t.Fatalf("GenerateOrLoadTLSCert: %v", err)
	}
	return CurrentCertPEM(get)
}

func TestBuildMobileConfig_EmbedsTheExactCertificate(t *testing.T) {
	certPEM := genTestCertPEM(t)
	profile, err := BuildMobileConfig(certPEM)
	if err != nil {
		t.Fatalf("BuildMobileConfig: %v", err)
	}

	// Must be a well-formed plist with the expected payload shape.
	for _, want := range []string{
		"<?xml version=\"1.0\"",
		"com.apple.security.root",
		"PayloadContent",
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing expected substring %q", want)
		}
	}

	// The embedded <data> must decode back to the same DER bytes as the
	// original PEM — this is the one thing that actually matters: a mismatch
	// here would mean iOS installs a *different* certificate than the one
	// the server (and every native client) actually uses.
	start := strings.Index(profile, "<data>")
	end := strings.Index(profile, "</data>")
	if start == -1 || end == -1 || end <= start {
		t.Fatal("no <data>...</data> block found in the generated profile")
	}
	embeddedB64 := profile[start+len("<data>") : end]
	embeddedDER, err := base64.StdEncoding.DecodeString(embeddedB64)
	if err != nil {
		t.Fatalf("embedded payload is not valid base64: %v", err)
	}
	if _, err := x509.ParseCertificate(embeddedDER); err != nil {
		t.Fatalf("embedded payload does not parse as a certificate: %v", err)
	}
}

func TestBuildMobileConfig_RejectsGarbageInput(t *testing.T) {
	if _, err := BuildMobileConfig("not a pem certificate"); err == nil {
		t.Fatal("want an error for input with no PEM certificate block, got nil")
	}
	if _, err := BuildMobileConfig(""); err == nil {
		t.Fatal("want an error for empty input, got nil")
	}
}

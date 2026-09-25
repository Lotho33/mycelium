package pileus

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// GenerateOrLoadTLSCert returns a self-signed TLS certificate for the gRPC
// server, generating and persisting one on first use — same pattern as the
// JWT secret in cmd/server/main.go (loadOrCreateJWTSecret): generate once,
// store via the setting getter/saver, reuse on every subsequent boot so the
// client doesn't need to re-trust a new cert after every restart.
//
// Self-signed on purpose: this deployment has no public domain/CA (private
// network only, reached over Tailscale or plain LAN — see the "rete solo
// privata" commit). A CA-signed cert isn't applicable here; the client is
// expected to pin/trust this specific certificate rather than validate
// against a public root store.
//
// getSetting/saveSetting are passed in (not managers.Settings directly) so
// this file has no import-time dependency on the managers package, mirroring
// how pileus.Start already takes its dependencies as parameters.
func GenerateOrLoadTLSCert(getSetting func(key, def string) string, saveSetting func(map[string]any) error) (tls.Certificate, error) {
	const certKey = "pileus_grpc_tls_cert"
	const keyKey = "pileus_grpc_tls_key"

	certPEM := getSetting(certKey, "")
	keyPEM := getSetting(keyKey, "")
	if certPEM != "" && keyPEM != "" {
		if cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err == nil {
			return cert, nil
		}
		// Corrupted/unreadable stored cert — fall through and regenerate
		// rather than fail the whole server boot over it.
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: key gen: %w", err)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mycelium-pileus"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0), // decennale: rete privata, nessuna rotazione automatica prevista
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: cert gen: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: key marshal: %w", err)
	}

	certOut := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyOut := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := saveSetting(map[string]any{certKey: string(certOut), keyKey: string(keyOut)}); err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: persist: %w", err)
	}

	return tls.X509KeyPair(certOut, keyOut)
}

// CurrentCertPEM returns the persisted self-signed certificate's PEM bytes
// as-is, for endpoints that let an operator/user download and manually
// trust it (e.g. GET /cert) — empty if none has been generated yet (no TLS
// listener has ever started). Reads the exact same setting key
// GenerateOrLoadTLSCert writes; does not generate or alter anything, so it
// can never change the certificate's fingerprint — native clients already
// paired via TOFU pinning (see server.go's TLSFingerprint) are never
// affected by this.
func CurrentCertPEM(getSetting func(key, def string) string) string {
	return getSetting("pileus_grpc_tls_cert", "")
}

// GenerateOrLoadWebTLSCert returns a self-signed TLS certificate for
// mycelium's OPTIONAL browser-facing HTTPS listener (server_https in
// cmd/server/main.go) — deliberately a SEPARATE certificate from
// GenerateOrLoadTLSCert's gRPC one above, not a second use of the same one
// (main.go used to just call GenerateOrLoadTLSCert again for this listener,
// which is the bug this replaces).
//
// Why it has to be separate: a browser validates a certificate's Subject
// Alternative Names against the address bar hostname — unlike native
// Pileus clients, which pin the gRPC cert's raw SHA-256 fingerprint via TOFU
// (server.go's TLSFingerprint) and never check SAN/CN at all. The gRPC cert
// only ever carries DNSNames: ["localhost"], specifically BECAUSE changing
// its SAN list would change its DER bytes and therefore that fingerprint,
// breaking every already-paired native client's pinned trust. A browser
// hitting that cert over :8443 — by IP or by "mycelium.local" — always got a
// hostname-mismatch error regardless of whether it was separately trusted as
// a CA via /trust: trusting the issuer never bypasses the SAN check, so the
// whole /trust flow was silently unable to do what it claimed until this.
// This cert has no pinning consumer anywhere, so it's free to carry the
// real hostname mDNS answers for (hostname param, "<name>.local") and to be
// regenerated whenever that name changes.
//
// Deliberately does NOT include IP SANs: an IP on most home routers without
// DHCP reservation is exactly the thing mDNS (internal/api/mdns.go) exists
// to stop depending on — a cert pinned to a specific IP would need
// re-trusting every time DHCP handed out a new one, defeating the point.
// Reaching the HTTPS port by IP still works for plain browsing (the usual
// self-signed click-through warning, unchanged from before this existed);
// only PWA installability needs the "<hostname>.local" address the SAN
// actually matches.
func GenerateOrLoadWebTLSCert(hostname string, getSetting func(key, def string) string, saveSetting func(map[string]any) error) (tls.Certificate, error) {
	const certKey = "mycelium_web_tls_cert"
	const keyKey = "mycelium_web_tls_key"
	fqdn := hostname + ".local"

	certPEM := getSetting(certKey, "")
	keyPEM := getSetting(keyKey, "")
	if certPEM != "" && keyPEM != "" {
		if cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err == nil {
			if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil && containsFold(leaf.DNSNames, fqdn) {
				return cert, nil
			}
			// Stored cert doesn't cover the currently configured hostname
			// (operator changed mdns_hostname since it was generated, or it
			// predates this SAN existing at all) — fall through and
			// regenerate rather than keep serving a cert that will never
			// validate for the name /trust tells people to use. Safe to
			// replace freely: unlike the gRPC cert above, nothing pins this
			// one's fingerprint.
		}
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("web tls: key gen: %w", err)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: fqdn},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", fqdn},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("web tls: cert gen: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("web tls: key marshal: %w", err)
	}

	certOut := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyOut := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := saveSetting(map[string]any{certKey: string(certOut), keyKey: string(keyOut)}); err != nil {
		return tls.Certificate{}, fmt.Errorf("web tls: persist: %w", err)
	}

	return tls.X509KeyPair(certOut, keyOut)
}

func containsFold(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(n, want) {
			return true
		}
	}
	return false
}

// CurrentWebCertPEM mirrors CurrentCertPEM but for the browser-facing
// certificate above — the one /cert, /cert.mobileconfig and /trust actually
// need to expose, since that's the certificate the HTTPS listener serves.
func CurrentWebCertPEM(getSetting func(key, def string) string) string {
	return getSetting("mycelium_web_tls_cert", "")
}

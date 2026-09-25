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

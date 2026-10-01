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

// GenerateOrLoadTLSCert returns the gRPC server's self-signed certificate,
// generating and persisting it on first use so clients keep the pinned one
// across restarts. There's no public CA for a private server: clients pin
// this certificate's fingerprint. getSetting/saveSetting keep this file
// independent of the managers package.
func GenerateOrLoadTLSCert(getSetting func(key, def string) string, saveSetting func(map[string]any) error) (tls.Certificate, error) {
	const certKey = "pileus_grpc_tls_cert"
	const keyKey = "pileus_grpc_tls_key"

	certPEM := getSetting(certKey, "")
	keyPEM := getSetting(keyKey, "")
	if certPEM != "" && keyPEM != "" {
		if cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM)); err == nil {
			return cert, nil
		}
		// Unreadable stored cert: regenerate rather than fail the boot.
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: key gen: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tls: serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "mycelium-pileus"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(10, 0, 0), // long-lived: clients pin it
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

// CurrentCertPEM returns the persisted certificate's PEM ("" when none was
// generated). It never changes the certificate.
func CurrentCertPEM(getSetting func(key, def string) string) string {
	return getSetting("pileus_grpc_tls_cert", "")
}

// randomSerial is a random 128-bit serial (Firefox rejects a regenerated
// self-signed cert that reuses issuer and serial).
func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

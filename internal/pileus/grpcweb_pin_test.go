package pileus

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The gRPC-web bridge dials the local gRPC server with InsecureSkipVerify but
// pins the leaf cert by SHA-256 (P1-6). pinGRPCServerCert is that check.
func TestPinGRPCServerCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	leaf := srv.Certificate()

	sum := sha256.Sum256(leaf.Raw)
	good := hex.EncodeToString(sum[:])

	saved := tlsFingerprint
	defer func() { tlsFingerprint = saved }()

	withCert := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}

	tlsFingerprint = good
	if err := pinGRPCServerCert(withCert); err != nil {
		t.Fatalf("matching cert rejected: %v", err)
	}

	tlsFingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := pinGRPCServerCert(withCert); err == nil {
		t.Fatal("mismatched cert accepted")
	}

	tlsFingerprint = ""
	if err := pinGRPCServerCert(withCert); err == nil {
		t.Fatal("accepted with no pin configured")
	}

	tlsFingerprint = good
	if err := pinGRPCServerCert(tls.ConnectionState{}); err == nil {
		t.Fatal("accepted with no peer certificates")
	}
}

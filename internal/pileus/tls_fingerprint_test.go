package pileus

import (
	"crypto/x509"
	"testing"
)

// Two generated gRPC certs must not share a serial: Firefox rejects a
// regenerated self-signed cert with the same issuer+serial.
func TestGRPCCert_RandomSerial(t *testing.T) {
	gen := func() *x509.Certificate {
		get, save, _ := fakeSettingsStore()
		cert, err := GenerateOrLoadTLSCert(get, save)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if gen().SerialNumber.Cmp(gen().SerialNumber) == 0 {
		t.Fatal("two generated certs share a serial number")
	}
}

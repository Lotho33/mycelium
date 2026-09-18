package core

import (
	"net"
	"testing"
)

// buildDNSQuery must encode the requested QTYPE (A vs AAAA).
func TestBuildDNSQuery_QType(t *testing.T) {
	q := buildDNSQuery(0x1234, "example.com.", dnsTypeAAAA)
	// …\x00 (root) QTYPE(2) QCLASS(2)
	qtype := uint16(q[len(q)-4])<<8 | uint16(q[len(q)-3])
	if qtype != dnsTypeAAAA {
		t.Fatalf("QTYPE = %d, want %d (AAAA)", qtype, dnsTypeAAAA)
	}
	if a := buildDNSQuery(0x1234, "x.", dnsTypeA); (uint16(a[len(a)-4])<<8 | uint16(a[len(a)-3])) != dnsTypeA {
		t.Errorf("A query QTYPE not 1")
	}
}

// parseDNSResponse must pull the AAAA rdata out as a v6 string.
func TestParseDNSResponse_AAAA(t *testing.T) {
	want := net.ParseIP("2a0d:3341:cc01:ea00::1")
	msg := []byte{
		0x12, 0x34, // id
		0x81, 0x80, // flags: response, no error
		0x00, 0x01, // QDCOUNT
		0x00, 0x01, // ANCOUNT
		0x00, 0x00, 0x00, 0x00, // NS/AR
		// question: 7"example" 3"com" 0
		0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0x03, 'c', 'o', 'm', 0x00,
		0x00, 0x1c, 0x00, 0x01, // QTYPE=AAAA QCLASS=IN
		// answer
		0xc0, 0x0c, // name ptr
		0x00, 0x1c, 0x00, 0x01, // TYPE=AAAA CLASS=IN
		0x00, 0x00, 0x00, 0x2d, // TTL
		0x00, 0x10, // RDLENGTH = 16
	}
	msg = append(msg, want...) // 16-byte v6 rdata

	got, err := parseDNSResponse(msg, 0x1234)
	if err != nil {
		t.Fatalf("parseDNSResponse: %v", err)
	}
	if !net.ParseIP(got).Equal(want) {
		t.Fatalf("got %q, want %s", got, want)
	}
}

package api

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func mustName(t *testing.T, s string) dnsmessage.Name {
	t.Helper()
	n, err := dnsmessage.NewName(s)
	if err != nil {
		t.Fatalf("dnsmessage.NewName(%q): %v", s, err)
	}
	return n
}

func TestAnyQuestionMatches(t *testing.T) {
	fqdn := "mycelium.local."
	cases := []struct {
		name string
		qs   []dnsmessage.Question
		want bool
	}{
		{
			"exact name, type A",
			[]dnsmessage.Question{{Name: mustName(t, fqdn), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
			true,
		},
		{
			"exact name, type ANY",
			[]dnsmessage.Question{{Name: mustName(t, fqdn), Type: dnsmessage.TypeALL, Class: dnsmessage.ClassINET}},
			true,
		},
		{
			"case-insensitive match",
			[]dnsmessage.Question{{Name: mustName(t, "MyCelium.LOCAL."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
			true,
		},
		{
			"different hostname",
			[]dnsmessage.Question{{Name: mustName(t, "other.local."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
			false,
		},
		{
			"right name, unsupported type (AAAA)",
			[]dnsmessage.Question{{Name: mustName(t, fqdn), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
			false,
		},
		{
			"no questions at all",
			nil,
			false,
		},
		{
			"one of several questions matches",
			[]dnsmessage.Question{
				{Name: mustName(t, "other.local."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
				{Name: mustName(t, fqdn), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
			},
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := anyQuestionMatches(c.qs, fqdn); got != c.want {
				t.Errorf("anyQuestionMatches(%v, %q) = %v, want %v", c.qs, fqdn, got, c.want)
			}
		})
	}
}

// TestBuildMDNSReply_RoundTrips builds a reply and re-parses it with the
// same Parser a real client would use — the regression that matters here is
// "does this actually decode back into a valid A record for the right name
// and IP", not just "does Finish() return no error".
func TestBuildMDNSReply_RoundTrips(t *testing.T) {
	fqdn := "mycelium.local."
	wantIP := net.IPv4(192, 168, 1, 42)

	reply, err := buildMDNSReply(0x1234, fqdn, wantIP)
	if err != nil {
		t.Fatalf("buildMDNSReply: %v", err)
	}

	var p dnsmessage.Parser
	hdr, err := p.Start(reply)
	if err != nil {
		t.Fatalf("re-parsing reply: %v", err)
	}
	if !hdr.Response {
		t.Error("reply header does not have Response=true")
	}
	if hdr.ID != 0x1234 {
		t.Errorf("reply ID = %#x, want %#x (must echo the query's ID)", hdr.ID, 0x1234)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatalf("SkipAllQuestions: %v", err)
	}
	answers, err := p.AllAnswers()
	if err != nil {
		t.Fatalf("AllAnswers: %v", err)
	}
	if len(answers) != 1 {
		t.Fatalf("want exactly 1 answer, got %d", len(answers))
	}
	a := answers[0]
	if got := a.Header.Name.String(); got != fqdn {
		t.Errorf("answer name = %q, want %q", got, fqdn)
	}
	aRes, ok := a.Body.(*dnsmessage.AResource)
	if !ok {
		t.Fatalf("answer body is %T, want *dnsmessage.AResource", a.Body)
	}
	if got := net.IP(aRes.A[:]); !got.Equal(wantIP) {
		t.Errorf("answer A record = %v, want %v", got, wantIP)
	}
}

func TestIsPrivateIPv4(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"172.15.255.255", false}, // just below the 172.16/12 range
		{"172.32.0.1", false},     // just above it
		{"8.8.8.8", false},
		{"100.64.0.1", false}, // Tailscale/CGNAT range — deliberately not "private" here
		{"169.254.1.1", false},
	}
	for _, c := range cases {
		t.Run(c.ip, func(t *testing.T) {
			ip := net.ParseIP(c.ip).To4()
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) failed", c.ip)
			}
			if got := isPrivateIPv4(ip); got != c.want {
				t.Errorf("isPrivateIPv4(%s) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
}

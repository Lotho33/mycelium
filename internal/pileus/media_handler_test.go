package pileus

import "testing"

func TestParseHTTPHostHint(t *testing.T) {
	const dp = "8000"
	cases := []struct {
		in                string
		wantHost, wantSch string
		wantOK            bool
	}{
		// bare host → default port, no scheme
		{"media.example.com", "media.example.com:8000", "", true},
		// host:port → verbatim, no scheme
		{"media.example.com:9000", "media.example.com:9000", "", true},
		// full origin, no explicit port → host stays portless, scheme kept
		{"https://media.example.com", "media.example.com", "https", true},
		// full origin with port
		{"http://10.0.0.5:8080", "10.0.0.5:8080", "http", true},
		// IPv6 literal with port
		{"[2001:db8::1]:8000", "[2001:db8::1]:8000", "", true},
		// loopback in every shape → rejected
		{"127.0.0.1:8000", "", "", false},
		{"localhost", "", "", false},
		{"http://localhost:3000", "", "", false},
		{"[::1]:8000", "", "", false},
		{"", "", "", false},
		{"   ", "", "", false},
	}
	for _, c := range cases {
		gotHost, gotSch, gotOK := parseHTTPHostHint(c.in, dp)
		if gotHost != c.wantHost || gotSch != c.wantSch || gotOK != c.wantOK {
			t.Errorf("parseHTTPHostHint(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, gotHost, gotSch, gotOK, c.wantHost, c.wantSch, c.wantOK)
		}
	}
}

func TestSplitServerHost(t *testing.T) {
	const dp = "8000"
	cases := []struct {
		in, wantHost, wantSch string
	}{
		// bare host keeps the historical behaviour: append default port
		{"box.lan", "box.lan:8000", ""},
		{"box.lan:1234", "box.lan:1234", ""},
		{"https://media.example.com", "media.example.com", "https"},
		{"https://media.example.com:8443", "media.example.com:8443", "https"},
		// operator may legitimately point at loopback — not rejected here
		{"127.0.0.1", "127.0.0.1:8000", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		gotHost, gotSch := splitServerHost(c.in, dp)
		if gotHost != c.wantHost || gotSch != c.wantSch {
			t.Errorf("splitServerHost(%q) = (%q, %q), want (%q, %q)",
				c.in, gotHost, gotSch, c.wantHost, c.wantSch)
		}
	}
}

func TestRedactProxyURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"https://box.local:8080/proxy/playlist.m3u8?data=aaa&cookies=bbb&origin=ccc",
			"https://box.local:8080/proxy/playlist.m3u8",
		},
		{
			"http://127.0.0.1:9000/proxy/segment.ts?data=x",
			"http://127.0.0.1:9000/proxy/segment.ts",
		},
		{"not-a-url", "(redacted)"},
	}
	for _, c := range cases {
		if got := redactProxyURL(c.in); got != c.want {
			t.Errorf("redactProxyURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

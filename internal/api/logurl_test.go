package api

import "testing"

func TestLogURL(t *testing.T) {
	saved := proxyDebug
	defer func() { proxyDebug = saved }()

	proxyDebug = false
	cases := []struct{ in, want string }{
		{
			"https://cdn.example.com/hls/master.m3u8?token=abc&exp=123",
			"https://cdn.example.com/hls/master.m3u8?<redacted>",
		},
		{
			"http://box.local:8080/proxy/segment.ts?data=aaa&cookies=bbb",
			"http://box.local:8080/proxy/segment.ts?<redacted>",
		},
		{"https://cdn.example.com/path/no/query", "https://cdn.example.com/path/no/query"},
		// bare incoming request URL (path only) — no "://" prefix
		{"/proxy/segment.ts?data=aaa&sig=bbb", "/proxy/segment.ts?<redacted>"},
		{"/proxy/playlist.m3u8", "/proxy/playlist.m3u8"},
		{"::not-a-url", "(unparseable url)"},
	}
	for _, c := range cases {
		if got := logURL(c.in); got != c.want {
			t.Errorf("logURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	proxyDebug = true
	full := "https://cdn.example.com/x?token=secret"
	if got := logURL(full); got != full {
		t.Errorf("with proxyDebug set, logURL(%q) = %q, want it unchanged", full, got)
	}
}

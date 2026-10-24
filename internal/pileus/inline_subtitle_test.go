package pileus

import (
	"strings"
	"testing"

	"mycelium/internal/core"
)

// subtitles[].data_base64 becomes an inline track; bad base64 is dropped.
func TestParseLuaResolved_InlineSubtitle(t *testing.T) {
	rs, err := parseLuaResolved([]byte(`{"url":"https://x/m.m3u8","subtitles":[
		{"data_base64":"MQowMDowMDowMSwwMDAgLS0+IDAwOjAwOjAyLDAwMApDaWFvCg==","lang":"it","name":"Italiano"},
		{"data_base64":"%%%","lang":"it"},
		{"url":"https://x/b.srt","lang":"it"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Subtitles) != 2 || !strings.HasPrefix(rs.Subtitles[0].URL, core.InlineSubtitleScheme) || rs.Subtitles[1].URL != "https://x/b.srt" {
		t.Fatalf("subtitles = %+v", rs.Subtitles)
	}
	if b, ok := core.InlineSubtitle(rs.Subtitles[0].URL); !ok || !strings.Contains(string(b), "Ciao") {
		t.Fatalf("inline bytes = %q", b)
	}
}

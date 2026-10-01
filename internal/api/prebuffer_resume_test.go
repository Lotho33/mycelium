package api

import (
	"reflect"
	"testing"
)

func TestSegmentURLs_FromResumePoint(t *testing.T) {
	const pl = "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n" +
		"#EXTINF:6,\ns0.m4s\n#EXTINF:6,\ns1.m4s\n#EXTINF:6,\ns2.m4s\n#EXTINF:6,\ns3.m4s\n#EXTINF:6,\ns4.m4s\n#EXT-X-ENDLIST\n"
	base := "http://cdn/v/"
	u := func(n string) string { return base + n }

	// From the start: init + the first segments.
	if got := segmentURLs(base+"i.m3u8", []byte(pl), 3, 0); !reflect.DeepEqual(got, []string{u("init.mp4"), u("s0.m4s"), u("s1.m4s")}) {
		t.Errorf("from 0: %v", got)
	}
	// Resume at 13 s: s2 covers [12,18) — init first, then s2, s3.
	if got := segmentURLs(base+"i.m3u8", []byte(pl), 3, 13); !reflect.DeepEqual(got, []string{u("init.mp4"), u("s2.m4s"), u("s3.m4s")}) {
		t.Errorf("from 13s: %v", got)
	}
	// Exactly on a boundary: 12 s starts s2.
	if got := segmentURLs(base+"i.m3u8", []byte(pl), 2, 12); !reflect.DeepEqual(got, []string{u("init.mp4"), u("s2.m4s")}) {
		t.Errorf("from 12s: %v", got)
	}
	// TS (no init section), resume near the end.
	const ts = "#EXTM3U\n#EXTINF:10,\na.ts\n#EXTINF:10,\nb.ts\n#EXTINF:10,\nc.ts\n#EXT-X-ENDLIST\n"
	if got := segmentURLs(base+"i.m3u8", []byte(ts), 5, 25); !reflect.DeepEqual(got, []string{u("c.ts")}) {
		t.Errorf("ts from 25s: %v", got)
	}
}

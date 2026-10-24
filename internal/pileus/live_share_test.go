package pileus

import (
	"testing"

	"mycelium/internal/managers"
)

func TestLiveShare_ReusedOnlyWhileWatched(t *testing.T) {
	old := shareLiveEnabled
	shareLiveEnabled = func(string) bool { return true }
	t.Cleanup(func() { shareLiveEnabled = old })
	liveShares.Lock()
	liveShares.m = map[string]liveShare{}
	liveShares.Unlock()

	rs := &ResolvedStream{URL: "https://cdn/live.m3u8", IsLive: true}
	liveShareStore("p", "ev1", rs)
	if liveShared("p", "ev1") != nil {
		t.Fatal("shared with nobody watching")
	}
	liveShareStore("p", "ev1", rs)
	s := &managers.StreamSession{PluginID: "p", SourceID: "ev1", IsLive: true, DeviceID: "live-share-test"}
	managers.Sessions.Register("live-share-test", s)
	t.Cleanup(func() { managers.Sessions.RevokeDevice("live-share-test") })
	if got := liveShared("p", "ev1"); got != rs {
		t.Fatalf("got %v, want the stored link", got)
	}
	if liveShared("p", "ev2") != nil {
		t.Fatal("another stream got the link")
	}
	// A VOD result is never shared.
	liveShareStore("p", "vod", &ResolvedStream{URL: "https://cdn/v.m3u8"})
	liveShares.Lock()
	_, kept := liveShares.m[resolveFlightKey("p", "vod")]
	liveShares.Unlock()
	if kept {
		t.Fatal("VOD stored")
	}
}

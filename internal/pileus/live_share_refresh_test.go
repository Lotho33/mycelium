package pileus

import "testing"

func TestRefreshLiveShare(t *testing.T) {
	orig := shareLiveEnabled
	shareLiveEnabled = func(string) bool { return true }
	defer func() { shareLiveEnabled = orig }()

	// No shared link yet: nothing to refresh, nothing created.
	RefreshLiveShare("p", "s-none", "https://x/new.m3u8", nil)
	liveShares.Lock()
	_, created := liveShares.m[resolveFlightKey("p", "s-none")]
	liveShares.Unlock()
	if created {
		t.Fatal("refresh created a share")
	}

	key := resolveFlightKey("p", "s1")
	old := &ResolvedStream{URL: "https://x/live.m3u8?t=old", IsLive: true, Extra: map[string]string{"k": "v"}}
	liveShares.Lock()
	liveShares.m[key] = liveShare{pluginID: "p", streamID: "s1", rs: old}
	liveShares.Unlock()
	defer func() { liveShares.Lock(); delete(liveShares.m, key); liveShares.Unlock() }()

	RefreshLiveShare("p", "s1", "https://x/live.m3u8?t=new", map[string]string{"Referer": "https://r/"})
	liveShares.Lock()
	got := liveShares.m[key].rs
	liveShares.Unlock()
	if got.URL != "https://x/live.m3u8?t=new" || got.Headers["Referer"] != "https://r/" {
		t.Fatalf("share not refreshed: %+v", got)
	}
	if !got.IsLive || got.Extra["k"] != "v" {
		t.Fatal("refresh dropped the rest of the resolve")
	}
	if old.URL != "https://x/live.m3u8?t=old" {
		t.Fatal("the stream already handed out was modified in place")
	}
}

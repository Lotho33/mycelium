package pileus

import "testing"

// deviceActive memoises the DB check; forgetDeviceActive drops the entry so the
// next call re-queries. The auth interceptor relies on both (P1-4).
func TestDeviceActiveCache(t *testing.T) {
	var calls int
	var live bool
	orig := deviceActiveLookup
	deviceActiveLookup = func(string) bool { calls++; return live }
	t.Cleanup(func() { deviceActiveLookup = orig })
	deviceActiveCache.Delete("d1")

	live = true
	if !deviceActive("d1") {
		t.Fatal("want active")
	}
	if calls != 1 {
		t.Fatalf("first call: calls=%d want 1", calls)
	}

	// within TTL: served from cache, no new lookup
	deviceActive("d1")
	deviceActive("d1")
	if calls != 1 {
		t.Fatalf("cache not used: calls=%d want 1", calls)
	}

	// revoke path: forget forces a re-query, now reporting inactive
	forgetDeviceActive("d1")
	live = false
	if deviceActive("d1") {
		t.Fatal("want inactive after revoke")
	}
	if calls != 2 {
		t.Fatalf("after forget: calls=%d want 2", calls)
	}
	// and that inactive result is itself cached
	deviceActive("d1")
	if calls != 2 {
		t.Fatalf("inactive result not cached: calls=%d", calls)
	}

	// empty id is never active and never hits the DB
	if deviceActive("") {
		t.Fatal("empty id reported active")
	}
	if calls != 2 {
		t.Fatalf("empty id queried the DB: calls=%d", calls)
	}
}

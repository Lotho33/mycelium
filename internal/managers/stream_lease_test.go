package managers

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func newTestLeases() (*leaseRegistry, *time.Time) {
	now := time.Unix(1_000_000, 0)
	r := &leaseRegistry{leases: map[string]*playbackLease{}, now: func() time.Time { return now }}
	return r, &now
}

func TestPlaybackLease_OtherDeviceRefusedWhileFresh(t *testing.T) {
	r, now := newTestLeases()
	if _, ok := r.Claim("p1", "tv"); !ok {
		t.Fatal("first claim must succeed")
	}
	if h, ok := r.Claim("p1", "phone"); ok || h != "tv" {
		t.Fatalf("phone claim = (%q,%v), want refused by tv", h, ok)
	}
	if h := r.Holder("p1", "phone"); h != "tv" {
		t.Fatalf("Holder = %q, want tv", h)
	}
	// Same device always passes (next episode, re-resolve).
	if _, ok := r.Claim("p1", "tv"); !ok {
		t.Fatal("same device must renew")
	}
	// Other profiles are independent.
	if _, ok := r.Claim("p2", "phone"); !ok {
		t.Fatal("another profile must be free")
	}
	// Renewals keep it alive past one TTL...
	*now = now.Add(PlaybackLeaseTTL - time.Second)
	r.Claim("p1", "tv")
	*now = now.Add(PlaybackLeaseTTL - time.Second)
	if _, ok := r.Claim("p1", "phone"); ok {
		t.Fatal("renewed lease must still refuse phone")
	}
	// ...and it lapses once the holder goes quiet.
	*now = now.Add(PlaybackLeaseTTL)
	if h := r.Holder("p1", "phone"); h != "" {
		t.Fatalf("expired lease still held by %q", h)
	}
	if _, ok := r.Claim("p1", "phone"); !ok {
		t.Fatal("phone must take over an expired lease")
	}
	if _, ok := r.Claim("p1", "tv"); ok {
		t.Fatal("tv lost the lease, its claim must now be refused")
	}
}

func TestPlaybackLease_NoProfileNeverConflicts(t *testing.T) {
	r, _ := newTestLeases()
	r.Claim("", "tv")
	if _, ok := r.Claim("", "phone"); !ok {
		t.Fatal("no profile, nothing to share")
	}
}

func TestPlaybackLease_ReleaseDevice(t *testing.T) {
	r, _ := newTestLeases()
	r.Claim("p1", "tv")
	r.Claim("p2", "tv")
	r.ReleaseDevice("tv")
	if _, ok := r.Claim("p1", "phone"); !ok {
		t.Fatal("revoked device's lease must be gone")
	}
	if h := r.Holder("p2", "phone"); h != "" {
		t.Fatalf("p2 still held by %q", h)
	}
}

// Every pooled connection must carry busy_timeout, not only the first one.
func TestOpenSQLite_PragmasOnEveryConnection(t *testing.T) {
	db, err := openSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var conns []*sql.Conn
	for i := 0; i < 3; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		var timeout int
		var mode string
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if timeout != 5000 || mode != "wal" {
			t.Errorf("conn %d: busy_timeout=%d journal_mode=%s", i, timeout, mode)
		}
		c.Close()
	}
}

// "Watch here": a device takes the playback over, the previous holder loses
// it (its claims fail) until it takes it back.
func TestPlaybackLease_TakeOverAndRelease(t *testing.T) {
	r, _ := newTestLeases()
	r.Claim("p1", "tv")
	if prev := r.TakeOver("p1", "phone"); prev != "tv" {
		t.Fatalf("previous holder %q", prev)
	}
	if h, ok := r.Claim("p1", "tv"); ok || h != "phone" {
		t.Fatalf("tv still holds after takeover: %q %v", h, ok)
	}
	if _, ok := r.Claim("p1", "phone"); !ok {
		t.Fatal("new holder refused")
	}
	// Closing the player releases right away — only for the holder.
	if r.ReleaseIfHolder("p1", "tv") {
		t.Fatal("a non-holder released the lease")
	}
	if !r.ReleaseIfHolder("p1", "phone") {
		t.Fatal("holder couldn't release")
	}
	if h := r.Holder("p1", "tv"); h != "" {
		t.Fatalf("lease not free after release: %q", h)
	}
	if prev := r.TakeOver("p2", "tv"); prev != "" {
		t.Fatalf("takeover of a free profile reported %q", prev)
	}
}

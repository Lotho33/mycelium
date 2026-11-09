package pileus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
)

type memCatalogStore struct {
	mu   sync.Mutex
	data map[string]string
	ttls map[string]time.Duration
}

func newMemCatalogStore() *memCatalogStore {
	return &memCatalogStore{data: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (s *memCatalogStore) Get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key], nil
}

func (s *memCatalogStore) Set(_ context.Context, key, value string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	s.ttls[key] = ttl
	return nil
}

func (s *memCatalogStore) expire(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}

func (s *memCatalogStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[key] != ""
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condizione non raggiunta in tempo")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func itemsFetch(calls *atomic.Int32, title string, err error) catalogFetch {
	return func(context.Context) ([]*gen.CatalogItem, bool, error) {
		calls.Add(1)
		if err != nil {
			return nil, false, err
		}
		return []*gen.CatalogItem{{Title: title}}, false, nil
	}
}

func TestCachedCatalogPage_StaleServedThenRefreshed(t *testing.T) {
	store := newMemCatalogStore()
	const key = "k"
	var calls atomic.Int32

	// Miss: fetched inline, both copies stored.
	resp, err := cachedCatalogPage(context.Background(), store, key, time.Minute, itemsFetch(&calls, "v1", nil))
	if err != nil || resp.Items[0].Title != "v1" {
		t.Fatalf("primo fetch: %v %v", resp, err)
	}
	waitFor(t, func() bool { return store.has(key) && store.has(catalogStaleKey(key)) })
	if store.ttls[catalogStaleKey(key)] != staleCatalogMin {
		t.Fatalf("ttl stale = %s, want %s", store.ttls[catalogStaleKey(key)], staleCatalogMin)
	}

	// Fresh hit: no fetch.
	if _, err := cachedCatalogPage(context.Background(), store, key, time.Minute, itemsFetch(&calls, "x", nil)); err != nil || calls.Load() != 1 {
		t.Fatalf("hit fresco: calls=%d err=%v", calls.Load(), err)
	}

	// Fresh expired: stale served at once, refresh in background.
	store.expire(key)
	resp, err = cachedCatalogPage(context.Background(), store, key, time.Minute, itemsFetch(&calls, "v2", nil))
	if err != nil || resp.Items[0].Title != "v1" {
		t.Fatalf("stale non servita: %v %v", resp, err)
	}
	waitFor(t, func() bool { return store.has(key) })
	resp, _ = cachedCatalogPage(context.Background(), store, key, time.Minute, itemsFetch(&calls, "x", nil))
	if resp.Items[0].Title != "v2" {
		t.Fatalf("dopo il refresh = %q, want v2", resp.Items[0].Title)
	}
}

func TestCachedCatalogPage_FailedRefreshKeepsStale(t *testing.T) {
	store := newMemCatalogStore()
	const key = "k2"
	var calls atomic.Int32
	if _, err := cachedCatalogPage(context.Background(), store, key, time.Minute, itemsFetch(&calls, "v1", nil)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return store.has(catalogStaleKey(key)) })
	store.expire(key)

	busy := errors.New("busy")
	for range 3 {
		resp, err := cachedCatalogPage(context.Background(), store, key, time.Minute, itemsFetch(&calls, "", busy))
		if err != nil || resp.Items[0].Title != "v1" {
			t.Fatalf("con refresh fallito serve la stale: %v %v", resp, err)
		}
	}
	waitFor(t, func() bool { _, running := catalogRefreshing.Load(key); return !running })
	if store.has(key) {
		t.Fatal("un refresh fallito non deve scrivere la copia fresca")
	}
}

func TestCachedCatalogPage_NoStoreAndErrors(t *testing.T) {
	var calls atomic.Int32
	if _, err := cachedCatalogPage(context.Background(), nil, "k3", time.Minute, itemsFetch(&calls, "", errors.New("down"))); err == nil {
		t.Fatal("senza cache l'errore del plugin deve arrivare al chiamante")
	}
	resp, err := cachedCatalogPage(context.Background(), nil, "k3", time.Minute, itemsFetch(&calls, "v", nil))
	if err != nil || resp.Items[0].Title != "v" {
		t.Fatalf("senza cache: %v %v", resp, err)
	}
}

func TestStaleCatalogTTL(t *testing.T) {
	for _, c := range []struct{ ttl, want time.Duration }{
		{90 * time.Second, 12 * time.Minute},
		{time.Second, staleCatalogMin},
		{15 * time.Minute, 2 * time.Hour},
		{24 * time.Hour, staleCatalogMax},
	} {
		if got := staleCatalogTTL(c.ttl); got != c.want {
			t.Errorf("staleCatalogTTL(%s) = %s, want %s", c.ttl, got, c.want)
		}
	}
}

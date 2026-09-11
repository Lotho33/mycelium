package managers

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRepo wraps go-redis with the key namespacing used by Mycelium.
//
// Key conventions (from architecture docs):
//   - Secrets:       mycelium:plugin:{pluginID}:user:{profileID}:secrets  (Hash, no TTL)
//   - Catalog cache: mycelium:plugin:{pluginID}:catalog:{catalogID}:p{page} (TTL 15min)
//   - Device session: mycelium:device:{deviceID}:session                   (TTL 30d)
type RedisRepo struct {
	client *redis.Client
}

// Redis is the process-wide singleton. Nil when Redis is unavailable.
var Redis *RedisRepo

// InitRedis connects to Redis at addr (e.g. "redis:6379" or "localhost:6379").
// Non-fatal: if connection fails, Redis remains nil and callers must handle that.
func InitRedis(addr string) error {
	if addr == "" {
		addr = "localhost:6379"
	}
	opts := &redis.Options{
		Addr:         addr,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	}
	// In Docker/Podman the process-wide net.DefaultResolver is overridden to use
	// Cloudflare 1.1.1.1, which cannot resolve container-internal hostnames like
	// "redis". Use a resolver that reads the container engine's own
	// /etc/resolv.conf (127.0.0.11 for Docker, the network gateway's
	// aardvark-dns for Podman) instead of hardcoding an engine-specific IP.
	if os.Getenv("MYCELIUM_DOCKER") == "1" {
		dockerResolver := &net.Resolver{PreferGo: true}
		dockerDialer := &net.Dialer{Timeout: 5 * time.Second, Resolver: dockerResolver}
		opts.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dockerDialer.DialContext(ctx, network, addr)
		}
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return err
	}
	Redis = &RedisRepo{client: client}
	log.Printf("[redis] connected to %s", addr)
	return nil
}

// Close shuts down the Redis client.
func (r *RedisRepo) Close() {
	if r != nil {
		r.client.Close()
	}
}

// Get returns the string value for key. Returns ("", nil) when the key does not exist.
func (r *RedisRepo) Get(ctx context.Context, key string) (string, error) {
	val, err := r.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return val, err
}

// Set stores a string value with an optional TTL (0 = no expiry).
func (r *RedisRepo) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

// Del removes one or more keys.
func (r *RedisRepo) Del(ctx context.Context, keys ...string) error {
	return r.client.Del(ctx, keys...).Err()
}

// Expire sets the TTL on an existing key.
func (r *RedisRepo) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return r.client.Expire(ctx, key, ttl).Err()
}

// HGetAll returns all field-value pairs of a Hash key.
// Returns an empty map (not nil) when the key does not exist.
func (r *RedisRepo) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	result, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	return result, nil
}

// HGet returns a single field from a Hash. Returns ("", nil) when field absent.
func (r *RedisRepo) HGet(ctx context.Context, key, field string) (string, error) {
	val, err := r.client.HGet(ctx, key, field).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return val, err
}

// HSet sets one or more field-value pairs in a Hash.
func (r *RedisRepo) HSet(ctx context.Context, key string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	return r.client.HSet(ctx, key, args...).Err()
}

// HDel removes one or more fields from a Hash key.
func (r *RedisRepo) HDel(ctx context.Context, key string, fields ...string) error {
	return r.client.HDel(ctx, key, fields...).Err()
}

// FlushDB removes every key in the current Redis database. Used by the admin
// "wipe plugin data" action — the compose stack runs a Redis instance
// dedicated to mycelium, so a full flush is the intended reset.
func (r *RedisRepo) FlushDB(ctx context.Context) error {
	return r.client.FlushDB(ctx).Err()
}

// DelByPattern SCANs for keys matching pattern and deletes them in batches.
// Returns the number of keys deleted. Non-atomic by design — safe to run
// against a live DB.
func (r *RedisRepo) DelByPattern(ctx context.Context, pattern string) (int, error) {
	var deleted int
	batch := make([]string, 0, 500)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := r.client.Del(ctx, batch...).Err(); err != nil {
			return err
		}
		deleted += len(batch)
		batch = batch[:0]
		return nil
	}
	iter := r.client.Scan(ctx, 0, pattern, 500).Iterator()
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) >= 500 {
			if err := flush(); err != nil {
				return deleted, err
			}
		}
	}
	if err := iter.Err(); err != nil {
		return deleted, err
	}
	err := flush()
	return deleted, err
}

// MGet fetches multiple keys in a single round-trip.
// Missing keys are returned as empty strings (not errors).
func (r *RedisRepo) MGet(ctx context.Context, keys ...string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	vals, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(vals))
	for i, v := range vals {
		if v != nil {
			out[i] = v.(string)
		}
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Namespace helpers
// ─────────────────────────────────────────────────────────────────────────────

// SecretsKey returns the Redis Hash key for per-profile plugin secrets
// (mycelium.context.get_secret/set_secret, Lua-side only — a plugin storing
// its own state, e.g. a token it obtained itself, not an admin/user-facing
// setting).
func SecretsKey(pluginID, profileID string) string {
	return "mycelium:plugin:" + pluginID + ":user:" + profileID + ":secrets"
}

// CatalogCacheKey returns the Redis string key for a paginated catalog response.
func CatalogCacheKey(pluginID, catalogID string, page int) string {
	return "mycelium:plugin:" + pluginID + ":catalog:" + catalogID + ":p" + itoa(page)
}

// CatalogCountKey returns the Redis key that stores the last known item count
// for page-1 of a catalog. Used by ListPlugins to suppress empty carousels.
func CatalogCountKey(pluginID, catalogID string) string {
	return "mycelium:plugin:" + pluginID + ":catalog_count:" + catalogID
}

// DeviceSessionKey returns the Redis string key for a device session.
func DeviceSessionKey(deviceID string) string {
	return "mycelium:device:" + deviceID + ":session"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

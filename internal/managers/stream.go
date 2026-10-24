package managers

import (
	"log"
	"sync"
	"time"

	"mycelium/internal/core"
)

// ─── Playback leases ──────────────────────────────────────────────────────────
// A profile may be playing on one device at a time (browsing is never
// limited). The lease is claimed when ResolveStream hands out a stream and
// renewed by the player's traffic (every proxied playlist/segment/key fetch)
// and its UpdateProgress heartbeat. It lapses after PlaybackLeaseTTL without
// renewal. Another device is refused while it's fresh; the same device
// always gets through.

// PlaybackLeaseTTL is how long a lease survives without renewal; well above
// the renewal gaps of a healthy player (heartbeat every 15s, live reloads
// every few seconds).
const PlaybackLeaseTTL = 90 * time.Second

type playbackLease struct {
	deviceID string
	lastSeen time.Time
}

type leaseRegistry struct {
	mu     sync.Mutex
	leases map[string]*playbackLease // profileID -> lease
	now    func() time.Time
}

// PlaybackLeases is the process-wide lease table (in memory: a restart frees
// every profile).
var PlaybackLeases = &leaseRegistry{
	leases: make(map[string]*playbackLease),
	now:    time.Now,
}

// Holder returns the device currently holding a fresh lease on profileID other
// than deviceID, or "" when deviceID may play. Read-only: it doesn't claim.
func (r *leaseRegistry) Holder(profileID, deviceID string) string {
	if profileID == "" || deviceID == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.leases[profileID]
	if l == nil || l.deviceID == deviceID || r.now().Sub(l.lastSeen) >= PlaybackLeaseTTL {
		return ""
	}
	return l.deviceID
}

// Claim takes or renews profileID's lease for deviceID. It fails (returning
// the holder) only when another device holds a fresh lease. Empty
// profileID/deviceID always succeed.
func (r *leaseRegistry) Claim(profileID, deviceID string) (holder string, ok bool) {
	if profileID == "" || deviceID == "" {
		return "", true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if l := r.leases[profileID]; l != nil && l.deviceID != deviceID && now.Sub(l.lastSeen) < PlaybackLeaseTTL {
		return l.deviceID, false
	}
	r.leases[profileID] = &playbackLease{deviceID: deviceID, lastSeen: now}
	return "", true
}

// TakeOver gives profileID's playback to deviceID even if another device
// holds it ("watch here") and returns the previous holder. That device's
// next /proxy fetch is refused and its next heartbeat learns about it.
func (r *leaseRegistry) TakeOver(profileID, deviceID string) (previous string) {
	if profileID == "" || deviceID == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if l := r.leases[profileID]; l != nil && l.deviceID != deviceID && now.Sub(l.lastSeen) < PlaybackLeaseTTL {
		previous = l.deviceID
	}
	r.leases[profileID] = &playbackLease{deviceID: deviceID, lastSeen: now}
	return previous
}

// ReleaseIfHolder frees profileID's lease when deviceID holds it.
func (r *leaseRegistry) ReleaseIfHolder(profileID, deviceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.leases[profileID]; l != nil && l.deviceID == deviceID {
		delete(r.leases, profileID)
		return true
	}
	return false
}

// heldBy reports whether deviceID holds a fresh lease on profileID.
func (r *leaseRegistry) heldBy(profileID, deviceID string) bool {
	if profileID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.leases[profileID]
	return l != nil && l.deviceID == deviceID && r.now().Sub(l.lastSeen) < PlaybackLeaseTTL
}

// ReleaseDevice drops every lease deviceID holds (device revoked/deleted)
// and retires its playback sessions (Sessions.RevokeDevice).
func (r *leaseRegistry) ReleaseDevice(deviceID string) {
	Sessions.RevokeDevice(deviceID)
	r.mu.Lock()
	defer r.mu.Unlock()
	for pid, l := range r.leases {
		if l.deviceID == deviceID {
			delete(r.leases, pid)
		}
	}
}

// Release drops profileID's lease whoever holds it.
func (r *leaseRegistry) Release(profileID string) {
	r.mu.Lock()
	delete(r.leases, profileID)
	r.mu.Unlock()
}

func (r *leaseRegistry) cleanupStale() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for pid, l := range r.leases {
		if now.Sub(l.lastSeen) >= PlaybackLeaseTTL {
			delete(r.leases, pid)
		}
	}
}

// ─── Session registry ─────────────────────────────────────────────────────────
// Active playback sessions, one per ResolveStream, keyed by the random token
// carried in the uid of every /proxy URL. Segment fetches update the
// estimated position.

type StreamSession struct {
	UserID string // progress key: the profile, or the device without one
	// ProfileID/DeviceID identify the playback lease; empty ProfileID = none.
	ProfileID string
	DeviceID  string
	PluginID  string
	SourceID  string
	IsLive    bool
	TotalSec  float64 // 0 when unknown
	// StartSec is where playback opened (the resume point): the position
	// estimate counts segments from there.
	StartSec float64

	mu          sync.Mutex
	segDuration float64 // average segment duration (s)
	segsFetched int
	lastSeen    time.Time
}

// ClaimPlayback renews the lease behind this session. ok=false: the profile
// is now playing on holder, another device.
func (s *StreamSession) ClaimPlayback() (holder string, ok bool) {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
	return PlaybackLeases.Claim(s.ProfileID, s.DeviceID)
}

// UpdateSegDuration updates the average segment duration (from #EXTINF).
func (s *StreamSession) UpdateSegDuration(d float64) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	if s.segDuration == 0 {
		s.segDuration = d
	} else {
		s.segDuration = (s.segDuration + d) / 2
	}
	s.mu.Unlock()
}

// RecordFetch counts a segment and periodically updates the stored position.
func (s *StreamSession) RecordFetch() {
	s.mu.Lock()
	s.segsFetched++
	s.lastSeen = time.Now()
	if s.IsLive || s.segDuration == 0 || DB == nil {
		s.mu.Unlock()
		return
	}
	pos := s.StartSec + float64(s.segsFetched)*s.segDuration
	interval := max(1, int(30.0/s.segDuration))
	if s.segsFetched%interval != 0 {
		s.mu.Unlock()
		return
	}
	snap := struct {
		userID, pluginID, sourceID string
		pos, total                 float64
	}{s.UserID, s.PluginID, s.SourceID, pos, s.TotalSec}
	s.mu.Unlock()

	// UpdateProgressPosition, not UpsertProgress: no metadata here, and the
	// stream id may differ from the id the client reports progress with.
	core.SafeGo("stream/progress-snapshot", func() {
		if err := DB.UpdateProgressPosition(
			snap.userID, snap.pluginID, snap.sourceID,
			snap.pos, snap.total,
		); err != nil {
			log.Printf("[stream/progress] %v", err)
		}
	})
}

type sessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*StreamSession
	// revoked holds the tokens of revoked/deleted/re-paired devices until
	// their /proxy URLs expire (token -> expiry): an unknown uid is let
	// through, a revoked one must not be.
	revoked map[string]time.Time
}

var Sessions = &sessionRegistry{
	sessions: make(map[string]*StreamSession),
	revoked:  make(map[string]time.Time),
}

func (r *sessionRegistry) Register(token string, s *StreamSession) {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
	r.mu.Lock()
	r.sessions[token] = s
	r.mu.Unlock()
}

func (r *sessionRegistry) Get(token string) (*StreamSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[token]
	return s, ok
}

// ActiveWithin reports whether a playback session fetched something through
// the proxy within d, i.e. someone is watching (downloads pause for it).
func (r *sessionRegistry) ActiveWithin(d time.Duration) bool {
	cutoff := time.Now().Add(-d)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		s.mu.Lock()
		recent := s.lastSeen.After(cutoff)
		s.mu.Unlock()
		if recent {
			return true
		}
	}
	return false
}

// RevokeDevice retires every session of deviceID: their uids are refused
// from now on (Revoked) instead of counting as unknown.
// LiveWatched reports whether a live session of pluginID/sourceID fetched
// something through the proxy within d.
func (r *sessionRegistry) LiveWatched(pluginID, sourceID string, d time.Duration) bool {
	cutoff := time.Now().Add(-d)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		if !s.IsLive || s.PluginID != pluginID || s.SourceID != sourceID {
			continue
		}
		s.mu.Lock()
		recent := s.lastSeen.After(cutoff)
		s.mu.Unlock()
		if recent {
			return true
		}
	}
	return false
}

func (r *sessionRegistry) RevokeDevice(deviceID string) {
	if deviceID == "" {
		return
	}
	until := time.Now().Add(core.ProxyURLTTL)
	r.mu.Lock()
	defer r.mu.Unlock()
	for token, s := range r.sessions {
		if s.DeviceID == deviceID {
			delete(r.sessions, token)
			r.revoked[token] = until
		}
	}
}

// Revoked reports whether token belonged to a revoked device.
func (r *sessionRegistry) Revoked(token string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.revoked[token]
	return ok
}

// cleanupStale drops sessions idle for idle, unless their lease is still
// renewed (a paused player sends heartbeats, not /proxy fetches), and
// revoked tokens whose URLs have expired.
func (r *sessionRegistry) cleanupStale(idle time.Duration) {
	now := time.Now()
	cutoff := now.Add(-idle)
	r.mu.Lock()
	defer r.mu.Unlock()
	for token, s := range r.sessions {
		s.mu.Lock()
		stale := !s.lastSeen.IsZero() && s.lastSeen.Before(cutoff)
		s.mu.Unlock()
		if stale && !PlaybackLeases.heldBy(s.ProfileID, s.DeviceID) {
			delete(r.sessions, token)
		}
	}
	for token, until := range r.revoked {
		if now.After(until) {
			delete(r.revoked, token)
		}
	}
}

// ─── Proxy owners (downloads) ─────────────────────────────────────────────────
// The downloader fetches through /proxy like a player but must not be a
// playback Session (it would claim the lease and pause itself). Its uid only
// names the owner profile, so an expired token is re-resolved with that
// profile's plugin login.

type proxyOwner struct {
	profileID string
	lastUsed  time.Time
}

type proxyOwnerRegistry struct {
	mu sync.Mutex
	m  map[string]*proxyOwner
}

// ProxyOwners maps a download's /proxy uid to its profile.
var ProxyOwners = &proxyOwnerRegistry{m: map[string]*proxyOwner{}}

// proxyOwnerIdle is how long an unused entry lives (a job registers a new
// uid at every attempt).
const proxyOwnerIdle = 12 * time.Hour

func (r *proxyOwnerRegistry) Register(token, profileID string) {
	r.mu.Lock()
	r.m[token] = &proxyOwner{profileID: profileID, lastUsed: time.Now()}
	r.mu.Unlock()
}

// Profile returns token's profile and keeps the entry alive.
func (r *proxyOwnerRegistry) Profile(token string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.m[token]
	if o == nil {
		return "", false
	}
	o.lastUsed = time.Now()
	return o.profileID, true
}

func (r *proxyOwnerRegistry) cleanupStale() {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-proxyOwnerIdle)
	for t, o := range r.m {
		if o.lastUsed.Before(cutoff) {
			delete(r.m, t)
		}
	}
}

func init() {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			// Recover per tick with a deferred Unlock, so a panic can't wedge the lock.
			func() {
				defer core.Guard("stream/session-gc-tick")
				PlaybackLeases.cleanupStale()
				ProxyOwners.cleanupStale()
				Sessions.cleanupStale(20 * time.Minute)
			}()
		}
	}()
}

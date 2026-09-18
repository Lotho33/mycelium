package managers

import (
	"log"
	"sync"
	"time"

	"mycelium/internal/core"
)

// ─── Stream Semaphore ─────────────────────────────────────────────────────────
// Garantisce al massimo uno stream attivo per utente.
// Una nuova acquire() revoca automaticamente quella precedente.

type streamSemaphore struct {
	mu    sync.Mutex
	slots map[string]*streamSlot
}

type streamSlot struct {
	cancel    func()
	token     string
	createdAt time.Time
}

var StreamSem = &streamSemaphore{
	slots: make(map[string]*streamSlot),
}

func (s *streamSemaphore) Acquire(clientID, token string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	evicted := make(chan struct{})
	if prev, ok := s.slots[clientID]; ok {
		prev.cancel()
	}
	s.slots[clientID] = &streamSlot{
		cancel:    sync.OnceFunc(func() { close(evicted) }),
		token:     token,
		createdAt: time.Now(),
	}
	return evicted
}

func (s *streamSemaphore) Release(clientID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot, ok := s.slots[clientID]; ok && slot.token == token {
		delete(s.slots, clientID)
	}
}

func (s *streamSemaphore) cleanupStale() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-30 * time.Minute)
	for id, slot := range s.slots {
		if slot.createdAt.Before(cutoff) {
			slot.cancel()
			delete(s.slots, id)
		}
	}
}

// ─── Session Registry ─────────────────────────────────────────────────────────
// Traccia le sessioni di streaming attive per user ID.
// Ogni segmento HLS scaricato aggiorna la posizione stimata nel DB.

type StreamSession struct {
	UserID   string
	PluginID string
	SourceID string
	ParentID string
	IsLive   bool
	TotalSec float64 // 0 se non noto (live o non sondato)

	mu          sync.Mutex
	segDuration float64 // durata media segmento in secondi
	segsFetched int
	lastSeen    time.Time
}

// UpdateSegDuration aggiorna la durata media dei segmenti (da #EXTINF della playlist).
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

// RecordFetch incrementa il contatore segmenti e aggiorna il DB periodicamente.
func (s *StreamSession) RecordFetch() {
	s.mu.Lock()
	s.segsFetched++
	s.lastSeen = time.Now()
	if s.IsLive || s.segDuration == 0 || DB == nil {
		s.mu.Unlock()
		return
	}
	pos := float64(s.segsFetched) * s.segDuration
	interval := max(1, int(30.0/s.segDuration))
	if s.segsFetched%interval != 0 {
		s.mu.Unlock()
		return
	}
	snap := struct {
		userID, pluginID, sourceID, parentID string
		pos, total                           float64
	}{s.UserID, s.PluginID, s.SourceID, s.ParentID, pos, s.TotalSec}
	s.mu.Unlock()

	core.SafeGo("stream/progress-snapshot", func() {
		if err := DB.UpsertProgress(
			snap.userID, snap.pluginID, snap.sourceID,
			snap.parentID, "", "", "",
			snap.pos, snap.total,
			0, nil, "", 0,
		); err != nil {
			log.Printf("[stream/progress] %v", err)
		}
	})
}

type sessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*StreamSession
}

var Sessions = &sessionRegistry{
	sessions: make(map[string]*StreamSession),
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

func (r *sessionRegistry) Remove(token string) {
	r.mu.Lock()
	delete(r.sessions, token)
	r.mu.Unlock()
}

func init() {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			// One bad tick must not wedge Sessions.mu forever nor stop future
			// ticks — recover per-iteration, with a deferred Unlock so a panic
			// mid-loop can't leave the lock held.
			func() {
				defer core.Guard("stream/session-gc-tick")
				StreamSem.cleanupStale()
				cutoff := time.Now().Add(-20 * time.Minute)
				Sessions.mu.Lock()
				defer Sessions.mu.Unlock()
				for token, s := range Sessions.sessions {
					s.mu.Lock()
					stale := !s.lastSeen.IsZero() && s.lastSeen.Before(cutoff)
					s.mu.Unlock()
					if stale {
						delete(Sessions.sessions, token)
					}
				}
			}()
		}
	}()
}

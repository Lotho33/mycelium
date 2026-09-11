package pileus

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"mycelium/internal/engine"
	"mycelium/internal/managers"
)

// ─────────────────────────────────────────────────────────────────────────────
// Pre-buffer the head of an HLS stream during ResolveStream
// ─────────────────────────────────────────────────────────────────────────────
//
// Before ResolveStream returns {result}, mycelium fetches the media playlist
// and the first few media segments itself, into managers.Segments. Two payoffs:
//
//   1. It can emit real progress for the buffering phase — "Preparazione
//      flusso… 3/8 segmenti" — instead of the player showing a blind spinner
//      for the whole segment-download stretch (which is where the tens-of-
//      seconds wait actually goes: the proxy path, not mpv's demux buffer).
//   2. Those exact segments are then served warm from RAM when the player
//      requests them, so playback starts almost immediately after {result}.
//
// Trade-off: {result} is sent slightly later — after the pre-buffer target is
// met or prebuffer_max_wait_ms elapses — in exchange for an honest progress
// readout and a warm start. Keep the target small; it is per-deployment
// configurable and smaller for live than VOD.
//
// Progress contract kept intact for older Pileus builds: every event is a
// {progress} with status "loading" (one of the 4 known values; unknown values
// are treated as loading anyway) and a human-readable `message` the client
// renders verbatim. No proto change. See docs/pileus-contract.md for the proposed
// structured-percentage follow-up.

// prebufferConfig is resolved once per call from settings + env.
type prebufferConfig struct {
	enabled      bool
	segmentsVOD  int
	segmentsLive int
	maxWait      time.Duration
	maxBytes     int64
}

func loadPrebufferConfig() prebufferConfig {
	return prebufferConfig{
		enabled:      prebufBool("prebuffer_enabled", true),
		segmentsVOD:  prebufInt("prebuffer_segments_vod", 3),
		segmentsLive: prebufInt("prebuffer_segments_live", 1),
		maxWait:      time.Duration(prebufInt("prebuffer_max_wait_ms", 8000)) * time.Millisecond,
		maxBytes:     int64(prebufInt("prebuffer_max_bytes", 12_000_000)),
	}
}

// prebufferHLS runs the pre-buffer for an HLS resolve and narrates it through
// sendProgress. Best-effort: any failure just means no warm cache and the
// resolve proceeds — EXCEPT an interactive-verification response on the
// playlist, which it returns as an error (its message carries
// "CHALLENGE:<domain>") so the caller can fail the RPC fast instead of handing
// the player a stream whose every segment fetch fails. Honours ctx
// cancellation (user backs out) and its own
// max-wait cap so it can never hang the RPC.
func prebufferHLS(
	ctx context.Context,
	pluginID, rawURL string,
	headers map[string]string,
	isLive bool,
	sendProgress func(status, message string) error,
) error {
	if managers.PrefetchHLSHead == nil {
		return nil // internal/api not linked (unit tests) or hook not wired
	}
	cfg := loadPrebufferConfig()
	if !cfg.enabled {
		return nil
	}
	// Keep the shared warm-cache ceiling in sync with the (optional) knob.
	// Cheap: a mutex + a couple of comparisons, an eviction loop only when
	// the ceiling actually shrank.
	if mb := prebufInt("prebuffer_cache_mb", 0); mb > 0 {
		managers.SegmentCacheConfigure(mb<<20, 0)
	}
	target := cfg.segmentsVOD
	if isLive {
		target = cfg.segmentsLive
	}
	if target <= 0 {
		return nil
	}

	opts := managers.PrefetchOptions{
		Headers:     headers,
		UseVPN:      engine.LuaPlugins.ShouldUseVPNForVideo(pluginID),
		Egress:      engine.LuaPlugins.PluginEgress(pluginID),
		IsLive:      isLive,
		MaxSegments: target,
		MaxBytes:    cfg.maxBytes,
		MaxWait:     cfg.maxWait,
	}

	lastDone := -1
	res := managers.PrefetchHLSHead(ctx, rawURL, opts, func(p managers.PrefetchProgress) {
		if ctx.Err() != nil {
			return
		}
		switch p.Phase {
		case "done":
			// The {result} event that follows is the real "ready" signal —
			// don't emit a redundant "8/8" right before it.
			return
		case "playlist":
			_ = sendProgress("loading", "Preparazione flusso…")
		default: // "segment"
			if p.Done == lastDone {
				// A heartbeat that didn't advance the count — keep the stream
				// alive for the client's inactivity timer without spamming an
				// identical line.
				_ = sendProgress("loading", "Preparazione flusso… (attesa CDN)")
				return
			}
			lastDone = p.Done
			if p.Total > 0 {
				_ = sendProgress("loading", fmt.Sprintf("Preparazione flusso… %d/%d segmenti", p.Done, p.Total))
			} else {
				_ = sendProgress("loading", "Preparazione flusso…")
			}
		}
	})

	switch {
	case res.Err != nil:
		// A challenge is not "best-effort" territory: bubble it up so
		// ResolveStream fails fast with the actionable message.
		if _, isChallenge := challengeDomainFromErr(res.Err.Error()); isChallenge {
			log.Printf("[pileus/media] prebuffer %s: %v (challenge — failing resolve)", rawURL, res.Err)
			return res.Err
		}
		log.Printf("[pileus/media] prebuffer %s: %v (proceeding without warm cache)", rawURL, res.Err)
	case res.TimedOut:
		log.Printf("[pileus/media] prebuffer %s: cap hit at %d/%d segs (%d bytes) — sending result anyway",
			rawURL, res.Segments, target, res.Bytes)
	default:
		log.Printf("[pileus/media] prebuffer %s: %d segs, %d bytes warmed", rawURL, res.Segments, res.Bytes)
	}
	return nil
}

// ─── settings helpers (string | number JSON | env override) ─────────────────

func prebufInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv("MYCELIUM_" + strings.ToUpper(key))); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	switch v := managers.Settings.Get(key, nil).(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func prebufBool(key string, def bool) bool {
	get := func(s string) (bool, bool) {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		}
		return false, false
	}
	if b, ok := get(os.Getenv("MYCELIUM_" + strings.ToUpper(key))); ok {
		return b
	}
	switch v := managers.Settings.Get(key, nil).(type) {
	case bool:
		return v
	case string:
		if b, ok := get(v); ok {
			return b
		}
	case float64:
		return v != 0
	}
	return def
}

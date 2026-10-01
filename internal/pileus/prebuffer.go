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
// Before {result}, mycelium fetches the media playlist and the first
// segments into managers.Segments. It can then report real progress for the
// buffering phase ("… 3/8 segmenti"), and those segments are served warm
// when the player asks for them. {result} arrives slightly later (target
// met or prebuffer_max_wait_ms elapsed). Every event is a {progress} with
// status "loading" and a message the client shows as is.

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
// sendProgress. Best-effort (a failure just means no warm cache), honouring
// ctx and its own time cap.
func prebufferHLS(
	ctx context.Context,
	pluginID, rawURL string,
	headers map[string]string,
	isLive bool,
	startSec float64,
	sendProgress func(status, message string) error,
) error {
	if managers.PrefetchHLSHead == nil {
		return nil // hook not wired (unit tests)
	}
	cfg := loadPrebufferConfig()
	if !cfg.enabled {
		return nil
	}
	// Keep the warm-cache ceiling in sync with the setting.
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
		StartSec:    startSec,
	}

	lastDone := -1
	res := managers.PrefetchHLSHead(ctx, rawURL, opts, func(p managers.PrefetchProgress) {
		if ctx.Err() != nil {
			return
		}
		switch p.Phase {
		case "done":
			// {result} is the "ready" signal: no redundant final count before it.
			return
		case "playlist":
			_ = sendProgress("loading", "Preparazione flusso…")
		default: // "segment"
			if p.Done == lastDone {
				// A heartbeat without progress: keep the stream alive without repeating the
				// same line.
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
		log.Printf("[pileus/media] prebuffer %s: %v (proceeding without warm cache)", upstreamHost(rawURL), res.Err)
	case res.TimedOut:
		log.Printf("[pileus/media] prebuffer %s: cap hit at %d/%d segs (%d bytes) — sending result anyway",
			upstreamHost(rawURL), res.Segments, target, res.Bytes)
	default:
		log.Printf("[pileus/media] prebuffer %s: %d segs, %d bytes warmed", upstreamHost(rawURL), res.Segments, res.Bytes)
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

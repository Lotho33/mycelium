package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mycelium/internal/core"
	"mycelium/internal/managers"

	"github.com/robfig/cron/v3"
	lua "github.com/yuin/gopher-lua"
	"gopkg.in/yaml.v3"
)

// ─────────────────────────────────────────────────────────────────────────────
// LuaManifest — manifest.yaml schema
// ─────────────────────────────────────────────────────────────────────────────

type LuaManifest struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Author      string `yaml:"author"`
	Description string `yaml:"description"`
	Icon        string `yaml:"icon"`
	// PoolSize controls how many concurrent Lua VMs are kept for this plugin.
	// Defaults to 2. Increase only for plugins with high parallel traffic.
	PoolSize int `yaml:"pool_size"`
	// DirectEgress opts this plugin OUT of the default VPN routing: its network
	// calls use the server's own connection instead of the configured VPN/proxy.
	// The default (field omitted / false) is the safe one — EVERY network call
	// of the plugin (scraping, headless browser, and the downstream HLS
	// playlist/segment/key relay) is forced through the VPN and fails closed if
	// none is configured, so the real egress IP can never leak by forgetting a
	// flag. Set this only for sources that must be reached directly: a media
	// server on the operator's own LAN (jellyfin/plex), or a site that actively
	// blocks the VPN's exit IP.
	DirectEgress bool `yaml:"direct_egress"`
	// VPNOptional lets the operator route just this plugin's video flow
	// (resolve/streams entrypoints + the downstream playlist/segment/key relay)
	// through the VPN on an admin toggle, even though the plugin is DirectEgress.
	// readLuaManifest injects a synthetic "vpn_enabled" bool setting (off by
	// default) for the admin UI. Only meaningful together with direct_egress:
	// true — without it the VPN already covers every call.
	VPNOptional bool `yaml:"vpn_optional"`
	// DirectStream declares that this plugin's resolved stream URL should be
	// handed to the player as-is, skipping mycelium's own HLS/segment proxy
	// entirely (see internal/pileus/media_handler.go ResolveStream). Meant for
	// sources that need none of what the proxy exists for — hiding upstream
	// cookies/tokens from a browser player, bypassing CORS, rewriting HLS
	// sub-URLs — and are hurt by what it costs: getSegmentClient's non-HLS
	// client (internal/api/proxy.go) caps the WHOLE request (not just connect)
	// at 60s, which silently truncates a large direct-play file partway
	// through. jellyfin is the first case: a personal server on the same
	// private network as the player, plain HTTP with the api_key already in
	// the query string, and native Range support the player can use directly.
	DirectStream bool                `yaml:"direct_stream"`
	Settings     LuaManifestSettings `yaml:"settings"`
	Exposes      LuaManifestExposes  `yaml:"exposes"`
	Entrypoints  map[string]string   `yaml:"entrypoints"`
	Tasks        []LuaManifestTask   `yaml:"tasks"`
}

// LuaManifestSettings used to also carry a User list — per-profile settings
// Pileus could read/write via gRPC (GetPluginSettings/SavePluginSetting).
// Removed: zero shipped plugins ever declared one, so it was schema, two
// RPCs, a Redis namespace and a Lua SDK accessor with no plugin behind them.
// Every setting is admin-configured now, from mycelium's own dashboard —
// Pileus stays a player, not a second settings UI.
type LuaManifestSettings struct {
	Global []LuaSettingField `yaml:"global"`
}

type LuaSettingField struct {
	ID       string `yaml:"id"`
	Label    string `yaml:"label"`
	Type     string `yaml:"type"` // "string" | "password" | "number" | "bool"
	Hidden   bool   `yaml:"hidden"`
	Required bool   `yaml:"required"`
}

type LuaManifestExposes struct {
	Capabilities []string        `yaml:"capabilities"`
	Catalogs     []LuaCatalogDef `yaml:"catalogs"`
}

type LuaCatalogDef struct {
	ID              string `yaml:"id"                  json:"id"`
	Name            string `yaml:"name"                json:"name"`
	Type            string `yaml:"type"                json:"type"` // "movie" | "series" | "live" | "music" | "audiobook" | "vod_clip"
	CacheTTLSeconds int32  `yaml:"cache_ttl_seconds"   json:"cache_ttl_seconds"`
	// AutoHideWhenEmpty: if true, ListPlugins omits this catalog when the last
	// fetched page-1 result had 0 items. Useful for time-sensitive catalogs
	// (e.g. "live_now") that should not appear when empty.
	AutoHideWhenEmpty bool `yaml:"auto_hide_when_empty" json:"auto_hide_when_empty"`
	// SectionKind: "carousel"|"grid", hint for how Pileus lays out this catalog
	// on the home screen. Empty ("") means unset — client falls back to its
	// default (carousel). See gen.CatalogDef.SectionKind for the forward-compat
	// contract (unrecognized values must be treated as unset, never break).
	SectionKind string `yaml:"section_kind" json:"section_kind"`
	// StyleHint: free-form presentation hint within SectionKind (e.g. "featured"|"compact").
	StyleHint string `yaml:"style_hint" json:"style_hint"`
	// CardLayout: card aspect ratio for this catalog's row, independent of
	// Type — "" (poster, 2:3, default) | "landscape" (16:9). Before this
	// field existed, the only way to get wide cards was Type == "live",
	// which also forces live-TV row styling and disables continue-watching
	// plugin-wide. See gen.CatalogDef.CardLayout for the full contract.
	CardLayout string `yaml:"card_layout" json:"card_layout"`
	// DisableHeroBackground: skip this catalog's fanart/poster in the home
	// screen's full-bleed hero — always show the neutral/anonymous fallback
	// instead. See gen.CatalogDef.DisableHeroBackground for the full contract.
	DisableHeroBackground bool `yaml:"disable_hero_background" json:"disable_hero_background"`
}

type LuaManifestTask struct {
	Function string `yaml:"function"`
	// Cron: "@every 12h" | standard cron expression | "" (vuoto/omesso).
	// Vuoto = task MANUAL-ONLY: compare comunque nella dashboard con un
	// pulsante "Esegui" (POST /admin/lua-plugins/run-task/...), ma non
	// parte mai da solo — né al boot né a intervalli. Usalo per audit/
	// repair one-shot che non devono mai auto-schedularsi (prima l'unico
	// modo era un cron finto lunghissimo tipo "@every 87600h", che però
	// NON impediva la partenza automatica al boot).
	Cron string `yaml:"cron"`
	// LiveRefresh marks a task Pileus may trigger on demand (PluginService.
	// TriggerRefresh), in addition to its normal Cron schedule — e.g. a
	// live-scores refresh task, so a user watching a "Live now" row isn't stuck
	// waiting for the next @every 2m tick. Surfaced to the client as
	// CatalogDef.live_refreshable.
	LiveRefresh bool `yaml:"live_refresh"`
	// TimeoutSec overrides the default 90s entrypoint budget (see
	// defaultEntrypointTimeout) for THIS task (cron or manual "Esegui"). For
	// legitimately long one-shots like a full catalog scrape that can't
	// finish in 90s. Capped at 15 min. 0 = default.
	TimeoutSec int `yaml:"timeout_seconds"`
}

// Entrypoint name constants matching the MediaPipeline gRPC methods.
const (
	EPGetCatalog       = "catalog"
	EPGetCatalogList   = "catalog_list"
	EPSearch           = "search"
	EPGetSearchFilters = "search_filters"
	EPGetDetails       = "details"
	EPBrowse           = "browse"
	EPGetStreams       = "streams"
	EPResolveStream    = "resolve"
)

// isVideoFlowEntrypoint reports whether ep is one that touches the actual video
// CDN — listing sources (streams) or resolving the final playable URL
// (resolve). For a direct_egress + vpn_optional plugin these are the ONLY
// entrypoints that follow the selected egress; catalog/search/details stay
// direct. Keeping the resolve sniff on the same egress as the HLS segment
// proxy and the operator's interactive-verification step is what makes the
// "verify once, then it just works" flow work: the browser service's
// per-(domain, egress) cookie jar is only reused by a sniff running under the
// same egress.
func isVideoFlowEntrypoint(ep string) bool {
	return ep == EPResolveStream || ep == EPGetStreams
}

// ─────────────────────────────────────────────────────────────────────────────
// PluginStatus — runtime state of a Lua plugin, exposed to Pileus via ListPlugins.
// ─────────────────────────────────────────────────────────────────────────────

// StatusLabel constants used by plugins and the core.
const (
	StatusReady       = "ready"
	StatusSyncing     = "syncing"
	StatusNeedsConfig = "needs_config"
	StatusError       = "error"
)

type PluginStatus struct {
	Label  string `json:"label"`  // StatusReady | StatusSyncing | StatusNeedsConfig | StatusError
	Detail string `json:"detail"` // human-readable, e.g. "Indicizzazione catalogo (1200/5000)"
}

// pluginStatusKey returns the Redis key for a plugin's runtime status.
func pluginStatusKey(pluginID string) string {
	return "mycelium:plugin:" + pluginID + ":status"
}

// ─────────────────────────────────────────────────────────────────────────────
// PluginHealth — reachability derived automatically from task outcomes.
//
// Unlike PluginStatus (opt-in: only meaningful for plugins that actually call
// set_plugin_status), this requires nothing from the plugin author — every
// task run, cron or manual, updates it. A user asking
// "is the plugin actually working" shouldn't have to read logs to find out;
// reachableFailureThreshold consecutive failures (not just one, to avoid
// flapping the indicator on a single transient network blip) flips it false
// until the next success.
// ─────────────────────────────────────────────────────────────────────────────

const reachableFailureThreshold = 2

type pluginHealth struct {
	LastOkUnix          int64 `json:"last_ok_unix"`
	ConsecutiveFailures int   `json:"consecutive_failures"`
}

func pluginHealthKey(pluginID string) string {
	return "mycelium:plugin:" + pluginID + ":health"
}

func (m *LuaPluginManager) getHealth(pluginID string) pluginHealth {
	if managers.Redis == nil {
		return pluginHealth{}
	}
	raw, err := managers.Redis.Get(context.Background(), pluginHealthKey(pluginID))
	if err != nil || raw == "" {
		return pluginHealth{}
	}
	var h pluginHealth
	_ = json.Unmarshal([]byte(raw), &h)
	return h
}

// RecordTaskResult updates pluginID's health after a task run (cron or
// manual). Called from runTasks' run() closure, the admin dashboard's manual
// run-task handler, and TriggerLiveRefresh — all three already serialize
// task execution per plugin via taskMu/bgTaskSem, so the read-modify-write
// here doesn't need its own lock.
func (m *LuaPluginManager) RecordTaskResult(pluginID string, ok bool) {
	if managers.Redis == nil {
		return
	}
	h := m.getHealth(pluginID)
	if ok {
		h.LastOkUnix = time.Now().Unix()
		h.ConsecutiveFailures = 0
	} else {
		h.ConsecutiveFailures++
	}
	b, err := json.Marshal(h)
	if err != nil {
		return
	}
	_ = managers.Redis.Set(context.Background(), pluginHealthKey(pluginID), string(b), 0)
}

// GetReachability reports whether pluginID is currently considered reachable
// and when it last succeeded. Optimistic default (reachable=true, 0) when no
// task has run yet — matches GetStatus's "ready until proven otherwise".
func (m *LuaPluginManager) GetReachability(pluginID string) (reachable bool, lastOkUnix int64) {
	h := m.getHealth(pluginID)
	return h.ConsecutiveFailures < reachableFailureThreshold, h.LastOkUnix
}

// ─────────────────────────────────────────────────────────────────────────────
// RunState — operational lifecycle of a Lua plugin, driven from the admin
// dashboard: a plugin is idle until its required settings are filled and it is
// explicitly started. Only a `running` plugin schedules cron tasks, serves
// entrypoints, or appears in ListPlugins.
// ─────────────────────────────────────────────────────────────────────────────

const (
	RunStateWaiting = "waiting" // one or more required settings unset
	RunStateStopped = "stopped" // manually stopped, or never started
	RunStateRunning = "running"
)

func pluginDisabledKey(pluginID string) string { return "plugin_" + pluginID + "_disabled" }

// MissingRequiredSettings returns the ids of this plugin's manifest settings
// marked `required: true` that currently have no stored value.
func (m *LuaPluginManager) MissingRequiredSettings(pluginID string) []string {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	var missing []string
	for _, f := range p.Manifest.Settings.Global {
		if !f.Required {
			continue
		}
		if managers.Settings.GetString("lua:"+pluginID+":global:"+f.ID, "") == "" {
			missing = append(missing, f.ID)
		}
	}
	return missing
}

// hasRequiredSettings reports whether the manifest declares any required field.
func (m *LuaPluginManager) hasRequiredSettings(pluginID string) bool {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	for _, f := range p.Manifest.Settings.Global {
		if f.Required {
			return true
		}
	}
	return false
}

// RunStateOf computes the current lifecycle state. Precedence: waiting (can't
// run) > stopped > running. A plugin that declares required settings and has
// never been explicitly started (no stored disabled flag) counts as stopped,
// so filling its config does not silently auto-start it.
func (m *LuaPluginManager) RunStateOf(pluginID string) string {
	if len(m.MissingRequiredSettings(pluginID)) > 0 {
		return RunStateWaiting
	}
	switch managers.Settings.GetString(pluginDisabledKey(pluginID), "") {
	case "true":
		return RunStateStopped
	case "false":
		return RunStateRunning
	default: // never set
		if m.hasRequiredSettings(pluginID) {
			return RunStateStopped
		}
		return RunStateRunning
	}
}

// IsOperational reports whether the plugin should schedule tasks, serve
// entrypoints and appear in ListPlugins.
func (m *LuaPluginManager) IsOperational(pluginID string) bool {
	return m.RunStateOf(pluginID) == RunStateRunning
}

// SetRunEnabled flips the stored start/stop flag. It does not itself schedule
// or unschedule anything — the cron scheduler and entrypoint gates re-check
// IsOperational on every tick/call, so the change takes effect immediately.
func (m *LuaPluginManager) SetRunEnabled(pluginID string, enabled bool) {
	_ = managers.Settings.Save(map[string]any{
		pluginDisabledKey(pluginID): map[bool]string{true: "false", false: "true"}[enabled],
	})
}

// TriggerLiveRefresh runs pluginID's live_refresh-marked task(s) right now,
// instead of waiting for their cron schedule (Pileus-facing "aggiorna ora").
// Non-blocking: returns immediately, same fire-and-forget pattern as the
// admin dashboard's manual "Esegui" button, gated by the same bgTaskSem so a
// user mashing refresh can't pile up concurrent runs.
func (m *LuaPluginManager) TriggerLiveRefresh(pluginID string) (ok bool, message string) {
	m.mu.RLock()
	p, found := m.plugins[pluginID]
	m.mu.RUnlock()
	if !found {
		return false, "plugin non caricato"
	}
	// Same guard as run()/warmupCatalogCounts/lookupBackend/resolveCatalogDefs:
	// unlike the cron scheduler (which re-checks IsOperational per tick before
	// running a task), this entrypoint is reachable on-demand from Pileus
	// ("aggiorna ora" pull-to-refresh) — without this check a stopped plugin
	// still ran its live_refresh task for real on request. No bundled plugin
	// declares live_refresh today, so this was a dormant gap rather than an
	// observed one.
	if !m.IsOperational(pluginID) {
		return false, "plugin non attivo"
	}

	var tasks []LuaManifestTask
	for _, t := range p.Manifest.Tasks {
		if t.LiveRefresh {
			tasks = append(tasks, t)
		}
	}
	if len(tasks) == 0 {
		return false, "il plugin non supporta l'aggiornamento manuale"
	}

	if !TryAcquireBgTaskSem() {
		return false, "un altro task è già in corso, riprova tra poco"
	}
	// bgTaskSem da solo limita solo il TOTALE di task concorrenti (capacità
	// 2): non basta a impedire che questo refresh manuale parta mentre un
	// tick cron dello STESSO plugin è già a metà — taskMu è il lock che
	// esiste apposta per quello (vedi il commento sul campo in LuaPlugin).
	// TryLock invece di Lock: se il plugin ha già un task in corso, meglio
	// dirlo subito al chiamante che tenere occupato uno slot di bgTaskSem in
	// attesa silenziosa.
	if !p.taskMu.TryLock() {
		ReleaseBgTaskSem()
		return false, "un task di questo plugin è già in corso, riprova tra poco"
	}

	go func() {
		defer ReleaseBgTaskSem()
		defer p.taskMu.Unlock()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[lua] live refresh %s: PANIC recuperato: %v", pluginID, r)
			}
		}()
		for _, t := range tasks {
			_, err := m.CallEntrypoint(pluginID, t.Function, nil, "")
			m.RecordTaskResult(pluginID, err == nil)
			if err != nil {
				log.Printf("[lua] live refresh %s/%s: %v", pluginID, t.Function, err)
			} else {
				log.Printf("[lua] live refresh %s/%s: OK", pluginID, t.Function)
				MarkTaskRan(pluginID, t.Function)
			}
		}
	}()
	return true, "aggiornamento avviato"
}

// ─────────────────────────────────────────────────────────────────────────────
// LuaPlugin — single loaded plugin
// ─────────────────────────────────────────────────────────────────────────────

type LuaPlugin struct {
	Manifest LuaManifest
	Dir      string
	Pool     *LuaPool
	LogBuf   *managers.PluginLogBuffer

	stopHR   chan struct{}
	cronStop chan struct{}
	taskMu   sync.Mutex // serializza i task dello stesso plugin: uno alla volta

	// discardedStates counts how many times an entrypoint call to this
	// plugin has timed out and forced its Lua state to be discarded and
	// replaced (see callWithTimeout / LuaPool.DiscardAndReplace). A single
	// occurrence is an expected, isolated event by design — but nothing else
	// tracked it, so a plugin whose cron task always times out (a hung
	// upstream, a degenerate loop) leaked one goroutine + one *lua.LState per
	// tick forever with zero visible signal short of the process eventually
	// running out of memory. Incremented via recordDiscard, read by
	// DiscardedStates (exposed to the admin dashboard) — atomic because
	// entrypoint calls for the same plugin can run concurrently (different
	// pooled LStates) from different request goroutines.
	discardedStates atomic.Uint64
}

// discardWarnEvery: log a warning every this-many timeout-discards for a
// plugin, so an unbounded stream of them (see discardedStates) leaves a
// trace in the raw logs even before anyone looks at the dashboard counter.
const discardWarnEvery = 5

// recordDiscard increments discardedStates and, every discardWarnEvery
// occurrences, logs a warning — see the field's doc comment for why this
// exists. Intentionally does NOT disable the plugin or its task: an
// unbounded counter is a strong signal something is stuck, but auto-disabling
// risks taking down a plugin that is only having a slow moment, and that
// decision needs a human, not a heuristic here.
func (p *LuaPlugin) recordDiscard() {
	n := p.discardedStates.Add(1)
	if n%discardWarnEvery == 0 {
		log.Printf("[lua] plugin %s: %d stati Lua scartati per timeout finora — possibile entrypoint/task bloccato, verificare i log del plugin", p.Manifest.ID, n)
	}
}

func (p *LuaPlugin) scriptPath() string {
	return filepath.Join(p.Dir, "init.lua")
}

// IconPath returns the absolute path to a plugin's branding icon, declared as
// `icon:` in its manifest (a path relative to the plugin dir). ok is false when
// the plugin is unknown, declares no icon, the file is missing, or the path
// tries to climb out of the plugin dir. Served to clients by
// internal/api/plugin_icon.go so Pileus can show it in the side menu.
func (m *LuaPluginManager) IconPath(pluginID string) (string, bool) {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok || p.Manifest.Icon == "" {
		return "", false
	}
	clean := filepath.Clean(p.Manifest.Icon)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	full := filepath.Join(p.Dir, clean)
	// Defense in depth: the resolved path must still sit under the plugin dir.
	if rel, err := filepath.Rel(p.Dir, full); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		return "", false
	}
	return full, true
}

// ─────────────────────────────────────────────────────────────────────────────
// LuaPluginManager
// ─────────────────────────────────────────────────────────────────────────────

var LuaPlugins = &LuaPluginManager{
	plugins: make(map[string]*LuaPlugin),
}

type LuaPluginManager struct {
	plugins      map[string]*LuaPlugin
	mu           sync.RWMutex
	pluginsRoot  string
	proxyAddr    string
	directClient *http.Client // uTLS, no proxy — always used for catalog/API calls
	proxyClient  *http.Client // VPN/proxy — the default egress; skipped only for direct_egress plugins (or _proxy=false)

	// proxyClients caches one scraping client per egress proxy URL, so a
	// plugin routed through a named egress (managers.EgressProfiles) reuses
	// connections instead of rebuilding a client per call.
	proxyClients   map[string]*http.Client
	proxyClientsMu sync.Mutex
}

// ProxyClientFor returns a cached scraping http.Client bound to proxyURL
// (empty → nil, the caller then uses the direct client / fails closed).
func (m *LuaPluginManager) ProxyClientFor(proxyURL string) *http.Client {
	if proxyURL == "" {
		return nil
	}
	m.proxyClientsMu.Lock()
	defer m.proxyClientsMu.Unlock()
	if m.proxyClients == nil {
		m.proxyClients = map[string]*http.Client{}
	}
	if c := m.proxyClients[proxyURL]; c != nil {
		return c
	}
	c := NewScrapingClient(proxyURL)
	m.proxyClients[proxyURL] = c
	return c
}

func (m *LuaPluginManager) SetProxyAddr(addr string) {
	m.proxyAddr = addr
	if addr != "" {
		m.proxyClient = NewScrapingClient(addr)
	} else {
		m.proxyClient = nil
	}
	// Reload all active pools so existing VMs pick up the new proxy client.
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, p := range m.plugins {
		if err := p.Pool.Reload(); err != nil {
			log.Printf("[lua] SetProxyAddr: reload %s failed: %v", id, err)
		}
	}
}

func (m *LuaPluginManager) GetProxyAddr() string {
	return m.proxyAddr
}

// UsesVPN reports whether the plugin with the given ID routes its traffic
// through the VPN proxy — the default for every plugin unless its manifest
// declares direct_egress: true. Returns false for unknown IDs.
func (m *LuaPluginManager) UsesVPN(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.plugins[id]
	return ok && !p.Manifest.DirectEgress
}

func (m *LuaPluginManager) LoadAll(dir string) error {
	m.pluginsRoot = dir
	if m.directClient == nil {
		m.directClient = NewScrapingClient("")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "shared" {
			continue
		}
		if err := m.loadPlugin(filepath.Join(dir, e.Name())); err != nil {
			log.Printf("[lua] plugin %s failed to load: %v", e.Name(), err)
		}
	}
	log.Printf("[lua] plugins loaded: %d", m.count())
	return nil
}

// maxPoolSize caps how many concurrent Lua VMs a single plugin's manifest may
// request via pool_size. NewLuaPool creates `size` *lua.LState instances
// synchronously and sequentially at load time (boot, or an admin ZIP upload —
// so the manifest is not necessarily operator-authored/reviewed), each
// re-reading the script from disk and executing its top level. Without an
// upper bound a manifest alone — no plugin action required — can stall server
// startup or exhaust memory just by declaring an absurd pool_size.
const maxPoolSize = 16

func (m *LuaPluginManager) loadPlugin(dir string) error {
	mf, err := readLuaManifest(dir)
	if err != nil {
		return err
	}
	if mf.ID == "" {
		return fmt.Errorf("manifest.yaml missing 'id' in %s", dir)
	}
	if !mf.DirectEgress && m.proxyClient == nil {
		log.Printf("[lua] plugin %s routes through the VPN by default but no VPN proxy is configured — its network calls will fail closed until one is set (or add direct_egress: true to its manifest)", mf.ID)
	}
	if mf.VPNOptional && !mf.DirectEgress {
		log.Printf("[lua] plugin %s sets vpn_optional without direct_egress — ignored, the VPN already covers every call", mf.ID)
	}
	scriptPath := filepath.Join(dir, "init.lua")
	if _, err := os.Stat(scriptPath); err != nil {
		return fmt.Errorf("init.lua not found in %s", dir)
	}

	logBuf := managers.NewPluginLogBuffer(mf.ID, 300)

	poolSize := mf.PoolSize
	if poolSize <= 0 {
		poolSize = 2
	}
	if poolSize > maxPoolSize {
		log.Printf("[lua] plugin %s: pool_size %d exceeds the max (%d), clamped", mf.ID, poolSize, maxPoolSize)
		poolSize = maxPoolSize
	}
	var sharedDirs []string
	if m.pluginsRoot != "" {
		sd := filepath.Join(m.pluginsRoot, "shared")
		if info, err := os.Stat(sd); err == nil && info.IsDir() {
			sharedDirs = []string{sd}
		}
	}
	pool, err := NewLuaPool(poolSize, scriptPath, sharedDirs, func(L *lua.LState) {
		direct := m.directClient
		if direct == nil {
			direct = http.DefaultClient
		}
		RegisterSDK(L, SDKOpts{
			PluginID:        mf.ID,
			PluginDir:       dir,
			Redis:           managers.Redis,
			Browser:         currentBrowserClient(),
			LogBuf:          logBuf,
			HTTPClient:      direct,
			ProxyHTTPClient: m.proxyClient,
			ProxyURL:        m.proxyAddr,
			ProxyClientFor:  m.ProxyClientFor,
		}, scopeOf(L))
	})
	if err != nil {
		return fmt.Errorf("lua pool for %s: %w", mf.ID, err)
	}

	p := &LuaPlugin{
		Manifest: mf,
		Dir:      dir,
		Pool:     pool,
		LogBuf:   logBuf,
		stopHR:   make(chan struct{}),
		cronStop: make(chan struct{}),
	}

	m.mu.Lock()
	if old, ok := m.plugins[mf.ID]; ok && old.Dir != dir {
		// Un plugin caricato da una directory diversa non può dichiarare
		// l'id di uno già caricato: altrimenti dirotterebbe la sua identità
		// (cache Redis, secrets per-profilo, settings — tutti keyed solo su
		// PluginID) semplicemente scrivendo lo stesso id nel proprio
		// manifest.yaml. Un reload dalla STESSA directory (hot-swap/update
		// legittimo) resta permesso.
		m.mu.Unlock()
		pool.Close()
		return fmt.Errorf("plugin id %q già in uso da un'altra directory (%s)", mf.ID, old.Dir)
	}
	if old, ok := m.plugins[mf.ID]; ok {
		close(old.stopHR)
		close(old.cronStop)
		old.Pool.Close()
	}
	m.plugins[mf.ID] = p
	m.mu.Unlock()

	core.SafeGo("lua/watch-hot-reload:"+mf.ID, func() { m.watchHotReload(p) })
	core.SafeGo("lua/run-tasks:"+mf.ID, func() { m.runTasks(p) })
	// Populate auto_hide counts immediately on load using whatever is already
	// in the matches cache (warm on restart; re-runs after first task anyway).
	// warmupCatalogCounts already recovers its own panics (see its body).
	go m.warmupCatalogCounts(p)
	log.Printf("[lua] plugin %q loaded (v%s)", mf.ID, mf.Version)
	return nil
}

// watchHotReloadPolling is a fallback hot-reload watcher using mtime polling.
// Used by lua_hotreload.go when fsnotify is unavailable.
func (m *LuaPluginManager) watchHotReloadPolling(p *LuaPlugin) {
	path := p.scriptPath()
	lastMod := fileMod(path)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopHR:
			return
		case <-ticker.C:
			mod := fileMod(path)
			if mod != lastMod {
				lastMod = mod
				// Same reasoning as the fsnotify path in lua_hotreload.go: a
				// bad reload must not kill hot-reload for this plugin forever.
				func() {
					defer core.Guard("lua/hot-reload-poll:" + p.Manifest.ID)
					if err := p.Pool.Reload(); err != nil {
						log.Printf("[lua] hot-reload %s: %v", p.Manifest.ID, err)
					} else {
						log.Printf("[lua] hot-reload %s: OK", p.Manifest.ID)
					}
				}()
			}
		}
	}
}

// taskLastRunKey returns the Redis key used to persist the last-run timestamp of a task.
func taskLastRunKey(pluginID, fn string) string {
	return "mycelium:task:lastrun:" + pluginID + ":" + fn
}

// MarkTaskRan records that a task just ran, so the startup catch-up logic in
// runTasks (shouldRunNow) doesn't treat it as overdue and fire it again on
// the next restart. Only the cron scheduler's own run() wrote this before —
// a manual dashboard trigger (POST /admin/lua-plugins/run-task/...) never
// did, so any task registered with a long "manual-only" cron (the
// convention used for one-off tasks like resync_all_nav so they still show
// up in the dashboard) would re-fire on every single server restart, since
// shouldRunNow treats "no last-run record" as "overdue" — confirmed live
// (2026-08-16): resync_all_nav kept auto-launching on every restart despite
// its 10-year cron, purely because manual runs left no record behind.
func MarkTaskRan(pluginID, fn string) {
	if managers.Redis == nil {
		return
	}
	// Una run tecnicamente riuscita (nessun errore Lua restituito) ma che il
	// plugin stesso ha marcato come fallita — set_plugin_status("error", …) —
	// non deve contare come "eseguito di recente": senza questo un fetch
	// andato a vuoto (rete KO, endpoint irraggiungibile, CSRF) blocca ogni
	// retry fino al prossimo tick cron, anche 12h. Caso reale: un
	// refresh_catalog girato durante la finestra VPN fail-closed scriveva il
	// marker e bloccava il fetch vero.
	if LuaPlugins != nil && LuaPlugins.GetStatus(pluginID).Label == StatusError {
		log.Printf("[lua] task %s/%s: stato plugin = error, marker last-run NON scritto (riproverà al prossimo avvio)", pluginID, fn)
		return
	}
	_ = managers.Redis.Set(context.Background(), taskLastRunKey(pluginID, fn), fmt.Sprintf("%d", time.Now().Unix()), 0)
}

// cronInterval parses "@every <duration>" and returns the duration, or 0 if unparseable.
func cronInterval(spec string) time.Duration {
	var d time.Duration
	if _, err := fmt.Sscanf(spec, "@every %s", new(string)); err == nil {
		// robfig/cron parses @every internally; we replicate it manually.
		var s string
		fmt.Sscanf(spec, "@every %s", &s)
		if parsed, err := time.ParseDuration(s); err == nil {
			d = parsed
		}
	}
	return d
}

// runTasks schedules cron tasks declared in the manifest.
// bgTaskSem serializza i task in background di TUTTI i plugin (a livello di
// processo). Senza questo, se più task risultano scaduti insieme (es. al
// boot, dopo che il servizio è stato fermo per ore), ognuno parte come
// goroutine indipendente e i sync di catalogo (che possono indicizzare
// decine di migliaia di elementi) girano tutti in parallelo, facendo
// esplodere temporaneamente la RSS ben oltre il fabbisogno reale.
// Capacità 2 (non più 1): con capacità 1 un singolo sync pesante (un rebuild
// completo di catalogo, 30+ minuti) affamava ogni altro plugin per tutta la
// sua durata — un refresh "live" leggero restava bloccato in coda per l'intera
// esecuzione. 2 lascia passare un secondo task leggero in parallelo senza
// tornare al problema originale (N task pesanti tutti insieme) — non è una
// soglia esatta, è un compromesso pragmatico tra i due estremi.
var bgTaskSem = make(chan struct{}, 2)

// TryAcquireBgTaskSem tenta di prendere uno slot di bgTaskSem senza bloccare.
// Usato dal trigger manuale (POST /admin/lua-plugins/run-task/...): prima
// bypassava del tutto il semaforo, potendo girare in parallelo con QUALSIASI
// numero di task schedulati pesanti — esattamente ciò per cui bgTaskSem
// esiste per evitare. Non blocca MAI in attesa: un click "Esegui" dalla
// dashboard deve dare un responso immediato (occupato o partito), non
// restare appeso in silenzio per la durata di un sync pesante altrui.
// Se true, il chiamante DEVE chiamare ReleaseBgTaskSem() quando finito
// (anche in caso di panic — usare defer).
func TryAcquireBgTaskSem() bool {
	select {
	case bgTaskSem <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseBgTaskSem rilascia lo slot preso con TryAcquireBgTaskSem.
func ReleaseBgTaskSem() {
	<-bgTaskSem
}

// TryAcquirePluginTaskLock tenta di prendere il taskMu di pluginID senza
// bloccare (come TryAcquireBgTaskSem, ma a livello del singolo plugin
// invece che globale). runTasks e TriggerLiveRefresh acquisiscono già
// entrambi taskMu prima di eseguire un task — runLuaTask (il pulsante
// "Esegui" manuale in admin, POST /admin/lua-plugins/run-task/...)
// acquisiva solo bgTaskSem: con capacità 2, un click manuale poteva
// partire mentre un tick cron dello STESSO plugin era già a metà,
// eseguendo lo stesso task Lua (es. sync_full_catalog) due volte in
// parallelo su LState diversi del pool — bgTaskSem da solo non impedisce
// concorrenza per lo STESSO plugin, solo il totale a livello di processo.
// Ritorna una funzione di rilascio e true se acquisito; il chiamante deve
// chiamarla (anche in caso di panic — usare defer) solo se ok è true.
func (m *LuaPluginManager) TryAcquirePluginTaskLock(pluginID string) (release func(), ok bool) {
	m.mu.RLock()
	p, found := m.plugins[pluginID]
	m.mu.RUnlock()
	if !found {
		return nil, false
	}
	if !p.taskMu.TryLock() {
		return nil, false
	}
	return p.taskMu.Unlock, true
}

// At startup each task runs immediately only if it has not run within its cron interval.
// taskDue reports whether t is due to run right now: no record of a prior
// run, or its cron interval has elapsed. cron == "" marks a manual-only
// task — compare comunque nella dashboard (con un pulsante "Esegui") ma non
// deve MAI auto-partire, né al boot né a intervalli — prima di questo,
// l'unico modo per registrare un task manuale-nella-dashboard era dargli un
// cron finto lunghissimo (es. "@every 87600h", 10 anni), che però NON
// impediva la partenza automatica al boot: la stessa logica trattava
// "nessun record di ultima esecuzione" come "scaduto", quindi un task
// manuale-solo-dashboard ripartiva da solo a ogni riavvio del server se non
// era mai stato lanciato manualmente prima.
func (m *LuaPluginManager) taskDue(ctx context.Context, pluginID string, t LuaManifestTask) bool {
	if t.Cron == "" {
		return false
	}
	interval := cronInterval(t.Cron)
	if interval <= 0 || managers.Redis == nil {
		return true
	}
	val, err := managers.Redis.Get(ctx, taskLastRunKey(pluginID, t.Function))
	if err != nil || val == "" {
		return true
	}
	var lastRun int64
	if _, err := fmt.Sscanf(val, "%d", &lastRun); err != nil {
		return true
	}
	return time.Since(time.Unix(lastRun, 0)) >= interval
}

// runTaskNow executes t for real, guarded: only a `running` plugin (started
// + all required settings filled) actually runs — callers (the cron ticker,
// the initial per-plugin pass, RunDueTasksNow) may all race a stop/settings
// change, so this is re-checked here every time, not trusted from the
// caller.
func (m *LuaPluginManager) runTaskNow(p *LuaPlugin, t LuaManifestTask) {
	if !m.IsOperational(p.Manifest.ID) {
		log.Printf("[lua] task %s/%s: skip (plugin non attivo: %s)", p.Manifest.ID, t.Function, m.RunStateOf(p.Manifest.ID))
		return
	}
	bgTaskSem <- struct{}{}
	defer func() { <-bgTaskSem }()
	p.taskMu.Lock()
	defer p.taskMu.Unlock()
	// Un panic qui (Go-level, non un semplice errore Lua — quelli sono
	// già un ritorno normale via err) prima non era recuperato: questa
	// funzione gira in una goroutine nuda (go m.runTaskNow(p, t)), quindi
	// un panic non recuperato scavalca TUTTI i defer sopra e crasha
	// l'intero processo — bgTaskSem/p.taskMu restavano acquisiti per
	// sempre anche se il crash non ci fosse stato, bloccando ogni task
	// futuro di OGNI plugin. Caso reale: crash ricorrenti del dev server
	// durante sync pesanti coincidevano esattamente con questo pattern
	// (nessun altro recover() esiste nel percorso di esecuzione dei task
	// Lua).
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[lua] task %s/%s: PANIC recuperato: %v", p.Manifest.ID, t.Function, r)
		}
	}()
	_, err := m.CallEntrypoint(p.Manifest.ID, t.Function, nil, "")
	m.RecordTaskResult(p.Manifest.ID, err == nil)
	if err != nil {
		log.Printf("[lua] task %s/%s: %v", p.Manifest.ID, t.Function, err)
	} else {
		log.Printf("[lua] task %s/%s: OK", p.Manifest.ID, t.Function)
		MarkTaskRan(p.Manifest.ID, t.Function)
	}
	// After any task, refresh auto_hide_when_empty catalog counts so that
	// ListPlugins reflects the current state without requiring a user fetch first.
	go m.warmupCatalogCounts(p)
}

// RunDueTasksNow runs, right now, every one of pluginID's cron-scheduled
// tasks that's due (see taskDue) — instead of waiting for the cron
// ticker's next tick or a full process restart. Call this right after a
// plugin transitions to running (dashboard "Avvia").
//
// Why this needs to exist at all: runTasks's own "what's due, run it"
// pass (below) only ever fires once per plugin, at LoadAll time (server
// boot) — long before the admin has had a chance to fill in required
// settings and click "Avvia". Starting a plugin later only flips a
// setting (SetRunEnabled); nothing re-ran that initial pass, so a
// freshly-configured plugin's first sync silently never happened until
// the whole process restarted (which re-runs LoadAll, masking the gap).
// Reported by the user 18/09: vix.movie/animeunity did nothing after
// "Avvia" until the dev server (`air`) was relaunched.
func (m *LuaPluginManager) RunDueTasksNow(pluginID string) {
	m.mu.RLock()
	p, found := m.plugins[pluginID]
	m.mu.RUnlock()
	if !found || !m.IsOperational(pluginID) {
		return
	}
	ctx := context.Background()
	for _, task := range p.Manifest.Tasks {
		t := task
		if m.taskDue(ctx, pluginID, t) {
			go m.runTaskNow(p, t)
		}
	}
}

func (m *LuaPluginManager) runTasks(p *LuaPlugin) {
	if len(p.Manifest.Tasks) == 0 {
		return
	}

	ctx := context.Background()

	for _, task := range p.Manifest.Tasks {
		t := task
		if m.taskDue(ctx, p.Manifest.ID, t) {
			go m.runTaskNow(p, t)
		} else if t.Cron == "" {
			log.Printf("[lua] task %s/%s: manual-only (cron vuoto), disponibile in dashboard", p.Manifest.ID, t.Function)
		} else {
			log.Printf("[lua] task %s/%s: skip (eseguito di recente)", p.Manifest.ID, t.Function)
		}
	}

	c := cron.New()
	for _, task := range p.Manifest.Tasks {
		if task.Cron == "" {
			continue // manual-only: mai una schedulazione ricorrente
		}
		t := task
		_, err := c.AddFunc(t.Cron, func() { m.runTaskNow(p, t) })
		if err != nil {
			log.Printf("[lua] plugin %s: invalid cron %q for task %s: %v", p.Manifest.ID, t.Cron, t.Function, err)
		}
	}
	c.Start()
	<-p.cronStop
	stopCtx := c.Stop()
	<-stopCtx.Done()
}

// resolveCatalogDefs returns the effective catalog list for p.
// If the plugin declares a "catalog_list" entrypoint the result of that call
// is used (allows fully dynamic catalogs). Falls back to the static manifest
// list on error or if the entrypoint is absent.
func (m *LuaPluginManager) resolveCatalogDefs(p *LuaPlugin) []LuaCatalogDef {
	if _, ok := p.Manifest.Entrypoints[EPGetCatalogList]; !ok {
		return p.Manifest.Exposes.Catalogs
	}
	// Same guard as run()/warmupCatalogCounts/lookupBackend: a stopped plugin
	// must not have its Lua invoked just because ListPlugins (media_handler.go)
	// resolves every loaded plugin's catalog defs before filtering by
	// IsOperational — without this, a plugin that declares catalog_list keeps
	// getting real entrypoint calls (and whatever network/log activity that
	// entails) on every ListPlugins refresh even while "fermo" in the
	// dashboard. No bundled plugin declares catalog_list today, so this was
	// a dormant gap rather than an observed one.
	if !m.IsOperational(p.Manifest.ID) {
		return p.Manifest.Exposes.Catalogs
	}
	raw, err := m.CallEntrypointJSON(p.Manifest.ID, EPGetCatalogList, nil, "")
	if err != nil {
		log.Printf("[lua] catalog_list %s: %v (using manifest)", p.Manifest.ID, err)
		return p.Manifest.Exposes.Catalogs
	}
	var defs []LuaCatalogDef
	if err := json.Unmarshal(raw, &defs); err != nil {
		log.Printf("[lua] catalog_list %s: parse: %v (using manifest)", p.Manifest.ID, err)
		return p.Manifest.Exposes.Catalogs
	}
	return defs
}

// ResolveCatalogDefs is the public version of resolveCatalogDefs for use by
// other packages (e.g. pileus).
func (m *LuaPluginManager) ResolveCatalogDefs(pluginID string) []LuaCatalogDef {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	return m.resolveCatalogDefs(p)
}

// warmupCatalogCounts fetches page 1 for every auto_hide_when_empty catalog
// and writes the item count to Redis. Called in a goroutine after each task
// run so that ListPlugins reflects current catalog state without waiting for
// a user to open each carousel first.
func (m *LuaPluginManager) warmupCatalogCounts(p *LuaPlugin) {
	if managers.Redis == nil {
		return
	}
	// Stesso controllo di run() in runTasks: senza questo, un plugin non
	// attivo fa comunque fetch reali a ogni caricamento (quindi anche a ogni
	// avvio del server) e dopo ogni task di QUALSIASI plugin — non solo il
	// proprio (vedi la chiamata in run(), sotto). Occupa inoltre uno slot di
	// bgTaskSem, lo stesso semaforo condiviso con l'esecuzione dei task veri.
	if !m.IsOperational(p.Manifest.ID) {
		return
	}
	bgTaskSem <- struct{}{}
	defer func() { <-bgTaskSem }()
	// Anche questa gira sempre in una goroutine nuda (go m.warmupCatalogCounts(p),
	// chiamata al boot e dopo ogni task) — stesso rischio di crash dell'intero
	// processo su un panic non recuperato, vedi il commento in runTasks/run().
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[lua] warmupCatalogCounts %s: PANIC recuperato: %v", p.Manifest.ID, r)
		}
	}()
	for _, c := range m.resolveCatalogDefs(p) {
		if !c.AutoHideWhenEmpty {
			continue
		}
		cat := c
		raw, err := m.CallEntrypointJSON(p.Manifest.ID, EPGetCatalog, map[string]any{
			"catalog_id": cat.ID,
			"page":       1,
		}, "")

		count := 0
		if err == nil && len(raw) > 0 {
			// Support both plain array and {items:[...], has_more:bool} shape.
			var wrapper struct {
				Items []json.RawMessage `json:"items"`
			}
			if jerr := json.Unmarshal(raw, &wrapper); jerr == nil && wrapper.Items != nil {
				count = len(wrapper.Items)
			} else {
				var arr []json.RawMessage
				if jerr2 := json.Unmarshal(raw, &arr); jerr2 == nil {
					count = len(arr)
				}
			}
		}

		// Use a generous TTL so the key outlives the refresh_matches interval (10m).
		// 30 minutes guarantees coverage even if a task run is delayed.
		wCtx, wCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = managers.Redis.Set(wCtx, managers.CatalogCountKey(p.Manifest.ID, cat.ID),
			strconv.Itoa(count), 30*time.Minute)
		wCancel()
		log.Printf("[lua] warmup count %s/%s: %d items", p.Manifest.ID, cat.ID, count)
	}
}

// defaultEntrypointTimeout is the budget for a plugin call unless a task in
// the manifest overrides it with timeout_seconds.
//
// Note this is ALSO the effective timeout for every entrypoints: mapping
// (search, browse, get_details, streams, resolve/resolve_stream, ...) —
// those are never tasks, so entrypointTimeout below can never find a match
// for them and always falls through to this constant. Audited 2026-09-14:
// resolve_stream in the bundled vix.movie/vix.series/animeunity plugins
// makes several sequential network calls (page fetch, optional
// browser.sniff, and for animeunity a domain-discovery step when not yet
// cached), each individually capped at ~30s by the underlying HTTP client,
// but whose SUM easily exceeds a 30s entrypoint budget covering all of
// them together. Worst case measured: ~90s "hot" (domain cached) for all
// three plugins, ~180s "cold" for animeunity's very first resolve after
// boot. 90s here comfortably covers every hot worst-case; the rare cold
// animeunity case is a known, accepted limit — not solved here, since
// chasing it would need extra logic (e.g. a longer budget only for a
// first-ever resolve) disproportionate to how rarely it happens. Revisit
// with a targeted fix if it turns out to matter in practice.
const defaultEntrypointTimeout = 90 * time.Second

// entrypointTimeout is the ctx budget for calling ep (alias-resolved to
// fnName): a task's manifest timeout_seconds when set (capped at 15 min),
// else the default. Applies to cron ticks and dashboard "Esegui" alike.
func entrypointTimeout(mf LuaManifest, ep, fnName string) time.Duration {
	for _, t := range mf.Tasks {
		if t.TimeoutSec <= 0 {
			continue
		}
		if t.Function == fnName || t.Function == ep {
			d := time.Duration(t.TimeoutSec) * time.Second
			if d > 15*time.Minute {
				d = 15 * time.Minute
			}
			return d
		}
	}
	return defaultEntrypointTimeout
}

// callWithTimeout runs L.CallByParam bounded by ctx — the entrypoint timeout
// used by CallEntrypoint/callEntrypointJSON previously only bounded acquiring a
// free LState from the pool, NOT the Lua call itself: L.CallByParam is a
// synchronous Go call with no cancellation hook, so a plugin function that
// hangs (an SDK network call missing its own timeout, a degenerate loop, a
// gopher-lua-level bug) ran forever, permanently holding that LState — and
// if invoked from a scheduled task, permanently holding bgTaskSem/p.taskMu
// too, since nothing could ever unblock the wait. Verified live
// (2026-08-18) as a real finding, not theoretical.
//
// Runs the call in its own goroutine and races it against ctx.Done(). On
// timeout the goroutine is NOT stopped (Go cannot force-kill a goroutine)
// — it keeps running against L in the background. The caller MUST treat L
// as unsafe to reuse in that case: never release() it back to the pool
// (see LuaPool.DiscardAndReplace) — a future Acquire() could hand it to an
// unrelated caller while the abandoned goroutine is still mutating it.
func callWithTimeout(ctx context.Context, L *lua.LState, fn lua.LValue, args *lua.LTable) (callErr error, timedOut bool) {
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		done <- L.CallByParam(lua.P{Fn: fn, NRet: 2, Protect: true}, args)
	}()
	select {
	case err := <-done:
		return err, false
	case <-ctx.Done():
		return ctx.Err(), true
	}
}

// CallEntrypoint calls a named entrypoint (by manifest key OR by Lua function name directly).
func (m *LuaPluginManager) CallEntrypoint(pluginID, ep string, args map[string]any, profileID string) (any, error) {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("lua plugin %q not loaded", pluginID)
	}

	// Resolve entrypoint alias from manifest, fall back to direct function name.
	fnName := ep
	if alias, ok := p.Manifest.Entrypoints[ep]; ok {
		fnName = alias
	}

	ctx, cancel := context.WithTimeout(context.Background(), entrypointTimeout(p.Manifest, ep, fnName))
	defer cancel()

	L, release, err := p.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: pool acquire: %w", pluginID, err)
	}
	// release() è chiamata solo se la call finisce entro ctx — su timeout L
	// potrebbe essere ancora in uso dalla goroutine abbandonata dentro
	// callWithTimeout, va scartata invece che rimessa nel pool (vedi
	// LuaPool.DiscardAndReplace). Stessa condizione per scope.reset(): su
	// timeout NON si azzera (la goroutine abbandonata potrebbe ancora leggere
	// requireProxy e finire su una connessione diretta), tanto quello stato
	// non torna mai nel pool.
	released := false
	sc := scopeOf(L)
	defer func() {
		if !released {
			// Strip any global the call just leaked (see resetNewGlobals) before
			// resetting the per-call scope fields and handing the state back —
			// the next Acquire() of this same LState can belong to a different
			// profile/user (pool_size default 2).
			sc.resetNewGlobals(L)
			sc.reset()
			release()
		}
	}()

	// Per-call state now lives in the LState's callScope (set here, read live
	// by the SDK closures built once in RegisterSDK) — no more rebuilding
	// context/network/browser on every call. requireProxy is forced true for a
	// video-flow entrypoint of a VPN-optional plugin, exactly as before.
	scReqProxy, scProxyURL := m.pluginCallProxy(pluginID, ep)
	sc.set(ctx, profileID, scReqProxy, scProxyURL, nil)

	fn := L.GetGlobal(fnName)
	if fn == lua.LNil {
		return nil, fmt.Errorf("plugin %q: function %q not defined", pluginID, fnName)
	}

	luaArgs := mapToLuaTable(L, args)

	callErr, timedOut := callWithTimeout(ctx, L, fn, luaArgs)
	if timedOut {
		p.Pool.DiscardAndReplace()
		p.recordDiscard()
		released = true
		return nil, fmt.Errorf("plugin %q %s: timeout, esecuzione abbandonata", pluginID, fnName)
	}
	if callErr != nil {
		return nil, fmt.Errorf("plugin %q %s: %w", pluginID, fnName, callErr)
	}

	// Lua convention: return value, err_string
	errVal := L.Get(-1)
	ret := L.Get(-2)
	L.Pop(2)

	if errVal != lua.LNil {
		if errStr, ok := errVal.(lua.LString); ok && string(errStr) != "" {
			return nil, fmt.Errorf("plugin %q %s: %s", pluginID, fnName, string(errStr))
		}
	}
	return luaToGo(ret), nil
}

// CallEntrypointJSON is like CallEntrypoint but serialises the Lua return value
// directly to JSON, skipping the intermediate Go-native representation.
// This eliminates one json.Marshal call and the associated allocations per request.
func (m *LuaPluginManager) CallEntrypointJSON(pluginID, ep string, args map[string]any, profileID string) (json.RawMessage, error) {
	return m.callEntrypointJSON(pluginID, ep, args, profileID, nil)
}

// CallEntrypointJSONWithProgress is CallEntrypointJSON, but the plugin can call
// mycelium.progress(status, message) while still running and have each update
// forwarded to onProgress — used by resolve_stream so a plugin can narrate what
// it's doing (trying a mirror, falling back to the browser sniffer, …) instead
// of the caller only ever seeing the final result.
func (m *LuaPluginManager) CallEntrypointJSONWithProgress(pluginID, ep string, args map[string]any, profileID string, onProgress ProgressFunc) (json.RawMessage, error) {
	return m.callEntrypointJSON(pluginID, ep, args, profileID, onProgress)
}

func (m *LuaPluginManager) callEntrypointJSON(pluginID, ep string, args map[string]any, profileID string, onProgress ProgressFunc) (json.RawMessage, error) {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("lua plugin %q not loaded", pluginID)
	}

	fnName := ep
	if alias, ok := p.Manifest.Entrypoints[ep]; ok {
		fnName = alias
	}

	ctx, cancel := context.WithTimeout(context.Background(), entrypointTimeout(p.Manifest, ep, fnName))
	defer cancel()

	L, release, err := p.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: pool acquire: %w", pluginID, err)
	}
	// Su timeout L potrebbe restare in uso dalla goroutine abbandonata dentro
	// callWithTimeout — non va rimessa nel pool in quel caso, vedi
	// LuaPool.DiscardAndReplace e il commento gemello in CallEntrypoint.
	// Idem per scope.reset(): saltato su timeout.
	released := false
	sc := scopeOf(L)
	defer func() {
		if !released {
			// Strip any global the call just leaked (see resetNewGlobals) before
			// resetting the per-call scope fields and handing the state back —
			// the next Acquire() of this same LState can belong to a different
			// profile/user (pool_size default 2).
			sc.resetNewGlobals(L)
			sc.reset()
			release()
		}
	}()

	// Per-call state (profile id, VPN forcing, streaming-progress sink) goes
	// into the LState's callScope; the SDK closures built once in RegisterSDK
	// read it live. No per-call module rebuild.
	scReqProxy, scProxyURL := m.pluginCallProxy(pluginID, ep)
	sc.set(ctx, profileID, scReqProxy, scProxyURL, onProgress)

	fn := L.GetGlobal(fnName)
	if fn == lua.LNil {
		return nil, fmt.Errorf("plugin %q: function %q not defined", pluginID, fnName)
	}

	callErr, timedOut := callWithTimeout(ctx, L, fn, mapToLuaTable(L, args))
	if timedOut {
		p.Pool.DiscardAndReplace()
		p.recordDiscard()
		released = true
		return nil, fmt.Errorf("plugin %q %s: timeout, esecuzione abbandonata", pluginID, fnName)
	}
	if callErr != nil {
		return nil, fmt.Errorf("plugin %q %s: %w", pluginID, fnName, callErr)
	}

	errVal := L.Get(-1)
	ret := L.Get(-2)
	L.Pop(2)

	if errVal != lua.LNil {
		if errStr, ok := errVal.(lua.LString); ok && string(errStr) != "" {
			return nil, fmt.Errorf("plugin %q %s: %s", pluginID, fnName, string(errStr))
		}
	}
	if ret == lua.LNil {
		return json.RawMessage("null"), nil
	}
	var buf bytes.Buffer
	luaValueToJSON(&buf, ret)
	return buf.Bytes(), nil
}

// luaValueToJSON serialises a Lua value directly to JSON without an intermediate
// Go representation, avoiding the luaToGo alloc chain.
func luaValueToJSON(w *bytes.Buffer, v lua.LValue) {
	switch val := v.(type) {
	case *lua.LNilType:
		w.WriteString("null")
	case lua.LBool:
		if bool(val) {
			w.WriteString("true")
		} else {
			w.WriteString("false")
		}
	case lua.LNumber:
		f := float64(val)
		if i := int64(f); float64(i) == f {
			w.WriteString(strconv.FormatInt(i, 10))
		} else {
			w.WriteString(strconv.FormatFloat(f, 'f', -1, 64))
		}
	case lua.LString:
		b, _ := json.Marshal(string(val))
		w.Write(b)
	case *lua.LTable:
		luaTableToJSON(w, val)
	default:
		b, _ := json.Marshal(v.String())
		w.Write(b)
	}
}

// luaTableToJSON decides array vs object using the same heuristic as luaTableToGo.
func luaTableToJSON(w *bytes.Buffer, t *lua.LTable) {
	isArray := true
	maxIdx := 0
	t.ForEach(func(k, _ lua.LValue) {
		if n, ok := k.(lua.LNumber); ok {
			idx := int(n)
			if float64(idx) == float64(n) && idx >= 1 {
				if idx > maxIdx {
					maxIdx = idx
				}
				return
			}
		}
		isArray = false
	})
	if isArray && maxIdx == t.Len() {
		w.WriteByte('[')
		for i := 1; i <= maxIdx; i++ {
			if i > 1 {
				w.WriteByte(',')
			}
			luaValueToJSON(w, t.RawGetInt(i))
		}
		w.WriteByte(']')
		return
	}
	w.WriteByte('{')
	first := true
	t.ForEach(func(k, v lua.LValue) {
		if !first {
			w.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(k.String())
		w.Write(kb)
		w.WriteByte(':')
		luaValueToJSON(w, v)
	})
	w.WriteByte('}')
}

// GetStatusBatch reads runtime status for multiple plugins in one Redis MGET,
// replacing N sequential GET calls in ListPlugins. When a plugin has no
// explicit status set, statusFallback derives one (warm-up vs ready).
func (m *LuaPluginManager) GetStatusBatch(pluginIDs []string) map[string]PluginStatus {
	result := make(map[string]PluginStatus, len(pluginIDs))
	if len(pluginIDs) == 0 {
		return result
	}
	var vals []string
	if managers.Redis != nil {
		keys := make([]string, len(pluginIDs))
		for i, id := range pluginIDs {
			keys[i] = pluginStatusKey(id)
		}
		vals, _ = managers.Redis.MGet(context.Background(), keys...)
	}
	for i, id := range pluginIDs {
		if i < len(vals) && vals[i] != "" {
			var s PluginStatus
			if json.Unmarshal([]byte(vals[i]), &s) == nil && s.Label != "" {
				result[id] = s
				continue
			}
		}
		result[id] = m.statusFallback(id)
	}
	return result
}

// GetLogBuffer returns the PluginLogBuffer for pluginID, or nil if not loaded.
func (m *LuaPluginManager) GetLogBuffer(pluginID string) *managers.PluginLogBuffer {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	return p.LogBuf
}

// Has reports whether a Lua plugin with pluginID is loaded.
func (m *LuaPluginManager) Has(pluginID string) bool {
	m.mu.RLock()
	_, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	return ok
}

// GetMeta returns manifest info for all loaded Lua plugins.
func (m *LuaPluginManager) GetMeta() []LuaManifest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]LuaManifest, 0, len(m.plugins))
	for _, p := range m.plugins {
		out = append(out, p.Manifest)
	}
	return out
}

// LuaPluginMeta combines the manifest with the live runtime status.
type LuaPluginMeta struct {
	LuaManifest
	Status PluginStatus `json:"status"`
	// RunState is the operational lifecycle: "waiting" | "stopped" | "running".
	RunState string `json:"run_state"`
	// MissingRequired lists the ids of required settings still unset (only
	// populated when RunState == "waiting").
	MissingRequired []string `json:"missing_required"`
	// Egress is the name of the network-exit profile this plugin's video flow
	// takes (managers.EgressProfiles): "direct", "warp", or an operator-added
	// profile. Editable from the dashboard Plugin card.
	Egress string `json:"egress"`
	// DiscardedStates is how many times an entrypoint timeout has forced this
	// plugin's Lua pool to discard-and-replace a state since it was loaded
	// (see LuaPlugin.discardedStates). Zero in the overwhelming majority of
	// cases; a number that keeps climbing flags a stuck task/entrypoint
	// leaking a goroutine + LState per occurrence (see recordDiscard).
	DiscardedStates uint64 `json:"discarded_states"`
}

// GetMetaWithStatus returns manifest + current runtime status for all loaded plugins.
func (m *LuaPluginManager) GetMetaWithStatus() []LuaPluginMeta {
	m.mu.RLock()
	ids := make([]string, 0, len(m.plugins))
	manifests := make(map[string]LuaManifest, len(m.plugins))
	for id, p := range m.plugins {
		ids = append(ids, id)
		manifests[id] = p.Manifest
	}
	m.mu.RUnlock()

	out := make([]LuaPluginMeta, 0, len(ids))
	for _, id := range ids {
		missing := m.MissingRequiredSettings(id)
		out = append(out, LuaPluginMeta{
			LuaManifest:     manifests[id],
			Status:          m.GetStatus(id),
			RunState:        m.RunStateOf(id),
			MissingRequired: missing,
			Egress:          m.PluginEgress(id),
			DiscardedStates: m.DiscardedStates(id),
		})
	}
	return out
}

// DiscardedStates returns how many times pluginID's Lua pool has had a state
// discarded-and-replaced due to an entrypoint timeout since it was loaded
// (see LuaPlugin.discardedStates / recordDiscard). Returns 0 for an unknown
// plugin id, and resets to 0 on reload (loadPlugin builds a fresh *LuaPlugin).
func (m *LuaPluginManager) DiscardedStates(pluginID string) uint64 {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return 0
	}
	return p.discardedStates.Load()
}

// GetStatus reads the current runtime status for pluginID. An explicit status
// set by the plugin (set_plugin_status) always wins; otherwise statusFallback
// derives one.
func (m *LuaPluginManager) GetStatus(pluginID string) PluginStatus {
	if managers.Redis != nil {
		if raw, err := managers.Redis.Get(context.Background(), pluginStatusKey(pluginID)); err == nil && raw != "" {
			var s PluginStatus
			if json.Unmarshal([]byte(raw), &s) == nil && s.Label != "" {
				return s
			}
		}
	}
	return m.statusFallback(pluginID)
}

// statusFallback derives a status for a plugin that has never called
// set_plugin_status. A `running` plugin with scheduled tasks that has not yet
// completed a single successful task run is still warming up (first indexing)
// — reported as syncing so the dashboard and Pileus don't present it as
// "ready" with an empty catalog. Note: task health is shared across all of a
// plugin's tasks, so any one task succeeding (even a light one like
// prefetch_logos) clears the warm-up hint; plugins that call
// set_plugin_status themselves are unaffected by this heuristic.
func (m *LuaPluginManager) statusFallback(pluginID string) PluginStatus {
	if m.RunStateOf(pluginID) == RunStateRunning && m.hasScheduledTasks(pluginID) {
		if _, lastOk := m.GetReachability(pluginID); lastOk == 0 {
			return PluginStatus{Label: StatusSyncing, Detail: "Prima indicizzazione in corso…"}
		}
	}
	return PluginStatus{Label: StatusReady}
}

// hasScheduledTasks reports whether the manifest declares at least one task
// with a real cron schedule (manual-only tasks with cron:"" don't count).
func (m *LuaPluginManager) hasScheduledTasks(pluginID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.plugins[pluginID]
	if !ok {
		return false
	}
	for _, t := range p.Manifest.Tasks {
		if t.Cron != "" {
			return true
		}
	}
	return false
}

// SetStatus persists the runtime status for pluginID to Redis (no TTL — lives until overwritten).
func (m *LuaPluginManager) SetStatus(pluginID, label, detail string) {
	if managers.Redis == nil {
		return
	}
	b, _ := json.Marshal(PluginStatus{Label: label, Detail: detail})
	_ = managers.Redis.Set(context.Background(), pluginStatusKey(pluginID), string(b), 0)
}

// LoadPlugin loads (or reloads) a single plugin from dir. Public wrapper around loadPlugin.
func (m *LuaPluginManager) LoadPlugin(dir string) error {
	return m.loadPlugin(dir)
}

// UnloadPlugin stops and removes a Lua plugin by ID. Does not delete files.
func (m *LuaPluginManager) UnloadPlugin(pluginID string) {
	m.mu.Lock()
	p, ok := m.plugins[pluginID]
	if ok {
		close(p.stopHR)
		close(p.cronStop)
		p.Pool.Close()
		delete(m.plugins, pluginID)
	}
	m.mu.Unlock()
}

// Shutdown stops all hot-reload watchers, task schedulers, and closes all pools.
func (m *LuaPluginManager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.plugins {
		close(p.stopHR)
		close(p.cronStop)
		p.Pool.Close()
	}
	m.plugins = make(map[string]*LuaPlugin)
}

func (m *LuaPluginManager) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.plugins)
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// ReadLuaManifest reads and parses manifest.yaml from dir. Public for use by api layer.
func ReadLuaManifest(dir string) (LuaManifest, error) {
	return readLuaManifest(dir)
}

// VPNOptInSettingID is the key of the synthetic per-plugin setting injected
// when a manifest declares vpn_optional: true (with direct_egress: true).
// Stored like any other global Lua setting (lua:{pluginID}:global:vpn_enabled),
// off by default.
const VPNOptInSettingID = "vpn_enabled"

// pluginIDPattern whitelists manifest.yaml's `id:` field: lowercase
// alphanumerics, with '.', '_' or '-' allowed only between two alphanumerics
// (real ids look like "animeunity" or "vix.movie", never leading/trailing
// punctuation). mf.ID is never sanitized by the YAML parser and, once loaded,
// is interpolated into HTML attributes (web/static/admin.js buildCard) and
// used to build filesystem/Redis/setting keys throughout this package — a
// hostile id (e.g. containing '"' to break out of an HTML attribute) must be
// rejected here, at the single choke point every manifest load goes through
// (loadPlugin, the admin ZIP upload and the admin manifest preview all call
// readLuaManifest), rather than relying only on callers to escape it.
var pluginIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`)

func readLuaManifest(dir string) (LuaManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		return LuaManifest{}, err
	}
	var mf LuaManifest
	if err := yaml.Unmarshal(data, &mf); err != nil {
		return LuaManifest{}, err
	}
	if mf.ID == "" {
		return LuaManifest{}, fmt.Errorf("manifest.yaml missing 'id' in %s", dir)
	}
	if !pluginIDPattern.MatchString(mf.ID) {
		return LuaManifest{}, fmt.Errorf("manifest.yaml 'id' %q in %s is invalid: must match %s", mf.ID, dir, pluginIDPattern.String())
	}
	if mf.VPNOptional && mf.DirectEgress {
		mf.Settings.Global = append(mf.Settings.Global, LuaSettingField{
			ID:    VPNOptInSettingID,
			Label: "Instrada il flusso video di questo plugin tramite VPN — più lento, attiva solo se la sorgente blocca l'IP del server",
			Type:  "bool",
		})
	}
	return mf, nil
}

// EgressSettingID is the per-plugin key naming which egress profile the
// plugin's video flow takes (managers.EgressProfiles): a name, or
// managers.EgressDirect. Stored at lua:{id}:global:egress.
const EgressSettingID = "egress"

// PluginEgress returns the egress-profile name for pluginID's video flow.
// Precedence: an explicit lua:{id}:global:egress → a one-time migration from
// the legacy vpn_enabled toggle → the manifest default (direct_egress plugins
// default to "direct", VPN-routed ones to "warp").
func (m *LuaPluginManager) PluginEgress(pluginID string) string {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	def := "warp"
	if ok && p.Manifest.DirectEgress {
		def = managers.EgressDirect
	}
	if v := managers.Settings.GetString("lua:"+pluginID+":global:"+EgressSettingID, ""); v != "" {
		return v
	}
	// migrate: an old "vpn_enabled=true" means "route video via WARP".
	if managers.Settings.GetString("lua:"+pluginID+":global:"+VPNOptInSettingID, "") == "true" {
		return "warp"
	}
	return def
}

// pluginCallProxy resolves the (requireProxy, proxyURL) the call scope should
// carry for entrypoint ep of pluginID — i.e. what mycelium.network / .browser
// do while the plugin's Lua runs.
//
//   - direct_egress, NO explicit egress picked in the dashboard: the default —
//     scrape from the box's own IP. A vpn_optional one still routes just its
//     video-flow entrypoints (resolve/streams) through PluginEgress (migrated
//     from the old vpn_enabled toggle), which MUST share the egress with the
//     HLS segment proxy and the operator's interactive-verification step (the
//     browser service's per-(domain, egress) cookie jar is only replayed to a
//     sniff on the same egress).
//   - direct_egress WITH an explicit egress setting: the operator overrode the
//     default on purpose — honour it for EVERY entrypoint. Their box's own
//     link may be the broken one (CGNAT, a flagged IP), so scraping needs the
//     tunnel too, not just the video flow.
//   - non-direct_egress: the VPN covers every call.
//
// A selected-but-disabled egress yields (true, "") so the SDK blocks the call
// rather than leaking a direct connection.
func (m *LuaPluginManager) pluginCallProxy(pluginID, ep string) (requireProxy bool, proxyURL string) {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()

	explicit := managers.Settings.GetString("lua:"+pluginID+":global:"+EgressSettingID, "") != ""
	name := m.PluginEgress(pluginID)
	if ok && p.Manifest.DirectEgress && !explicit &&
		(!p.Manifest.VPNOptional || !isVideoFlowEntrypoint(ep)) {
		name = managers.EgressDirect
	}
	if name == "" || name == managers.EgressDirect {
		return false, ""
	}
	url, avail := managers.ResolveEgressProxy(name)
	if !avail || url == "" {
		return true, "" // selected egress unavailable → fail closed
	}
	return true, url
}

// shouldUseVPNForVideo: kept for the proxy/media_handler call sites — true when
// the plugin's video flow is NOT direct.
func (m *LuaPluginManager) shouldUseVPNForVideo(pluginID string) bool {
	return m.PluginEgress(pluginID) != managers.EgressDirect
}

// ShouldUseVPNForVideo is the public form of shouldUseVPNForVideo, used by
// internal/api/proxy.go and internal/pileus/media_handler.go to decide
// whether to tag proxied playlist/segment/key URLs with vpn=1.
func (m *LuaPluginManager) ShouldUseVPNForVideo(pluginID string) bool {
	return m.shouldUseVPNForVideo(pluginID)
}

// ShouldSkipProxy reports whether pluginID's resolved stream URL should
// bypass mycelium's own HLS/segment proxy (see LuaManifest.DirectStream).
func (m *LuaPluginManager) ShouldSkipProxy(pluginID string) bool {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	return ok && p.Manifest.DirectStream
}

func fileMod(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func mapToLuaTable(L *lua.LState, m map[string]any) *lua.LTable {
	t := L.NewTable()
	for k, v := range m {
		t.RawSetString(k, goToLua(L, v))
	}
	return t
}

func goToLua(L *lua.LState, v any) lua.LValue {
	if v == nil {
		return lua.LNil
	}
	switch val := v.(type) {
	case string:
		return lua.LString(val)
	case int:
		return lua.LNumber(val)
	case int32:
		return lua.LNumber(val)
	case int64:
		return lua.LNumber(val)
	case float32:
		return lua.LNumber(val)
	case float64:
		return lua.LNumber(val)
	case bool:
		if val {
			return lua.LTrue
		}
		return lua.LFalse
	case map[string]any:
		return mapToLuaTable(L, val)
	case []any:
		t := L.NewTable()
		for _, item := range val {
			t.Append(goToLua(L, item))
		}
		return t
	default:
		b, _ := json.Marshal(v)
		return lua.LString(string(b))
	}
}

func luaToGo(v lua.LValue) any {
	switch val := v.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(val)
	case lua.LNumber:
		return float64(val)
	case lua.LString:
		return string(val)
	case *lua.LTable:
		return luaTableToGo(val)
	default:
		return v.String()
	}
}

func luaTableToGo(t *lua.LTable) any {
	isArray := true
	maxIdx := 0
	t.ForEach(func(k, _ lua.LValue) {
		if n, ok := k.(lua.LNumber); ok {
			idx := int(n)
			if float64(idx) == float64(n) && idx >= 1 {
				if idx > maxIdx {
					maxIdx = idx
				}
				return
			}
		}
		isArray = false
	})
	if isArray && maxIdx == t.Len() {
		arr := make([]any, 0, maxIdx)
		for i := 1; i <= maxIdx; i++ {
			arr = append(arr, luaToGo(t.RawGetInt(i)))
		}
		return arr
	}
	m := make(map[string]any)
	t.ForEach(func(k, v lua.LValue) {
		m[k.String()] = luaToGo(v)
	})
	return m
}

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// DirectEgress opts the plugin out of VPN routing: its calls use the server's
	// own connection. By default every call (HTTP, browser, HLS relay) goes
	// through the configured egress and fails closed without one.
	DirectEgress bool `yaml:"direct_egress"`
	// VPNOptional lets the operator route only the video flow (streams/resolve
	// and the HLS relay) of a DirectEgress plugin through the VPN, via a
	// synthetic "vpn_enabled" setting.
	VPNOptional bool `yaml:"vpn_optional"`
	// DirectStream hands the resolved URL to the player as-is, bypassing the
	// HLS proxy — for sources the player can reach directly (e.g. a LAN media
	// server with native Range support).
	DirectStream bool `yaml:"direct_stream"`
	// Download opts the plugin into offline downloads (internal/downloads).
	Download    LuaManifestDownload `yaml:"download"`
	Settings    LuaManifestSettings `yaml:"settings"`
	Exposes     LuaManifestExposes  `yaml:"exposes"`
	Entrypoints map[string]string   `yaml:"entrypoints"`
	Tasks       []LuaManifestTask   `yaml:"tasks"`
}

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
	// AutoHideWhenEmpty: ListPlugins omits the catalog while its last page-1
	// fetch returned no items.
	AutoHideWhenEmpty bool `yaml:"auto_hide_when_empty" json:"auto_hide_when_empty"`
	// SectionKind: "carousel"|"grid" layout hint; "" = client default.
	SectionKind string `yaml:"section_kind" json:"section_kind"`
	// StyleHint: free-form presentation hint within SectionKind (e.g. "featured"|"compact").
	StyleHint string `yaml:"style_hint" json:"style_hint"`
	// CardLayout: "" (poster, 2:3) | "landscape" (16:9).
	CardLayout string `yaml:"card_layout" json:"card_layout"`
	// DisableHeroBackground: never use this catalog's artwork as the home hero
	// background.
	DisableHeroBackground bool `yaml:"disable_hero_background" json:"disable_hero_background"`
}

type LuaManifestTask struct {
	Function string `yaml:"function"`
	// Cron: "@every <dur>", a cron expression, or "" for a manual-only task
	// (run from the dashboard, never scheduled).
	Cron string `yaml:"cron"`
	// LiveRefresh lets the client trigger the task on demand
	// (PluginService.TriggerRefresh), besides its schedule.
	LiveRefresh bool `yaml:"live_refresh"`
	// TimeoutSec overrides defaultEntrypointTimeout for this task (max 15 min;
	// 0 = default).
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

// isVideoFlowEntrypoint reports whether ep touches the video CDN (streams,
// resolve): for a direct_egress + vpn_optional plugin these are the only
// entrypoints that follow the selected egress.
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
	Detail string `json:"detail"` // human-readable
}

// pluginStatusKey returns the Redis key for a plugin's runtime status.
func pluginStatusKey(pluginID string) string {
	return "mycelium:plugin:" + pluginID + ":status"
}

// ─────────────────────────────────────────────────────────────────────────────
// PluginHealth — reachability derived from task outcomes: every task run
// updates it, and reachableFailureThreshold consecutive failures mark the
// plugin unreachable until the next success.
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

// RecordTaskResult updates pluginID's health after a task run. Callers
// already serialize task execution per plugin, so no extra lock is needed.
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

// GetReachability reports whether pluginID is considered reachable and when
// it last succeeded (true, 0 when no task has run yet).
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

// RunStateOf computes the lifecycle state: waiting > stopped > running. A
// plugin with required settings that was never started counts as stopped.
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

// SetRunEnabled flips the stored start/stop flag; schedulers and entrypoint
// gates re-check IsOperational on every call.
func (m *LuaPluginManager) SetRunEnabled(pluginID string, enabled bool) {
	_ = managers.Settings.Save(map[string]any{
		pluginDisabledKey(pluginID): map[bool]string{true: "false", false: "true"}[enabled],
	})
}

// TriggerLiveRefresh runs pluginID's live_refresh task(s) now, in the
// background, gated by bgTaskSem.
func (m *LuaPluginManager) TriggerLiveRefresh(pluginID string) (ok bool, message string) {
	m.mu.RLock()
	p, found := m.plugins[pluginID]
	m.mu.RUnlock()
	if !found {
		return false, "plugin non caricato"
	}
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
	// Skip rather than wait when a task of this plugin is already running.
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
	taskMu   sync.Mutex // serializes this plugin's tasks

	// discardedStates counts entrypoint calls whose Lua state was discarded
	// after a timeout (see callWithTimeout); exposed to the dashboard.
	discardedStates atomic.Uint64
}

// discardWarnEvery: log a warning every this-many discards of a plugin.
const discardWarnEvery = 5

// recordDiscard increments discardedStates and periodically logs a warning.
// It never disables the plugin: that decision is left to the operator.
func (p *LuaPlugin) recordDiscard() {
	n := p.discardedStates.Add(1)
	if n%discardWarnEvery == 0 {
		log.Printf("[lua] plugin %s: %d stati Lua scartati per timeout finora — possibile entrypoint/task bloccato, verificare i log del plugin", p.Manifest.ID, n)
	}
}

func (p *LuaPlugin) scriptPath() string {
	return filepath.Join(p.Dir, "init.lua")
}

// IconPath returns the absolute path of the plugin's `icon:` (relative to
// its directory); ok is false when unset, missing, or outside the directory.
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
	proxyClient  *http.Client // VPN/proxy — the default egress; skipped for direct_egress plugins

	// proxyClients caches one plugin HTTP client per egress proxy URL.
	proxyClients   map[string]*http.Client
	proxyClientsMu sync.Mutex

	// readCo merges identical concurrent read calls, see lua_readcoalesce.go.
	readCo readCoalescer
}

// ProxyClientFor returns a cached http.Client bound to proxyURL (nil when
// proxyURL is empty).
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
	c := NewPluginHTTPClient(proxyURL)
	m.proxyClients[proxyURL] = c
	return c
}

func (m *LuaPluginManager) SetProxyAddr(addr string) {
	m.proxyAddr = addr
	if addr != "" {
		m.proxyClient = NewPluginHTTPClient(addr)
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

// UsesVPN reports whether the plugin routes its traffic through the VPN
// proxy (every plugin without direct_egress). False for unknown IDs.
func (m *LuaPluginManager) UsesVPN(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.plugins[id]
	return ok && !p.Manifest.DirectEgress
}

func (m *LuaPluginManager) LoadAll(dir string) error {
	m.pluginsRoot = dir
	if m.directClient == nil {
		m.directClient = NewPluginHTTPClient("")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		// Skip "shared" (modules preloaded into every plugin) and dot-dirs
		// (leftover upload staging directories).
		if !e.IsDir() || e.Name() == "shared" || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := m.loadPlugin(filepath.Join(dir, e.Name())); err != nil {
			log.Printf("[lua] plugin %s failed to load: %v", e.Name(), err)
		}
	}
	log.Printf("[lua] plugins loaded: %d", m.count())
	return nil
}

// maxPoolSize caps a manifest's pool_size: every state is created at load
// time, so an absurd value could stall startup or exhaust memory.
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
		// A plugin loaded from another directory may not claim an already-loaded
		// id (it would take over its cache, secrets and settings); reloading from
		// the same directory is allowed.
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
	// Populate auto_hide counts right away from whatever is cached.
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
				// A bad reload must not stop hot-reload for this plugin.
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

// MarkTaskRan records that a task just ran — cron or manual — so the startup
// catch-up (taskDue) doesn't treat it as overdue on the next restart.
func MarkTaskRan(pluginID, fn string) {
	if managers.Redis == nil {
		return
	}
	// A run the plugin itself marked as failed (set_plugin_status("error", …))
	// doesn't count as a recent run, so it is retried on the next tick.
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
		var s string
		fmt.Sscanf(spec, "@every %s", &s)
		if parsed, err := time.ParseDuration(s); err == nil {
			d = parsed
		}
	}
	return d
}

// runTasks schedules the manifest's cron tasks.
// bgTaskSem caps concurrent background tasks process-wide, so tasks that are
// all due at once (e.g. at boot) don't run every heavy catalog sync in
// parallel. Capacity 2 lets a light task run beside one long sync.
var bgTaskSem = make(chan struct{}, 2)

// TryAcquireBgTaskSem takes a bgTaskSem slot without blocking (the
// dashboard's manual run answers busy/started at once). On true the caller
// must ReleaseBgTaskSem() when done (defer).
func TryAcquireBgTaskSem() bool {
	select {
	case bgTaskSem <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseBgTaskSem releases a slot taken with TryAcquireBgTaskSem.
func ReleaseBgTaskSem() {
	<-bgTaskSem
}

// TryAcquirePluginTaskLock takes pluginID's task lock without blocking, so a
// manual run never overlaps a scheduled run of the same plugin. Returns a
// release function and true when acquired; call release only when ok.
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

// taskDue reports whether t should run now: never run before, or its cron
// interval has elapsed. A task with cron "" is manual-only and never due.
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

// runTaskNow executes t, re-checking that the plugin is running (callers may
// race a stop or a settings change).
func (m *LuaPluginManager) runTaskNow(p *LuaPlugin, t LuaManifestTask) {
	if !m.IsOperational(p.Manifest.ID) {
		log.Printf("[lua] task %s/%s: skip (plugin non attivo: %s)", p.Manifest.ID, t.Function, m.RunStateOf(p.Manifest.ID))
		return
	}
	bgTaskSem <- struct{}{}
	defer func() { <-bgTaskSem }()
	p.taskMu.Lock()
	defer p.taskMu.Unlock()
	// Recover here: this runs in its own goroutine, and an unrecovered panic
	// would crash the process with bgTaskSem/taskMu still held.
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
	// After any task, refresh auto_hide_when_empty catalog counts.
	go m.warmupCatalogCounts(p)
}

// RunDueTasksNow runs pluginID's due cron tasks right away — called when a
// plugin is started from the dashboard, since runTasks' initial pass only
// happens at load time.
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
			continue // manual-only: never scheduled
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
	// A stopped plugin must not run Lua: ListPlugins resolves catalog defs of
	// every loaded plugin before filtering.
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
	// Only running plugins; this takes a bgTaskSem slot like a real task.
	if !m.IsOperational(p.Manifest.ID) {
		return
	}
	bgTaskSem <- struct{}{}
	defer func() { <-bgTaskSem }()
	// Runs in its own goroutine: recover so a panic can't crash the process.
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
		raw, err := m.CallEntrypointJSONBackground(p.Manifest.ID, EPGetCatalog, map[string]any{
			"catalog_id": cat.ID,
			"page":       1,
		}, "")
		if err != nil {
			// A failed call is "unknown", not "empty": keep the cached count instead of
			// hiding the catalog.
			log.Printf("[lua] warmup count %s/%s: skip (%v)", p.Manifest.ID, cat.ID, err)
			continue
		}

		count := 0
		if len(raw) > 0 {
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

		// TTL outlives the usual refresh intervals of live catalogs.
		wCtx, wCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = managers.Redis.Set(wCtx, managers.CatalogCountKey(p.Manifest.ID, cat.ID),
			strconv.Itoa(count), 30*time.Minute)
		wCancel()
		log.Printf("[lua] warmup count %s/%s: %d items", p.Manifest.ID, cat.ID, count)
	}
}

// defaultEntrypointTimeout is the budget of a plugin call when the manifest
// sets no timeout_seconds for it. It is also the budget of every entrypoint
// that isn't a task (resolve chains several network calls).
const defaultEntrypointTimeout = 90 * time.Second

// entrypointTimeout is the ctx budget for calling ep (alias-resolved to
// fnName): the task's timeout_seconds when set (max 15 min), else the default.
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
	if isReadEntrypoint(ep) {
		return readEntrypointTimeout
	}
	return defaultEntrypointTimeout
}

// readEntrypointTimeout bounds the plain read entrypoints (search, filters,
// details, browse, streams): shorter than resolve's budget so a stuck
// upstream frees its LState sooner. The client's read timeout sits just above.
const readEntrypointTimeout = 45 * time.Second

func isReadEntrypoint(ep string) bool {
	switch ep {
	case EPSearch, EPGetSearchFilters, EPGetDetails, EPBrowse, EPGetStreams:
		return true
	}
	return false
}

// callWithTimeout runs L.CallByParam bounded by ctx. On timeout the goroutine
// keeps running (Go can't kill it), so the caller must discard L instead of
// returning it to the pool (see LuaPool.DiscardAndReplace).
func callWithTimeout(ctx context.Context, L *lua.LState, fn lua.LValue, args *lua.LTable) (callErr error, timedOut bool) {
	// With a context bound, gopher-lua stops the abandoned goroutine at the next
	// instruction once ctx is done. Cleared only on normal completion; a
	// timed-out L is discarded.
	L.SetContext(ctx)
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
		L.RemoveContext()
		return err, false
	case <-ctx.Done():
		return ctx.Err(), true
	}
}

// userAcquireWait caps how long a user-facing call waits for a free LState;
// a pool full for this long means an overloaded plugin. A var for tests.
var userAcquireWait = 15 * time.Second

// A finished plugin call is logged only when it waited for a state or ran
// long.
const (
	slowWaitLog = time.Second
	slowExecLog = 3 * time.Second
)

// callClass says who is waiting on a plugin call, which decides how it may
// use the plugin's small LState pool.
type callClass int

const (
	// classInteractive: a user just asked for this (details, browse, search,
	// streams, resolve). Never held back by a slot, but waits a bounded time.
	classInteractive callClass = iota
	// classCatalog: the GetCatalog entrypoint. Shares one budget with
	// classBackground (LuaPool.BackgroundSlot/CatalogSlot), so an interactive
	// call always finds a free state.
	classCatalog
	// classBackground: cron tasks and the catalog-count warmup. Shares the
	// same budget as classCatalog above.
	classBackground
)

func (c callClass) String() string {
	switch c {
	case classCatalog:
		return "catalog"
	case classBackground:
		return "background"
	}
	return "interactive"
}

// classOf is the class of a request-driven call to entrypoint ep.
func classOf(ep string) callClass {
	if ep == EPGetCatalog || ep == EPGetCatalogList {
		return classCatalog
	}
	return classInteractive
}

// acquireForCall borrows an LState for one plugin call. Catalog and
// background callers first take their slot so they can never hold every
// state; interactive callers wait a bounded time. done must always be called
// (defer) — it gives the slot back; release returns the LState itself and is
// only for the normal, non-timeout path.
func acquireForCall(ctx context.Context, p *LuaPlugin, class callClass) (L *lua.LState, release, done func(), wait time.Duration, err error) {
	start := time.Now()
	done = func() {}
	waitCtx := ctx
	switch class {
	case classBackground, classCatalog:
		take := p.Pool.BackgroundSlot
		if class == classCatalog {
			take = p.Pool.CatalogSlot
		}
		free, serr := take(ctx)
		if serr != nil {
			return nil, nil, nil, time.Since(start), serr
		}
		done = free
	default:
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, userAcquireWait)
		defer cancel()
	}
	L, release, err = p.Pool.Acquire(waitCtx)
	wait = time.Since(start)
	if err != nil {
		done()
		if class == classInteractive && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			free, size := p.Pool.Stats()
			err = fmt.Errorf("%w: nessuno stato Lua libero entro %s (liberi %d/%d)", ErrPluginBusy, userAcquireWait, free, size)
		}
		return nil, nil, nil, wait, err
	}
	return L, release, done, wait, nil
}

// logCall records how a plugin call went when it is worth a line: it timed
// out, could not get a state, waited for one, or ran long. outcome is "ok",
// "error", "timeout" or "busy".
func (p *LuaPlugin) logCall(fnName string, class callClass, wait, exec time.Duration, outcome string) {
	if outcome != "timeout" && outcome != "busy" && wait < slowWaitLog && exec < slowExecLog {
		return
	}
	free, size := p.Pool.Stats()
	log.Printf("[lua] call plugin=%s fn=%s class=%s outcome=%s wait=%s exec=%s free=%d/%d",
		p.Manifest.ID, fnName, class, outcome,
		wait.Round(time.Millisecond), exec.Round(time.Millisecond), free, size)
}

// CallEntrypoint calls a named entrypoint (by manifest key OR by Lua function name directly).
func (m *LuaPluginManager) CallEntrypoint(pluginID, ep string, args map[string]any, profileID string) (any, error) {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPluginNotLoaded, pluginID)
	}

	// Resolve entrypoint alias from manifest, fall back to direct function name.
	fnName := ep
	if alias, ok := p.Manifest.Entrypoints[ep]; ok {
		fnName = alias
	}

	ctx, cancel := context.WithTimeout(context.Background(), entrypointTimeout(p.Manifest, ep, fnName))
	defer cancel()

	// Cron tasks and manual runs: background work, capped so it can't hold
	// every LState.
	const class = classBackground
	L, release, done, wait, err := acquireForCall(ctx, p, class)
	if err != nil {
		p.logCall(fnName, class, wait, 0, "busy")
		return nil, fmt.Errorf("plugin %q: pool acquire: %w", pluginID, err)
	}
	defer done()
	execStart := time.Now()
	outcome := "ok"
	defer func() { p.logCall(fnName, class, wait, time.Since(execStart), outcome) }()
	// Return L to the pool only when the call finished within ctx: after a
	// timeout the abandoned goroutine may still use it (and its scope), so it
	// is discarded instead (LuaPool.DiscardAndReplace).
	released := false
	sc := scopeOf(L)
	defer func() {
		if !released {
			// Strip globals the call leaked and reset the per-call scope before the
			// state goes back: its next user can be another profile.
			sc.resetNewGlobals(L)
			sc.reset()
			release()
		}
	}()

	// Per-call state lives in the LState's callScope, read live by the SDK
	// closures. requireProxy is forced for a video-flow entrypoint of a
	// VPN-optional plugin.
	scReqProxy, scProxyURL := m.pluginCallProxy(pluginID, ep)
	sc.set(ctx, profileID, scReqProxy, scProxyURL, nil)

	fn := L.GetGlobal(fnName)
	if fn == lua.LNil {
		return nil, fmt.Errorf("plugin %q: function %q not defined", pluginID, fnName)
	}

	luaArgs := mapToLuaTable(L, args)

	callErr, timedOut := callWithTimeout(ctx, L, fn, luaArgs)
	if timedOut {
		outcome = "timeout"
		p.Pool.DiscardAndReplace()
		p.recordDiscard()
		released = true
		return nil, fmt.Errorf("plugin %q %s: %w, esecuzione abbandonata", pluginID, fnName, ErrPluginTimeout)
	}
	if callErr != nil {
		outcome = "error"
		return nil, fmt.Errorf("plugin %q %s: %w", pluginID, fnName, callErr)
	}

	// Lua convention: return value, err_string
	errVal := L.Get(-1)
	ret := L.Get(-2)
	L.Pop(2)

	if errVal != lua.LNil {
		if errStr, ok := errVal.(lua.LString); ok && string(errStr) != "" {
			return nil, &PluginError{PluginID: pluginID, Fn: fnName, Msg: string(errStr)}
		}
	}
	return luaToGo(ret), nil
}

// CallEntrypointJSON is like CallEntrypoint but serialises the Lua return
// value straight to JSON.
func (m *LuaPluginManager) CallEntrypointJSON(pluginID, ep string, args map[string]any, profileID string) (json.RawMessage, error) {
	return m.CallEntrypointJSONCtx(context.Background(), pluginID, ep, args, profileID)
}

// CallEntrypointJSONCtx is CallEntrypointJSON bound to the caller's ctx, so
// the call stops when the request goes away. Coalesced reads are the
// exception: one call serves every identical request.
func (m *LuaPluginManager) CallEntrypointJSONCtx(ctx context.Context, pluginID, ep string, args map[string]any, profileID string) (json.RawMessage, error) {
	if coalescibleRead(ep) {
		if key, ok := readKey(pluginID, ep, profileID, args); ok {
			return m.readCo.do(key, func() (json.RawMessage, error) {
				return m.callEntrypointJSON(context.Background(), pluginID, ep, args, profileID, nil, false)
			})
		}
	}
	return m.callEntrypointJSON(ctx, pluginID, ep, args, profileID, nil, false)
}

// CallEntrypointJSONBackground is CallEntrypointJSON for work nobody waits
// on: it can't hold every LState, so a user request always finds one free.
func (m *LuaPluginManager) CallEntrypointJSONBackground(pluginID, ep string, args map[string]any, profileID string) (json.RawMessage, error) {
	return m.callEntrypointJSON(context.Background(), pluginID, ep, args, profileID, nil, true)
}

// CallEntrypointJSONWithProgress is CallEntrypointJSON with
// mycelium.progress(status, message) updates forwarded to onProgress while
// the plugin runs (used by resolve_stream).
func (m *LuaPluginManager) CallEntrypointJSONWithProgress(pluginID, ep string, args map[string]any, profileID string, onProgress ProgressFunc) (json.RawMessage, error) {
	return m.CallEntrypointJSONWithProgressCtx(context.Background(), pluginID, ep, args, profileID, onProgress)
}

// CallEntrypointJSONWithProgressCtx is CallEntrypointJSONWithProgress bound to
// the caller's ctx (see CallEntrypointJSONCtx).
func (m *LuaPluginManager) CallEntrypointJSONWithProgressCtx(ctx context.Context, pluginID, ep string, args map[string]any, profileID string, onProgress ProgressFunc) (json.RawMessage, error) {
	return m.callEntrypointJSON(ctx, pluginID, ep, args, profileID, onProgress, false)
}

func (m *LuaPluginManager) callEntrypointJSON(parent context.Context, pluginID, ep string, args map[string]any, profileID string, onProgress ProgressFunc, background bool) (json.RawMessage, error) {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrPluginNotLoaded, pluginID)
	}

	fnName := ep
	if alias, ok := p.Manifest.Entrypoints[ep]; ok {
		fnName = alias
	}

	ctx, cancel := context.WithTimeout(parent, entrypointTimeout(p.Manifest, ep, fnName))
	defer cancel()

	class := classOf(ep)
	if background {
		class = classBackground
	}
	L, release, done, wait, err := acquireForCall(ctx, p, class)
	if err != nil {
		p.logCall(fnName, class, wait, 0, "busy")
		return nil, fmt.Errorf("plugin %q: pool acquire: %w", pluginID, err)
	}
	defer done()
	execStart := time.Now()
	outcome := "ok"
	defer func() { p.logCall(fnName, class, wait, time.Since(execStart), outcome) }()
	// On timeout L may still be in use by the abandoned goroutine: discard it
	// instead of returning it (and skip scope.reset()).
	released := false
	sc := scopeOf(L)
	defer func() {
		if !released {
			// Strip globals the call leaked and reset the per-call scope before the
			// state goes back: its next user can be another profile.
			sc.resetNewGlobals(L)
			sc.reset()
			release()
		}
	}()

	// Per-call state goes into the LState's callScope, read live by the SDK.
	scReqProxy, scProxyURL := m.pluginCallProxy(pluginID, ep)
	sc.set(ctx, profileID, scReqProxy, scProxyURL, onProgress)

	fn := L.GetGlobal(fnName)
	if fn == lua.LNil {
		return nil, fmt.Errorf("plugin %q: function %q not defined", pluginID, fnName)
	}

	callErr, timedOut := callWithTimeout(ctx, L, fn, mapToLuaTable(L, args))
	if timedOut {
		outcome = "timeout"
		p.Pool.DiscardAndReplace()
		p.recordDiscard()
		released = true
		return nil, fmt.Errorf("plugin %q %s: %w, esecuzione abbandonata", pluginID, fnName, ErrPluginTimeout)
	}
	if callErr != nil {
		outcome = "error"
		return nil, fmt.Errorf("plugin %q %s: %w", pluginID, fnName, callErr)
	}

	errVal := L.Get(-1)
	ret := L.Get(-2)
	L.Pop(2)

	if errVal != lua.LNil {
		if errStr, ok := errVal.(lua.LString); ok && string(errStr) != "" {
			return nil, &PluginError{PluginID: pluginID, Fn: fnName, Msg: string(errStr)}
		}
	}
	if ret == lua.LNil {
		return json.RawMessage("null"), nil
	}
	var buf bytes.Buffer
	luaValueToJSON(&buf, ret)
	return buf.Bytes(), nil
}

// luaValueToJSON serialises a Lua value straight to JSON.
func luaValueToJSON(w *bytes.Buffer, v lua.LValue) {
	luaValueToJSONGuarded(w, v, newLuaConvGuard())
}

func luaValueToJSONGuarded(w *bytes.Buffer, v lua.LValue, g *luaConvGuard) {
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
		if !g.enter(val) {
			w.WriteString("null") // cycle / too deep / too many tables
			return
		}
		luaTableToJSON(w, val, g)
		g.leave(val)
	default:
		b, _ := json.Marshal(v.String())
		w.Write(b)
	}
}

// luaTableToJSON decides array vs object using the same heuristic as luaTableToGo.
func luaTableToJSON(w *bytes.Buffer, t *lua.LTable, g *luaConvGuard) {
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
			luaValueToJSONGuarded(w, t.RawGetInt(i), g)
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
		luaValueToJSONGuarded(w, v, g)
	})
	w.WriteByte('}')
}

// GetStatusBatch reads the runtime status of several plugins in one MGET;
// statusFallback derives one for a plugin that never set it.
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
	// Egress is the network-exit profile of the plugin's video flow
	// (managers.EgressProfiles).
	Egress string `json:"egress"`
	// DiscardedStates counts Lua states discarded after an entrypoint timeout
	// since load; a growing number flags a stuck task or entrypoint.
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

// DiscardedStates returns how many of pluginID's Lua states were discarded
// after a timeout since it was loaded (0 for an unknown plugin).
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

// statusFallback derives a status for a plugin that never called
// set_plugin_status: a running plugin with scheduled tasks and no successful
// run yet is still warming up ("syncing").
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

// SetStatus persists pluginID's runtime status to Redis (no TTL).
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

// VPNOptInSettingID is the synthetic setting injected for a manifest with
// vpn_optional: true (stored as lua:{pluginID}:global:vpn_enabled, off by
// default).
const VPNOptInSettingID = "vpn_enabled"

// pluginIDPattern whitelists manifest ids: lowercase alphanumerics with
// '.', '_' or '-' only between two of them. Ids end up in HTML attributes
// and in filesystem/Redis/setting keys, so every manifest load rejects
// anything else here.
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

// PluginEgress returns the egress-profile name of pluginID's video flow:
// an explicit lua:{id}:global:egress, else the legacy vpn_enabled toggle,
// else the manifest default ("direct" for direct_egress plugins, "warp"
// otherwise).
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
	// legacy: vpn_enabled=true meant "route video via WARP".
	if managers.Settings.GetString("lua:"+pluginID+":global:"+VPNOptInSettingID, "") == "true" {
		return "warp"
	}
	return def
}

// pluginCallProxy resolves the (requireProxy, proxyURL) of a call scope for
// entrypoint ep of pluginID:
//   - direct_egress without an explicit egress: direct; a vpn_optional
//     plugin still routes its video-flow entrypoints through PluginEgress,
//     matching the HLS proxy's egress;
//   - direct_egress with an explicit egress: every entrypoint uses it;
//   - otherwise the VPN covers every call.
//
// A selected but disabled egress yields (true, "") so the SDK blocks the
// call instead of going direct.
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

// shouldUseVPNForVideo reports whether the plugin's video flow is not direct.
func (m *LuaPluginManager) shouldUseVPNForVideo(pluginID string) bool {
	return m.PluginEgress(pluginID) != managers.EgressDirect
}

// ShouldUseVPNForVideo is the exported form of shouldUseVPNForVideo, used to
// tag proxied URLs with vpn=1.
func (m *LuaPluginManager) ShouldUseVPNForVideo(pluginID string) bool {
	return m.shouldUseVPNForVideo(pluginID)
}

// LuaManifestDownload is a manifest's optional `download:` section.
type LuaManifestDownload struct {
	Enabled bool `yaml:"enabled"`
	// PreferredHours is the window ("HH:MM-HH:MM", server local time, may wrap
	// midnight) when the source usually serves its best quality.
	PreferredHours string `yaml:"preferred_hours"`
	// Note is a fixed hint shown with the download options; a resolve can add a
	// live one through extra["download_notice"].
	Note string `yaml:"note"`
}

// DownloadConfig returns pluginID's download section (zero value when the
// plugin is unknown or doesn't declare one).
func (m *LuaPluginManager) DownloadConfig(pluginID string) LuaManifestDownload {
	m.mu.RLock()
	p, ok := m.plugins[pluginID]
	m.mu.RUnlock()
	if !ok {
		return LuaManifestDownload{}
	}
	return p.Manifest.Download
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
	return luaToGoGuarded(v, newLuaConvGuard())
}

func luaToGoGuarded(v lua.LValue, g *luaConvGuard) any {
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
		if !g.enter(val) {
			return nil // cycle / too deep / too many tables
		}
		defer g.leave(val)
		return luaTableToGo(val, g)
	default:
		return v.String()
	}
}

func luaTableToGo(t *lua.LTable, g *luaConvGuard) any {
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
			arr = append(arr, luaToGoGuarded(t.RawGetInt(i), g))
		}
		return arr
	}
	m := make(map[string]any)
	t.ForEach(func(k, v lua.LValue) {
		m[k.String()] = luaToGoGuarded(v, g)
	})
	return m
}

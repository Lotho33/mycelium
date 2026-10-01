package managers

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"log"
	"mycelium/internal/core"
)

const dockerSecretsDir = "/run/secrets"

var (
	configPath = core.AppPath("data", "config.json")
	envFile    = core.AppPath(".env")
)

type SettingsManager struct {
	data map[string]any
	mu   sync.RWMutex
}

var Settings = &SettingsManager{
	data: make(map[string]any),
}

// Load reads the settings, highest priority last applied:
func (s *SettingsManager) Load() {
	s.mu.Lock()
	defer s.mu.Unlock()

	merged := make(map[string]any)

	// 4. config.json
	if fileData, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(fileData, &merged); err != nil {
			log.Printf("Errore lettura config.json: %v", err)
		}
	}

	// 3. .env file
	if envData, err := os.ReadFile(envFile); err == nil {
		for line := range strings.SplitSeq(string(envData), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if key, val, ok := strings.Cut(line, "="); ok {
				merged[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(val), `"'`)
			}
		}
	}

	// 2. environment variables
	for key := range merged {
		if envVal := os.Getenv(strings.ToUpper(key)); envVal != "" {
			merged[key] = envVal
		}
	}

	// 1. Docker secrets
	if entries, err := os.ReadDir(dockerSecretsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				secretPath := filepath.Join(dockerSecretsDir, entry.Name())
				if secretVal, err := os.ReadFile(secretPath); err == nil {
					val := strings.TrimSpace(string(secretVal))
					if val != "" {
						merged[entry.Name()] = val
					}
				}
			}
		}
	}

	s.data = merged
}

// ResetAll wipes every persisted setting (factory reset): config.json becomes
// an empty object and is reloaded. Env vars and Docker secrets still apply;
// master_admin_hash is gone, so the next request goes to /setup.
func (s *SettingsManager) ResetAll() error {
	os.MkdirAll(filepath.Dir(configPath), 0755)
	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte("{}"), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	s.Load()
	return nil
}

// allowedKeyPrefixes whitelists keys writable via the settings API.
var (
	allowedKeyPrefixes = []string{
		"server_port",
		"server_host", // address clients reach the server at, for the proxy URLs
		// master_admin_hash is deliberately not writable here: only the one-shot
		// setup and POST /admin/password/change write it, through SaveInternal.
		"tmdb_bearer_token",
		"sync_interval_hours",
		"domain_interval_hours",
		"plugin_",           // plugin_<id>_disabled flags
		"enricher_bindings", // JSON map: enricher_id → []provider_id
		"mycelium.",         // per-plugin settings: {pluginID}_{key}
		"lua:",              // Lua plugin settings: lua:{pluginID}:global:{key}
		// No generic "pileus_" prefix: it would cover the master secret and the TLS
		// key. Writable pileus_* keys are listed by exact name in allowedExactKeys.
		"vpn_proxy_url",   // VPN proxy address, restored at boot
		"http_profile",    // upstream fetch path: "standard" forces direct fetches
		"prebuffer_",      // HLS pre-buffer: prebuffer_enabled/_segments_vod/_segments_live/_max_wait_ms/_max_bytes
		"egress_profiles", // JSON: network exit registry (egress.go)
		"egress_ipv6",     // "1"/"0": prefer IPv6 on direct connections
		"download_",       // offline downloads (internal/downloads)
	}
	allowedPrefixesMu sync.RWMutex

	// allowedExactKeys: keys writable only by exact name, where a prefix would
	// also match sensitive neighbours.
	allowedExactKeys = map[string]bool{
		"pileus_web_repo": true, // "owner/repo" of the Pileus web build
	}
)

// isAllowedKey reports whether key is in the allowlist.
func isAllowedKey(key string) bool {
	if allowedExactKeys[key] {
		return true
	}
	allowedPrefixesMu.RLock()
	defer allowedPrefixesMu.RUnlock()
	for _, prefix := range allowedKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// RegisterAllowedKeyPrefix lets plugins add their own config key prefixes during Init.
func RegisterAllowedKeyPrefix(prefix string) {
	allowedPrefixesMu.Lock()
	allowedKeyPrefixes = append(allowedKeyPrefixes, prefix)
	allowedPrefixesMu.Unlock()
}

func (s *SettingsManager) Save(newData map[string]any) error {
	for k := range newData {
		if !isAllowedKey(k) {
			return fmt.Errorf("chiave di configurazione non consentita: %q", k)
		}
	}
	return s.SaveInternal(newData)
}

// SaveInternal writes newData like Save but without the allowlist check,
// for the few trusted server-side paths (setup, password change). Never
// wire it to an endpoint that accepts arbitrary keys.
func (s *SettingsManager) SaveInternal(newData map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Start from the file's own data only, so the env/secrets overlay is never
	// persisted.
	fileData := make(map[string]any)
	if existing, err := os.ReadFile(configPath); err == nil {
		_ = json.Unmarshal(existing, &fileData)
	}
	maps.Copy(fileData, newData)

	os.MkdirAll(filepath.Dir(configPath), 0755)

	data, err := json.MarshalIndent(fileData, "", "    ")
	if err != nil {
		return err
	}

	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		log.Printf("Errore scrittura config tmp: %v", err)
		return err
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		log.Printf("Errore rename config: %v", err)
		os.Remove(tmpPath)
		return err
	}

	// Update in-memory runtime state (env/secrets overlay preserved).
	maps.Copy(s.data, newData)
	log.Println("⚙️ Configurazioni salvate correttamente.")
	return nil
}

func (s *SettingsManager) Get(key string, def any) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if val, ok := s.data[key]; ok {
		return val
	}
	return def
}

func (s *SettingsManager) GetString(key string, def string) string {
	val := s.Get(key, def)
	if str, ok := val.(string); ok {
		return str
	}
	return def
}

// GetBool reads a boolean setting through core.ParseBoolish ("1", "true",
// "yes", "on", any case, are true); unset or anything else gives def.
func (s *SettingsManager) GetBool(key string, def bool) bool {
	raw := s.GetString(key, "")
	if raw == "" {
		return def
	}
	return core.ParseBoolish(raw)
}

func (s *SettingsManager) MasterAdminHash() string {
	return s.GetString("master_admin_hash", "")
}

func (s *SettingsManager) IsSetupDone() bool {
	return s.MasterAdminHash() != ""
}

// GetAllAsStrings returns a string copy of all settings for passing to plugins.
func (s *SettingsManager) GetAllAsStrings() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		if str, ok := v.(string); ok {
			out[k] = str
		}
	}
	return out
}

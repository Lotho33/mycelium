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

// Load carica la configurazione seguendo la priorità.
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

	// 3. .env file (lettura base)
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

	// 2. Variabili d'ambiente (sovrascrive le chiavi esistenti)
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

// ResetAll wipes every persisted setting back to a blank slate — used by the
// dashboard's factory reset (Impostazioni → Zona pericolosa). Overwrites
// config.json with an empty object and reloads, so whatever the deployment's
// own env vars/docker secrets provide still applies (same as a genuinely
// fresh install would see), but every operator-configured value —
// master_admin_hash included, which is what sends the next request back to
// /setup — is gone.
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
		"server_host", // IP/hostname reale della macchina per gli URL proxy restituiti al player (vedi media_handler.go ResolveStream)
		// master_admin_hash NON è qui di proposito: era scrivibile verbatim (nessun
		// bcrypt, nessuna verifica della password attuale) tramite l'endpoint
		// generico POST /admin/settings/save — bastava il cookie di sessione
		// (rubabile via XSS/dispositivo condiviso) per piantare un hash a scelta
		// e ottenere un accesso admin persistente che sopravviveva a un cambio
		// password legittimo. Va scritta solo dal setup one-shot (setup.go) e da
		// POST /admin/password/change (admin_password.go), entrambi via
		// SaveInternal — mai tramite questo path generico.
		"tmdb_bearer_token",
		"sync_interval_hours",
		"domain_interval_hours",
		"plugin_",           // flag enable/disable plugin: plugin_<id>_disabled
		"enricher_bindings", // JSON map: enricher_id → []provider_id
		"mycelium.",         // per-plugin settings: {pluginID}_{key}
		"lua:",              // Lua plugin global settings: lua:{pluginID}:global:{key}
		"pileus_",           // Pileus device auth settings: pileus_master_pin_hash, etc.
		// (pileus_web_repo — "owner/repo" del repo GitHub del build web di
		// Pileus, usato da POST /admin/pileus-web/update — rientra già in
		// questo prefisso, nessuna voce a parte necessaria)
		"vpn_proxy_url",   // stato on/off del routing VPN, ripristinato al boot
		"http_profile",    // profilo fetch upstream: "standard" (default, net/http) | "browser" (profilo TLS/H2 mainstream per compatibilità CDN)
		"prebuffer_",      // pre-buffer del flusso HLS: prebuffer_enabled / _segments_vod / _segments_live / _max_wait_ms / _max_bytes
		"egress_profiles", // JSON: registry delle uscite di rete (vedi egress.go)
	}
	allowedPrefixesMu sync.RWMutex
)

// isAllowedKey verifica che la chiave rientri nell'allowlist.
func isAllowedKey(key string) bool {
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

// SaveInternal scrive newData esattamente come Save, ma SENZA il controllo
// isAllowedKey — per i pochi path server-side fidati che devono scrivere
// chiavi non esposte alla superficie generica /admin/settings/save (oggi:
// master_admin_hash, dal setup one-shot e da POST /admin/password/change).
// Non collegare mai questa funzione a un endpoint che accetta chiavi/valori
// arbitrari dal chiamante.
func (s *SettingsManager) SaveInternal(newData map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Read only file-origin data to avoid persisting env/secrets overlay.
	// If the file doesn't exist yet we start from an empty map.
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

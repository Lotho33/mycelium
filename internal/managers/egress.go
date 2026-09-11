package managers

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"mycelium/internal/core"
)

// Egress profiles = the menu of network exits a plugin's traffic can take.
// "direct" is built-in (the box's own IP); the rest are proxies (WARP via
// microwarp, or any socks5/http proxy the operator adds). A plugin picks one
// by name in the dashboard; disabled profiles aren't selectable and any plugin
// still pointing at one falls back to "direct" (fail-open for scraping — a
// VPN-*required* plugin is a separate concept, see DirectEgress).
//
// Stored as one JSON blob under the "egress_profiles" setting.

const egressSettingKey = "egress_profiles"

// EgressDirect is the reserved name of the built-in no-proxy exit.
const EgressDirect = "direct"

type EgressProfile struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`      // "warp" | "proxy" | "wireproxy"
	ProxyURL string `json:"proxy_url"` // socks5://… | http://… ; empty only for the built-in "direct"
	Enabled  bool   `json:"enabled"`
	// Port is the host port a "wireproxy" sidecar publishes its SOCKS5 on
	// (0 for every other kind). Persisted so the port survives a restart.
	Port int `json:"port,omitempty"`
}

var egressMu sync.Mutex

// InitEgressDefaults seeds a "warp" profile from VPN_PROXY_URL / the persisted
// vpn_proxy_url the first time, so an existing deployment keeps working. Safe
// to call once at boot after Settings is ready.
func InitEgressDefaults(envProxyURL string) {
	egressMu.Lock()
	defer egressMu.Unlock()
	list := loadEgressLocked()
	for _, p := range list {
		if p.Name == "warp" {
			return // already have one
		}
	}
	url := strings.TrimSpace(envProxyURL)
	if url == "" && Settings != nil {
		url = Settings.GetString("vpn_proxy_url", "")
	}
	if url == "" {
		return
	}
	list = append(list, EgressProfile{Name: "warp", Kind: "warp", ProxyURL: url, Enabled: true})
	saveEgressLocked(list)
}

func loadEgressLocked() []EgressProfile {
	if Settings == nil {
		return nil
	}
	raw := Settings.GetString(egressSettingKey, "")
	if raw == "" {
		return nil
	}
	var out []EgressProfile
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

func saveEgressLocked(list []EgressProfile) {
	if Settings == nil {
		return
	}
	b, _ := json.Marshal(list)
	if err := Settings.Save(map[string]any{egressSettingKey: string(b)}); err != nil {
		fmt.Fprintf(os.Stderr, "[egress] save failed: %v\n", err)
	}
}

// EgressProfiles returns the built-in "direct" first, then the stored ones
// (sorted by name).
func EgressProfiles() []EgressProfile {
	egressMu.Lock()
	list := loadEgressLocked()
	egressMu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	out := make([]EgressProfile, 0, len(list)+1)
	out = append(out, EgressProfile{Name: EgressDirect, Kind: EgressDirect, Enabled: true})
	return append(out, list...)
}

// UpsertEgressProfile adds or replaces a profile. "direct" is reserved.
func UpsertEgressProfile(p EgressProfile) error {
	p.Name = strings.TrimSpace(strings.ToLower(p.Name))
	if p.Name == "" || p.Name == EgressDirect {
		return fmt.Errorf("nome egress non valido")
	}
	p.ProxyURL = strings.TrimSpace(p.ProxyURL)
	if p.ProxyURL == "" {
		return fmt.Errorf("proxy_url obbligatorio")
	}
	if !strings.Contains(p.ProxyURL, "://") {
		p.ProxyURL = "socks5://" + p.ProxyURL
	}
	if p.Kind == "" {
		p.Kind = "proxy"
	}
	egressMu.Lock()
	defer egressMu.Unlock()
	list := loadEgressLocked()
	for i := range list {
		if list[i].Name == p.Name {
			list[i] = p
			saveEgressLocked(list)
			return nil
		}
	}
	saveEgressLocked(append(list, p))
	return nil
}

// SetEgressEnabled flips a profile's enabled flag. For a wireproxy-backed
// profile it also brings the sidecar up or down to match.
func SetEgressEnabled(name string, enabled bool) error {
	name = strings.ToLower(strings.TrimSpace(name))
	egressMu.Lock()
	list := loadEgressLocked()
	var found *EgressProfile
	for i := range list {
		if list[i].Name == name {
			list[i].Enabled = enabled
			found = &list[i]
			saveEgressLocked(list)
			break
		}
	}
	egressMu.Unlock()
	if found == nil {
		return fmt.Errorf("egress %q non trovato", name)
	}
	if found.Kind == "wireproxy" {
		if err := ApplyWireproxyEgress(*found); err != nil {
			return fmt.Errorf("profilo salvato ma il sidecar wireproxy non ha risposto: %w", err)
		}
	}
	return nil
}

// DeleteEgressProfile removes one (not "direct"). A wireproxy-backed profile
// also stops the sidecar and drops the stored .conf.
func DeleteEgressProfile(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == EgressDirect {
		return fmt.Errorf("non si può eliminare l'uscita diretta")
	}
	egressMu.Lock()
	list := loadEgressLocked()
	wasWireproxy := false
	kept := list[:0]
	for _, p := range list {
		if p.Name == name {
			wasWireproxy = p.Kind == "wireproxy"
			continue
		}
		kept = append(kept, p)
	}
	saveEgressLocked(kept)
	egressMu.Unlock()
	if wasWireproxy {
		return DeleteWireproxyEgress(name)
	}
	return nil
}

// UpsertWireproxyEgress saves a WireGuard config, registers (or updates) a
// wireproxy-backed egress profile under `name` — assigning it a stable host
// port — and brings its dedicated sidecar up. Any number of these can coexist.
func UpsertWireproxyEgress(name, rawConf string) error {
	// Store the sanitised form as the canonical Name so the registry key, the
	// conf filename and the container name can never drift apart.
	name = sanitizeEgressName(name)
	if name == "" || name == EgressDirect {
		return fmt.Errorf("nome egress non valido (usa lettere, cifre, - o _)")
	}
	if err := SaveWireproxyConf(name, rawConf); err != nil {
		return err
	}
	egressMu.Lock()
	list := loadEgressLocked()
	port, perr := assignWireproxyPort(list, name)
	if perr != nil {
		egressMu.Unlock()
		return perr
	}
	p := EgressProfile{
		Name:     name,
		Kind:     "wireproxy",
		Port:     port,
		ProxyURL: fmt.Sprintf("socks5://127.0.0.1:%d", port),
		Enabled:  true,
	}
	replaced := false
	for i := range list {
		if list[i].Name == name {
			list[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, p)
	}
	saveEgressLocked(list)
	egressMu.Unlock()

	if err := ApplyWireproxyEgress(p); err != nil {
		return fmt.Errorf("config salvata ma il sidecar wireproxy non è partito: %w", err)
	}
	return nil
}

// ReapplyWireproxyAtBoot brings every wireproxy sidecar back to the state the
// stored registry says it should be in — call once after Settings is ready so a
// server restart doesn't leave an enabled WireGuard egress dead. It also
// migrates profiles saved before per-egress ports existed (Port == 0): assign a
// port + fix the ProxyURL if the .conf is still there, disable it otherwise.
func ReapplyWireproxyAtBoot() {
	// egressMu also guards ResolveEgressProxy — on the hot path for every
	// VPN-routed plugin call. A panic here without a deferred Unlock would
	// wedge it forever (every plugin request blocks, silently, from then on)
	// instead of just skipping this boot-time re-apply.
	var list []EgressProfile
	func() {
		defer core.Guard("managers/reapply-wireproxy-boot")
		egressMu.Lock()
		defer egressMu.Unlock()
		list = loadEgressLocked()
		dirty := false
		for i := range list {
			if list[i].Kind != "wireproxy" || (list[i].Port >= wireproxyPortBase && list[i].Port <= wireproxyPortMax) {
				continue
			}
			if WireproxyConfigured(list[i].Name) {
				if port, err := assignWireproxyPort(list, list[i].Name); err == nil {
					list[i].Port = port
					list[i].ProxyURL = fmt.Sprintf("socks5://127.0.0.1:%d", port)
					dirty = true
					continue
				}
			}
			list[i].Enabled = false // no conf / no free port → don't try to route through it
			dirty = true
		}
		if dirty {
			saveEgressLocked(list)
		}
	}()

	for _, p := range list {
		if p.Kind != "wireproxy" {
			continue
		}
		if err := ApplyWireproxyEgress(p); err != nil {
			fmt.Fprintf(os.Stderr, "[egress] wireproxy %q boot re-apply: %v\n", p.Name, err)
		}
	}
}

// ResolveEgressProxy maps an egress name to a proxy URL for use by the cobweb browser service /
// the HLS relay. Returns:
//   - ("", true)  for "direct" or an empty/unknown name → go direct
//   - (url, true) for an enabled profile
//   - ("", false) for a profile that exists but is DISABLED → caller should
//     fail-closed for a VPN-required plugin, or fall back to direct otherwise
func ResolveEgressProxy(name string) (proxyURL string, ok bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" || name == EgressDirect {
		return "", true
	}
	egressMu.Lock()
	list := loadEgressLocked()
	egressMu.Unlock()
	for _, p := range list {
		if p.Name == name {
			if !p.Enabled {
				return "", false
			}
			return p.ProxyURL, true
		}
	}
	return "", true // unknown name → direct
}

package engine

import (
	"fmt"
	"net/http"
	"path/filepath"

	lua "github.com/yuin/gopher-lua"
)

// NewPluginTestState loads the plugin in dir into a fresh Lua state exactly
// as a pooled production state is built (locked-down stdlib, the real
// mycelium.* SDK, the plugin's modules preloaded, init.lua run), with every
// network call going through client — the plugin test tool (cmd/plugintest)
// hands it a client that answers from canned routes. No Redis (the cache
// stays empty), no browser service. Returns the manifest id too.
func NewPluginTestState(dir string, client *http.Client) (*lua.LState, string, error) {
	mf, err := ReadLuaManifest(dir)
	if err != nil {
		return nil, "", fmt.Errorf("manifest: %w", err)
	}
	opts := SDKOpts{
		PluginID:        mf.ID,
		PluginDir:       dir,
		HTTPClient:      client,
		ProxyHTTPClient: client,
		ProxyClientFor:  func(string) *http.Client { return client },
	}
	p := &LuaPool{
		script:  filepath.Join(dir, "init.lua"),
		setupFn: func(L *lua.LState) { RegisterSDK(L, opts, scopeOf(L)) },
		gens:    map[*lua.LState]uint64{},
	}
	L, err := p.newState()
	if err != nil {
		return nil, "", err
	}
	return L, mf.ID, nil
}

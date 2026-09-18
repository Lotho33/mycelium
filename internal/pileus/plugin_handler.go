package pileus

import (
	"context"
	"log"

	"mycelium/internal/engine"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PluginHandler implements gen.PluginServiceServer. GetPluginSettings/
// SavePluginSetting used to live here — per-profile plugin settings Pileus
// could read/write over gRPC. Removed: zero shipped plugins ever declared a
// "scope: user" field, so it was RPC surface with nothing behind it. Every
// setting is admin-configured now, from mycelium's own dashboard.
// UnimplementedPluginServiceServer answers Unimplemented for those two RPCs
// on a stale client — no proto change needed to drop them here.
type PluginHandler struct {
	gen.UnimplementedPluginServiceServer
}

func NewPluginHandler() *PluginHandler { return &PluginHandler{} }

// TriggerRefresh runs a plugin's live_refresh-marked task(s) on demand —
// e.g. sport's refresh_matches, so a user looking at "Live Ora" isn't stuck
// waiting for the next cron tick. Lua-only for now: no gRPC plugin manifest
// declares an equivalent yet, and none is currently installed in this
// deployment. A gRPC plugin ID returns Unimplemented rather than silently
// no-op'ing.
func (h *PluginHandler) TriggerRefresh(ctx context.Context, req *gen.TriggerRefreshRequest) (*gen.TriggerRefreshResponse, error) {
	if req.PluginId == "" {
		return nil, status.Error(codes.InvalidArgument, "plugin_id required")
	}
	if !engine.LuaPlugins.IsOperational(req.PluginId) {
		return nil, status.Errorf(codes.NotFound, "plugin %q not found or not ready", req.PluginId)
	}
	if !engine.LuaPlugins.Has(req.PluginId) {
		return nil, status.Errorf(codes.Unimplemented, "plugin %q does not support manual refresh", req.PluginId)
	}
	ok, message := engine.LuaPlugins.TriggerLiveRefresh(req.PluginId)
	if !ok {
		log.Printf("[pileus/plugin] TriggerRefresh %s: %s", req.PluginId, message)
	}
	return &gen.TriggerRefreshResponse{Ok: ok, Message: message}, nil
}

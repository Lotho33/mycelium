package pileus

import (
	"context"
	"strconv"
	"sync/atomic"

	"mycelium/internal/managers"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
)

// RemoteInfo is where the server answers on its tailnet (internal/remote),
// zero while remote access is off or not connected.
type RemoteInfo struct {
	Host string // Tailscale IPv4
	FQDN string // MagicDNS name
}

var remoteInfoFn atomic.Pointer[func() RemoteInfo]

// SetRemoteInfo installs the source of GetServerInfo's tailnet address (nil
// removes it).
func SetRemoteInfo(f func() RemoteInfo) {
	if f == nil {
		remoteInfoFn.Store(nil)
		return
	}
	remoteInfoFn.Store(&f)
}

func currentRemoteInfo() RemoteInfo {
	if f := remoteInfoFn.Load(); f != nil {
		return (*f)()
	}
	return RemoteInfo{}
}

// GetServerInfo hands a paired device (the auth interceptor already checked
// its JWT) the server's tailnet address, so a native client can reach it
// away from home while its own Tailscale app is on. Never on the
// unauthenticated /pileus/info.
func (h *AuthHandler) GetServerInfo(ctx context.Context, _ *gen.GetServerInfoRequest) (*gen.ServerInfoResponse, error) {
	if _, err := h.deviceFromCtx(ctx); err != nil {
		return nil, err
	}
	ri := currentRemoteInfo()
	resp := &gen.ServerInfoResponse{}
	if ri.Host == "" {
		return resp, nil
	}
	resp.RemoteHost = ri.Host
	resp.RemoteFqdn = ri.FQDN
	resp.RemoteGrpcPort = portSetting("pileus_grpc_port", 50051)
	resp.RemoteHttpPort = portSetting("server_port", 8000)
	return resp, nil
}

func portSetting(key string, def int32) int32 {
	p, err := strconv.Atoi(managers.Settings.GetString(key, ""))
	if err != nil || p <= 0 || p > 65535 {
		return def
	}
	return int32(p)
}

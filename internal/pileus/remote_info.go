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
	grpc, _ := strconv.Atoi(managers.Settings.GRPCPort())
	http, _ := strconv.Atoi(managers.Settings.ServerPort())
	resp.RemoteGrpcPort, resp.RemoteHttpPort = int32(grpc), int32(http)
	return resp, nil
}

package managers

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

// dockerSocketPath is hardcoded, matching the rest of the repo's
// low-configurability style — the socket always lives at the standard
// location. Container names ("microwarp", "redis") are likewise hardcoded to
// their fixed container_name in both docker-compose.yml and docker-compose.dev.yml.
const dockerSocketPath = "/var/run/docker.sock"

// dockerClient is a minimal HTTP client dialing the Docker Engine API over
// its Unix socket. No official SDK: github.com/docker/docker/client pulls a
// large dependency tree for what here is a single read-only endpoint — same
// reasoning as the hand-rolled SOCKS5/HTTP-CONNECT dialer in
// internal/core/proxy_dialer.go instead of a full proxy library.
var dockerClient = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", dockerSocketPath)
		},
	},
}

// dockerStatsResponse mirrors only the fields we need from the Docker Engine
// API's GET /containers/{id}/stats?stream=false response — the full response
// has many more (network, blkio, pids...) that we don't use.
type dockerStatsResponse struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage  uint64   `json:"total_usage"`
			PercpuUsage []uint64 `json:"percpu_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs     uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
	} `json:"memory_stats"`
}

// DockerContainerStats holds resource usage for a sidecar container, read
// from the Docker Engine API (requires /var/run/docker.sock mounted into
// this container — see docker-compose.yml for the security trade-off note).
type DockerContainerStats struct {
	Available  bool
	CPUPercent float64
	MemBytes   int64
	MemLimit   int64
}

// GetContainerStats queries Docker for containerName's current resource
// usage. Available is false on any failure (socket not mounted, Docker
// unreachable, container not found/not running) — never an error the caller
// needs to handle, matching the rest of the admin status endpoint's
// graceful-degradation style.
func GetContainerStats(containerName string) DockerContainerStats {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/"+containerName+"/stats?stream=false", nil)
	if err != nil {
		return DockerContainerStats{}
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return DockerContainerStats{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DockerContainerStats{}
	}

	var s dockerStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return DockerContainerStats{}
	}

	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(s.CPUStats.SystemCPUUsage) - float64(s.PreCPUStats.SystemCPUUsage)
	onlineCPUs := float64(s.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	cpuPct := 0.0
	if systemDelta > 0 && cpuDelta >= 0 {
		cpuPct = (cpuDelta / systemDelta) * onlineCPUs * 100
	}

	return DockerContainerStats{
		Available:  true,
		CPUPercent: cpuPct,
		MemBytes:   int64(s.MemoryStats.Usage),
		MemLimit:   int64(s.MemoryStats.Limit),
	}
}

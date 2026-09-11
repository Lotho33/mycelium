package managers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Container lifecycle control over the same Docker Engine API socket
// docker_stats.go reads from. Used by the egress registry to bring the
// wireproxy sidecar up/down when its WireGuard profile is added, toggled or
// removed (see wireproxy.go). Requires /var/run/docker.sock mounted — write
// access to it is root-equivalent on the host, same trade-off already
// accepted for the metrics reads.

// containerAction issues POST /containers/{name}/{action} (start|stop|restart).
// A 304 (already in that state) is treated as success. Returns a descriptive
// error otherwise so the caller can surface it in the dashboard.
func containerAction(name, action string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/containers/"+name+"/"+action, nil)
	if err != nil {
		return err
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", action, name, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("container %q non trovato (è nel docker-compose?)", name)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("docker %s %s: HTTP %d %s", action, name, resp.StatusCode, string(body))
	}
}

// RestartContainer restarts a container by name (Docker's restart also starts
// a stopped one).
func RestartContainer(name string) error { return containerAction(name, "restart") }

// ─── create / remove (dynamic sidecars — Fase C) ────────────────────────────

// ContainerSpec is the minimal subset of the Docker create payload the egress
// sidecars need.
type ContainerSpec struct {
	Name          string
	Image         string
	Entrypoint    []string // "" → keep the image's own ENTRYPOINT
	Cmd           []string
	Labels        map[string]string
	Binds         []string          // "src:/dest[:ro]" — src may be a named volume
	Ports         map[string]string // containerPort ("1080/tcp") → host "127.0.0.1:1082"
	RestartPolicy string            // "" | "no" | "unless-stopped" | "always"
}

// SelfImageRef returns the image reference of mycelium's own container
// ("<registry>/mycelium:latest" & co.), read from the
// Docker API. Used as the default image for the wireproxy egress sidecars —
// it's already on the box and pulls with the same creds mycelium itself did.
// "" on any failure (caller falls back).
func SelfImageRef() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// container_name in docker-compose.prod.yml — same assumption as
	// GetContainerStats. os.Hostname() is unreliable under network_mode: host.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/mycelium/json", nil)
	if err != nil {
		return ""
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var out struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	return out.Config.Image
}

// ContainerExists reports whether a container with that name is present (any
// state). false on any lookup failure.
func ContainerExists(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+name+"/json", nil)
	if err != nil {
		return false
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// imageAvailable reports whether the image is already present locally.
func imageAvailable(image string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/images/"+image+"/json", nil)
	if err != nil {
		return false
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// PullImage pulls image unless it is already local. Blocks until the pull
// stream ends (can take a while on a cold box), so it gets its own long ctx.
func PullImage(image string) error {
	if imageAvailable(image) {
		return nil
	}
	repo, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		repo, tag = image[:i], image[i+1:]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/images/create?fromImage="+repo+"&tag="+tag, nil)
	if err != nil {
		return err
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pull %s: HTTP %d %s", image, resp.StatusCode, string(body))
	}
	_, _ = io.Copy(io.Discard, resp.Body) // drain the progress stream to completion
	return nil
}

// CreateContainer creates (does not start) a container from spec. Pulls the
// image first if missing.
func CreateContainer(spec ContainerSpec) error {
	if err := PullImage(spec.Image); err != nil {
		return err
	}

	portBindings := map[string][]map[string]string{}
	exposed := map[string]struct{}{}
	for cPort, host := range spec.Ports {
		hIP, hPort := "127.0.0.1", host
		if i := strings.LastIndex(host, ":"); i >= 0 {
			hIP, hPort = host[:i], host[i+1:]
		}
		portBindings[cPort] = []map[string]string{{"HostIp": hIP, "HostPort": hPort}}
		exposed[cPort] = struct{}{}
	}

	rp := spec.RestartPolicy
	if rp == "" {
		rp = "no"
	}
	body := map[string]any{
		"Image":        spec.Image,
		"Cmd":          spec.Cmd,
		"Labels":       spec.Labels,
		"ExposedPorts": exposed,
		// Drop any healthcheck inherited from the image — a sidecar running the
		// mycelium image would inherit its `wget localhost:8000` probe and sit
		// forever "unhealthy".
		"Healthcheck": map[string]any{"Test": []string{"NONE"}},
		"HostConfig": map[string]any{
			"Binds":         spec.Binds,
			"PortBindings":  portBindings,
			"RestartPolicy": map[string]string{"Name": rp},
		},
	}
	if len(spec.Entrypoint) > 0 {
		body["Entrypoint"] = spec.Entrypoint
	}
	b, _ := json.Marshal(body)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://docker/containers/create?name="+spec.Name, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := dockerClient.Do(req)
	if err != nil {
		return fmt.Errorf("create %s: %w", spec.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("create %s: HTTP %d %s", spec.Name, resp.StatusCode, string(rb))
	}
	return nil
}

// RemoveContainer force-removes a container by name (and its anonymous
// volumes). A 404 is success — it is already gone.
func RemoveContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		"http://docker/containers/"+name+"?force=1&v=1", nil)
	if err != nil {
		return err
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("remove %s: HTTP %d %s", name, resp.StatusCode, string(rb))
	}
}

// StopContainer stops a running container by name (no-op if already stopped).
func StopContainer(name string) error { return containerAction(name, "stop") }

// StartContainer starts a stopped container by name (no-op if already running).
func StartContainer(name string) error { return containerAction(name, "start") }

// ContainerRunning reports whether the named container exists and is running.
// false on any lookup failure (socket missing, container absent) — matching
// docker_stats.go's graceful-degradation style.
func ContainerRunning(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/"+name+"/json", nil)
	if err != nil {
		return false
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var out struct {
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	return out.State.Running
}

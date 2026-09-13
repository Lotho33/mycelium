package managers

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"mycelium/internal/core"
)

// These tests exercise the in-process subprocess supervisor (start/stop/crash
// detection/registry) using a fake "wireproxy" — a tiny shell script — instead
// of the real binary: the CI container has neither the real wireproxy binary
// nor a working WireGuard peer to dial, but none of that is needed to test
// process supervision.

// withTempWireproxyDir points wireproxyDir()/wireproxyConfPath() at a
// throwaway directory for the duration of one test.
func withTempWireproxyDir(t *testing.T) {
	t.Helper()
	old := core.BasePath
	core.BasePath = t.TempDir()
	t.Cleanup(func() { core.BasePath = old })
}

// withFakeWireproxyBin points wireproxyBinPath at path for the duration of
// one test.
func withFakeWireproxyBin(t *testing.T, path string) {
	t.Helper()
	old := wireproxyBinPath
	wireproxyBinPath = path
	t.Cleanup(func() { wireproxyBinPath = old })
}

// writeFakeConf drops a placeholder .conf on disk — wireproxyStartProcess only
// checks the file exists, it never parses it (that's normalizeWireguardConf's
// job, exercised separately).
func writeFakeConf(t *testing.T, name string) {
	t.Helper()
	if err := os.MkdirAll(wireproxyDir(), 0o755); err != nil {
		t.Fatalf("mkdir wireproxy dir: %v", err)
	}
	if err := os.WriteFile(wireproxyConfPath(name), []byte("fake conf\n"), 0o600); err != nil {
		t.Fatalf("write fake conf: %v", err)
	}
}

// writeFakeScript writes an executable shell script and returns its path.
func writeFakeScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake script %s: %v", name, err)
	}
	return path
}

// sleeperScript runs until it receives SIGTERM (a well-behaved long-running
// "wireproxy"). Note: real wireproxy v1.1.3 only traps SIGINT/SIGQUIT itself —
// an un-handled SIGTERM kills it via the OS default disposition, which is
// exactly what wireproxyStopProcess relies on. This fake instead traps it
// explicitly so the test doesn't depend on shell-specific default-signal
// timing during a tight sleep loop.
func sleeperScript(t *testing.T) string {
	return writeFakeScript(t, "fake-wireproxy-sleep.sh", "#!/bin/sh\ntrap 'exit 0' TERM\nwhile true; do sleep 0.05; done\n")
}

// stubbornScript ignores SIGTERM outright, forcing wireproxyStopProcess down
// its SIGKILL fallback path.
func stubbornScript(t *testing.T) string {
	return writeFakeScript(t, "fake-wireproxy-stubborn.sh", "#!/bin/sh\ntrap '' TERM\nwhile true; do sleep 0.05; done\n")
}

// crashScript exits immediately with a non-zero status (simulates wireproxy
// failing right after boot — bad config, unreachable peer, …).
func crashScript(t *testing.T) string {
	return writeFakeScript(t, "fake-wireproxy-crash.sh", "#!/bin/sh\nexit 7\n")
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %v", timeout)
	}
}

func TestWireproxyStartStopRunning(t *testing.T) {
	withTempWireproxyDir(t)
	withFakeWireproxyBin(t, sleeperScript(t))
	writeFakeConf(t, "toggle")

	if wireproxyIsRunning("toggle") {
		t.Fatal("should not be running before start")
	}
	if err := wireproxyStartProcess("toggle"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !wireproxyIsRunning("toggle") {
		t.Fatal("should be running right after start")
	}

	wireproxyStopProcess("toggle")
	if wireproxyIsRunning("toggle") {
		t.Fatal("should not be running after stop")
	}
}

func TestWireproxyStartProcess_MissingConf(t *testing.T) {
	withTempWireproxyDir(t)
	withFakeWireproxyBin(t, sleeperScript(t))

	if err := wireproxyStartProcess("ghost"); err == nil {
		t.Fatal("expected an error when the .conf is missing")
	}
	if wireproxyIsRunning("ghost") {
		t.Fatal("nothing should be tracked as running")
	}
}

func TestWireproxyCrashDetection(t *testing.T) {
	withTempWireproxyDir(t)
	withFakeWireproxyBin(t, crashScript(t))
	writeFakeConf(t, "crashy")

	if err := wireproxyStartProcess("crashy"); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The crash happens asynchronously (fork+exec+exit, then our Wait()
	// goroutine reaps it) — poll instead of asserting immediately.
	waitUntil(t, 2*time.Second, func() bool { return !wireproxyIsRunning("crashy") })

	// No auto-restart: it must stay down until something explicitly starts
	// it again (matches the old sidecars' RestartPolicy: "no").
	time.Sleep(50 * time.Millisecond)
	if wireproxyIsRunning("crashy") {
		t.Fatal("a crashed process must not auto-restart")
	}
}

func TestWireproxyConcurrentProfiles(t *testing.T) {
	withTempWireproxyDir(t)
	withFakeWireproxyBin(t, sleeperScript(t))
	writeFakeConf(t, "alpha")
	writeFakeConf(t, "beta")

	if err := wireproxyStartProcess("alpha"); err != nil {
		t.Fatalf("start alpha: %v", err)
	}
	if err := wireproxyStartProcess("beta"); err != nil {
		t.Fatalf("start beta: %v", err)
	}
	if !wireproxyIsRunning("alpha") || !wireproxyIsRunning("beta") {
		t.Fatal("both profiles should be running independently")
	}

	wireproxyStopProcess("alpha")
	if wireproxyIsRunning("alpha") {
		t.Fatal("alpha should be stopped")
	}
	if !wireproxyIsRunning("beta") {
		t.Fatal("stopping alpha must not affect beta")
	}
	wireproxyStopProcess("beta")
	if wireproxyIsRunning("beta") {
		t.Fatal("beta should be stopped")
	}
}

func TestWireproxyStop_KillFallback(t *testing.T) {
	withTempWireproxyDir(t)
	withFakeWireproxyBin(t, stubbornScript(t))
	writeFakeConf(t, "stubborn")

	old := wireproxyStopTimeout
	wireproxyStopTimeout = 100 * time.Millisecond
	t.Cleanup(func() { wireproxyStopTimeout = old })

	if err := wireproxyStartProcess("stubborn"); err != nil {
		t.Fatalf("start: %v", err)
	}
	start := time.Now()
	wireproxyStopProcess("stubborn")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stop took too long (%v) — kill fallback not firing", elapsed)
	}
	if wireproxyIsRunning("stubborn") {
		t.Fatal("still running after the kill fallback")
	}
}

func TestDeleteWireproxyEgress_StopsProcessAndRemovesConf(t *testing.T) {
	withTempWireproxyDir(t)
	withFakeWireproxyBin(t, sleeperScript(t))
	writeFakeConf(t, "gone")

	if err := wireproxyStartProcess("gone"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := DeleteWireproxyEgress("gone"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if wireproxyIsRunning("gone") {
		t.Fatal("still running after delete")
	}
	if _, err := os.Stat(wireproxyConfPath("gone")); !os.IsNotExist(err) {
		t.Fatal("conf file not removed")
	}
}

func TestApplyWireproxyEgress_DisableStopsProcess(t *testing.T) {
	withTempWireproxyDir(t)
	withFakeWireproxyBin(t, sleeperScript(t))
	writeFakeConf(t, "disabler")

	p := EgressProfile{Name: "disabler", Kind: "wireproxy", Enabled: true, Port: wireproxyPortBase}
	if err := ApplyWireproxyEgress(p); err != nil {
		t.Fatalf("apply enabled: %v", err)
	}
	if !WireproxyRunning("disabler") {
		t.Fatal("expected running once enabled+configured")
	}

	p.Enabled = false
	if err := ApplyWireproxyEgress(p); err != nil {
		t.Fatalf("apply disabled: %v", err)
	}
	if WireproxyRunning("disabler") {
		t.Fatal("expected stopped once disabled")
	}
}

// TestReapplyWireproxyAtBoot_RestartsEnabledProfile is the ReapplyWireproxyAtBoot
// equivalent required by the task: given a profile saved+enabled on disk but
// with an empty in-process registry (as it would be right after a mycelium
// restart — subprocesses die with their parent, unlike the old Docker
// sidecars), the boot re-apply must start it back up from the stored .conf.
func TestReapplyWireproxyAtBoot_RestartsEnabledProfile(t *testing.T) {
	withTempSettings(t)
	withTempWireproxyDir(t)
	withFakeWireproxyBin(t, sleeperScript(t))

	validConf := "[Interface]\nPrivateKey = aGVsbG8td29ybGQtcHJpdmF0ZS1rZXktMzJieXRlcw==\n[Peer]\nPublicKey = c2VydmVyLXB1YmxpYy1rZXktMzJieXRlcy1oZWxsbw==\nEndpoint = 193.32.1.1:51820\n"
	if err := UpsertWireproxyEgress("bootprofile", validConf); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !wireproxyIsRunning("bootprofile") {
		t.Fatal("expected running right after upsert")
	}

	// Simulate a mycelium restart: the in-process registry is gone (a fresh
	// process starts with an empty map) even though the egress profile and
	// its .conf are still on disk. Kill the old process directly first so the
	// test doesn't leak it.
	wireproxyRegMu.Lock()
	old := wireproxyReg["bootprofile"]
	delete(wireproxyReg, "bootprofile")
	wireproxyRegMu.Unlock()
	if old != nil {
		_ = old.cmd.Process.Kill()
	}
	if wireproxyIsRunning("bootprofile") {
		t.Fatal("registry wipe didn't take")
	}

	ReapplyWireproxyAtBoot()

	if !wireproxyIsRunning("bootprofile") {
		t.Fatal("ReapplyWireproxyAtBoot did not restart the enabled profile")
	}
	wireproxyStopProcess("bootprofile")
}

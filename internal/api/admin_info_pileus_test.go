package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mycelium/internal/core"
)

// The dashboard's iOS/web-app card shows which Pileus web build is installed,
// read from data/pileus-web/version.json.
func TestGetAdminInfo_ReportsInstalledPileusWebVersion(t *testing.T) {
	oldBase := core.BasePath
	core.BasePath = t.TempDir()
	t.Cleanup(func() { core.BasePath = oldBase })

	get := func() map[string]any {
		rec := httptest.NewRecorder()
		getAdminInfo(rec, httptest.NewRequest(http.MethodGet, "/admin/info", nil))
		var d map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return d
	}

	if d := get(); d["pileus_web_version"] != "" {
		t.Fatalf("no build installed: pileus_web_version = %v, want empty", d["pileus_web_version"])
	}

	dir := filepath.Join(core.BasePath, "data", "pileus-web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version.json"), []byte(`{"version":"1.3.9","build_number":"139"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	d := get()
	if d["pileus_web_version"] != "1.3.9" || d["pileus_web_build"] != "139" {
		t.Fatalf("got version=%v build=%v, want 1.3.9 / 139", d["pileus_web_version"], d["pileus_web_build"])
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mycelium/internal/core"
)

// Rete → "Preferisci IPv6 in uscita": saving the setting must switch the dial
// preference live (no restart) and /admin/info must report it.
func TestSaveSettings_EgressIPv6AppliesLive(t *testing.T) {
	orig := core.EgressPreferIPv6()
	t.Cleanup(func() { core.SetEgressPreferIPv6(orig) })

	for _, c := range []struct {
		val  string
		want bool
	}{{"1", true}, {"0", false}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/settings/save",
			strings.NewReader(`{"egress_ipv6":"`+c.val+`"}`))
		saveSettings(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("save %s: status %d %s", c.val, rec.Code, rec.Body.String())
		}
		if core.EgressPreferIPv6() != c.want {
			t.Fatalf("after saving %s: EgressPreferIPv6 = %v, want %v", c.val, core.EgressPreferIPv6(), c.want)
		}
		info := httptest.NewRecorder()
		getAdminInfo(info, httptest.NewRequest(http.MethodGet, "/admin/info", nil))
		var d map[string]any
		_ = json.Unmarshal(info.Body.Bytes(), &d)
		if d["egress_ipv6"] != c.want {
			t.Fatalf("/admin/info egress_ipv6 = %v, want %v", d["egress_ipv6"], c.want)
		}
	}
}

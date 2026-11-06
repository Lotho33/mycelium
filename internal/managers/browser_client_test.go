package managers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Lotho33/stipes-sdk/sdk/gen"
)

func TestBrowserServiceConfigSettingOverridesEnv(t *testing.T) {
	withTempSettings(t)
	t.Setenv("MYCELIUM_BROWSER_URL", "http://env-host:8191")
	t.Setenv("MYCELIUM_BROWSER_API_KEY", "env-key")

	if u, k := BrowserServiceConfig(); u != "http://env-host:8191" || k != "env-key" {
		t.Fatalf("no setting: got %q,%q; want the environment", u, k)
	}

	if err := Settings.Save(map[string]any{BrowserURLSetting: " http://browser:8191 ", BrowserAPIKeySetting: "ui-key"}); err != nil {
		t.Fatal(err)
	}
	if u, k := BrowserServiceConfig(); u != "http://browser:8191" || k != "ui-key" {
		t.Fatalf("with setting: got %q,%q; want the dashboard values", u, k)
	}

	// Clearing the setting falls back to the environment again.
	if err := Settings.Save(map[string]any{BrowserURLSetting: "", BrowserAPIKeySetting: ""}); err != nil {
		t.Fatal(err)
	}
	if u, k := BrowserServiceConfig(); u != "http://env-host:8191" || k != "env-key" {
		t.Fatalf("cleared: got %q,%q; want the environment", u, k)
	}
}

func TestApplyBrowserServiceConfigConnectsAndDisconnects(t *testing.T) {
	withTempSettings(t)
	t.Setenv("MYCELIUM_BROWSER_URL", "")
	orig := BrowserClient()
	t.Cleanup(func() { SetBrowserClient(orig) })

	// Port 9 (discard): the background health check just logs a failure.
	if err := Settings.Save(map[string]any{BrowserURLSetting: "http://127.0.0.1:9/"}); err != nil {
		t.Fatal(err)
	}
	ApplyBrowserServiceConfig()
	if bc := BrowserClient(); bc == nil || bc.BaseURL() != "http://127.0.0.1:9" {
		t.Fatalf("after apply: client %+v, want one for http://127.0.0.1:9", bc)
	}

	if err := Settings.Save(map[string]any{BrowserURLSetting: ""}); err != nil {
		t.Fatal(err)
	}
	ApplyBrowserServiceConfig()
	if BrowserClient() != nil {
		t.Fatal("with no URL anywhere the client must be dropped")
	}
}

func TestWaitInterceptViaSendsRefererOnlyWhenSet(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		got = append(got, body)
		_, _ = w.Write([]byte(`{"intercepted_url":"https://cdn.example/master.m3u8","headers":{}}`))
	}))
	defer srv.Close()
	c := &BrowserServiceClient{baseURL: srv.URL, http: srv.Client()}
	req := &gen.WaitInterceptRequest{TriggerUrl: "https://embed.example/e/1", UrlPattern: "*playlist*", TimeoutMs: 1000}

	if _, err := c.WaitInterceptVia(context.Background(), req, "", "https://site.example/"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitInterceptVia(context.Background(), req, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d requests, want 2", len(got))
	}
	if got[0]["referer"] != "https://site.example/" {
		t.Fatalf("referer = %v, want it forwarded to /v1/sniff", got[0]["referer"])
	}
	if _, ok := got[1]["referer"]; ok {
		t.Fatalf("empty referer must not be sent, body %v", got[1])
	}
}

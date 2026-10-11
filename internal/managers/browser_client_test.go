package managers

import "testing"

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

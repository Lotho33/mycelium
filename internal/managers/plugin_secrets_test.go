package managers

import "testing"

const pluginSecretsSchema = `
CREATE TABLE plugin_secrets (
	plugin_id  TEXT NOT NULL,
	profile_id TEXT NOT NULL,
	key        TEXT NOT NULL,
	value      TEXT NOT NULL,
	PRIMARY KEY (plugin_id, profile_id, key)
);`

func TestPluginSecrets_RoundTrip(t *testing.T) {
	m := NewDBManager(newTestDB(t, pluginSecretsSchema))

	if _, ok, err := m.GetPluginSecret("p", "u1", "token"); err != nil || ok {
		t.Fatalf("missing secret: ok=%v err=%v, want not found", ok, err)
	}
	if err := m.SetPluginSecret("p", "u1", "token", "a"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetPluginSecret("p", "u1", "token", "b"); err != nil { // replace
		t.Fatal(err)
	}
	if err := m.SetPluginSecret("p", "u2", "token", "c"); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := m.GetPluginSecret("p", "u1", "token"); !ok || v != "b" {
		t.Fatalf("u1 token = %q (ok=%v), want b", v, ok)
	}

	if err := m.DeletePluginSecretsForProfile("u1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.GetPluginSecret("p", "u1", "token"); ok {
		t.Fatal("u1 secret survived profile deletion")
	}
	if v, ok, _ := m.GetPluginSecret("p", "u2", "token"); !ok || v != "c" {
		t.Fatal("deleting u1's secrets touched u2")
	}
	if n, err := m.DeleteAllPluginSecrets(); err != nil || n != 1 {
		t.Fatalf("DeleteAllPluginSecrets = %d, %v; want 1", n, err)
	}
}

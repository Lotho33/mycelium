package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal plugin: one entrypoint that reads a global setting and calls the
// network through the real SDK.
const testManifest = `id: "sample"
name: "Sample"
version: "1.0.0"
entrypoints:
  search: "search_items"
`

const testInit = `
function search_items(args)
  local token = mycelium.context.get_global_setting("token")
  local r = mycelium.network.get("https://api.example.org/find?q=" .. args.query, { Authorization = token })
  local data = mycelium.json.parse(r.body)
  return { items = data.items, status = r.status_code }
end
`

func writePlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"manifest.yaml": testManifest, "init.lua": testInit} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeTest(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x_test.lua")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunFile_RoutesSettingsAndRequests(t *testing.T) {
	dir := writePlugin(t)
	file := writeTest(t, `
plugintest.route("/find", { status = 200, body = '{"items":[{"id":"a"}]}' })
local res = search_items({ query = "x" })
check(res.status == 200 and res.items[1].id == "a", "risposta dalla route")
local q = plugintest.requests()
check(#q == 1 and q[1].url == "https://api.example.org/find?q=x", "richiesta registrata")
check(q[1].headers.Authorization == "secret", "impostazione globale passata")
`)
	if err := runFile(dir, file, []string{"token=secret"}); err != nil {
		t.Fatal(err)
	}
}

func TestRunFile_FailuresAreReported(t *testing.T) {
	dir := writePlugin(t)
	err := runFile(dir, writeTest(t, `check(false, "deve fallire")`), nil)
	if err == nil || !strings.Contains(err.Error(), "deve fallire") {
		t.Fatalf("failed check not reported: %v", err)
	}
	// A request no route answers fails the file even when every check passed.
	err = runFile(dir, writeTest(t, `
local res = search_items({ query = "y" })
check(res.status == 599, "senza route: 599")
`), nil)
	if err == nil || !strings.Contains(err.Error(), "find?q=y") {
		t.Fatalf("unmatched request not reported: %v", err)
	}
}

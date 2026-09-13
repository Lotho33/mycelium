package api

import (
	"path/filepath"
	"testing"

	"mycelium/internal/core"
)

// TestSanitizeName_RejectsCollapsingNames covers the "....zip" bug: an
// upload filename that TrimSuffix(".zip") turns into "..", "." or "" must
// never come back as a name usable to build a filesystem path — the old
// implementation (filepath.Base + strings.ReplaceAll("..","")) collapsed
// "..0" -> "" here, which let a zip named "....zip" extract straight into
// plugins/ itself and overwrite every plugin already installed there.
func TestSanitizeName_RejectsCollapsingNames(t *testing.T) {
	cases := []string{
		"..", // from "....zip" after TrimSuffix(".zip")
		".",  // from "..zip" after TrimSuffix(".zip")
		"",   // from ".zip" after TrimSuffix(".zip")
	}
	for _, in := range cases {
		got, err := sanitizeName(in)
		if err == nil {
			t.Errorf("sanitizeName(%q) = %q, nil — want an error, never a silently-accepted name", in, got)
		}
		if got != "" {
			t.Errorf("sanitizeName(%q) returned non-empty name %q alongside an error", in, got)
		}
	}
}

// TestSanitizeName_RejectsTraversalAndSeparators covers slashes, backslashes
// and absolute paths sneaking in via the archive/file name.
func TestSanitizeName_RejectsTraversalAndSeparators(t *testing.T) {
	cases := []string{
		"../../etc/passwd",
		"foo/../../bar",
		`foo\bar`,
		"/etc/passwd",
		"..",
		"...", // three dots is not "..", but exercise it isn't mistaken for legit either way below
	}
	for _, in := range cases {
		got, err := sanitizeName(in)
		if err != nil {
			continue // rejecting outright is fine and expected for most of these
		}
		// If it wasn't rejected, the result must be a single, safe path
		// segment: no separators, and not "." or "..".
		if got == "" || got == "." || got == ".." {
			t.Errorf("sanitizeName(%q) = %q, nil — unsafe name accepted", in, got)
		}
		if filepath.Base(got) != got {
			t.Errorf("sanitizeName(%q) = %q — contains a path separator", in, got)
		}
	}
}

// TestSanitizeName_AllowsLegitimateNames ensures normal plugin/file names
// keep working.
func TestSanitizeName_AllowsLegitimateNames(t *testing.T) {
	cases := map[string]string{
		"vix.movie":      "vix.movie",
		"animeunity":     "animeunity",
		"my-plugin_v2.1": "my-plugin_v2.1",
		"plugin.zip.bak": "plugin.zip.bak",
	}
	for in, want := range cases {
		got, err := sanitizeName(in)
		if err != nil {
			t.Errorf("sanitizeName(%q) unexpectedly failed: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSanitizeName_RejectsBadCharacters exercises the whitelist directly.
func TestSanitizeName_RejectsBadCharacters(t *testing.T) {
	cases := []string{
		"plugin name", // space
		"plugin;rm -rf",
		"plugin$(whoami)",
		"plugin\x00null",
	}
	for _, in := range cases {
		if got, err := sanitizeName(in); err == nil {
			t.Errorf("sanitizeName(%q) = %q, nil — want rejection of disallowed characters", in, got)
		}
	}
}

// TestUploadDestDir_NeverCollapsesToPluginsRoot reproduces the exact
// destDir computation from uploadLuaPlugin for the historical bug input and
// asserts the defense-in-depth check (destDir must be a direct child of
// plugins/, never plugins/ itself) would reject it even if sanitizeName
// somehow let it through.
func TestUploadDestDir_NeverCollapsesToPluginsRoot(t *testing.T) {
	pluginsRoot := filepath.Clean(core.AppPath("plugins"))

	badNames := []string{"", ".", ".."}
	for _, pluginName := range badNames {
		destDir := core.AppPath("plugins", pluginName)
		destDirClean := filepath.Clean(destDir)
		isDirectChild := destDirClean != pluginsRoot && filepath.Dir(destDirClean) == pluginsRoot
		if isDirectChild {
			t.Errorf("pluginName %q produced destDir %q, which the guard would wrongly accept as a direct child of %q", pluginName, destDirClean, pluginsRoot)
		}
	}

	// Sanity check the guard still accepts a normal, legitimate name.
	destDir := core.AppPath("plugins", "vix.movie")
	destDirClean := filepath.Clean(destDir)
	if destDirClean == pluginsRoot || filepath.Dir(destDirClean) != pluginsRoot {
		t.Errorf("legitimate destDir %q was wrongly rejected by the direct-child guard", destDirClean)
	}
}

package api

import (
	"archive/zip"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A ZIP must not be able to plant setuid/world-writable files or symlinks.
func TestExtractZipSafe_IgnoresArchiveModesAndSymlinks(t *testing.T) {
	buf := &bytes.Buffer{}
	zw := zip.NewWriter(buf)
	hdr := &zip.FileHeader{Name: "run.sh", Method: zip.Store}
	hdr.SetMode(os.ModeSetuid | 0o777)
	w, _ := zw.CreateHeader(hdr)
	w.Write([]byte("#!/bin/sh\n"))
	link := &zip.FileHeader{Name: "escape", Method: zip.Store}
	link.SetMode(os.ModeSymlink | 0o777)
	w, _ = zw.CreateHeader(link)
	w.Write([]byte("/etc/passwd"))
	zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	extractZipSafe(zr, dir, 1<<20, 10<<20, "[test]")

	fi, err := os.Stat(filepath.Join(dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&(os.ModeSetuid|0o022) != 0 {
		t.Fatalf("mode %v: setuid or group/world-writable survived", fi.Mode())
	}
	if _, err := os.Lstat(filepath.Join(dir, "escape")); !os.IsNotExist(err) {
		t.Fatalf("symlink entry was extracted (err=%v)", err)
	}
}

func TestLanUDPSource(t *testing.T) {
	for ip, want := range map[string]bool{
		"192.168.1.20": true, "10.0.0.5": true, "fe80::1": true, "100.100.1.1": true, "127.0.0.1": true,
		"8.8.8.8": false, "2001:4860::8888": false,
	} {
		if got := lanUDPSource(&net.UDPAddr{IP: net.ParseIP(ip), Port: 5353}); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
	if lanUDPSource(nil) {
		t.Error("nil source allowed")
	}
}

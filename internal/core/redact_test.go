package core

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestRedactURLError(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://cdn.example/hls/abc/index.m3u8?token=SECRET", Err: context.DeadlineExceeded}
	got := RedactURLError(err)
	if strings.Contains(got.Error(), "SECRET") || strings.Contains(got.Error(), "/hls/") {
		t.Fatalf("URL leaked: %q", got)
	}
	if !strings.Contains(got.Error(), "cdn.example") {
		t.Fatalf("host missing: %q", got)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatal("underlying error lost")
	}
	plain := errors.New("x")
	if RedactURLError(plain) != plain {
		t.Fatal("non-url error must pass through")
	}
}

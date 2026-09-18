package core

import (
	"net/http/httptest"
	"testing"
)

func TestSetCORSHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	SetCORSHeaders(rec, "GET, POST", "Content-Type", "")
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("Allow-Origin = %q, want *", h.Get("Access-Control-Allow-Origin"))
	}
	if h.Get("Access-Control-Allow-Methods") != "GET, POST" {
		t.Errorf("Allow-Methods = %q", h.Get("Access-Control-Allow-Methods"))
	}
	if h.Get("Access-Control-Allow-Headers") != "Content-Type" {
		t.Errorf("Allow-Headers = %q", h.Get("Access-Control-Allow-Headers"))
	}
	if _, ok := h["Access-Control-Expose-Headers"]; ok {
		t.Error("Expose-Headers set when exposeHeaders was empty")
	}
}

func TestSetCORSHeaders_ExposeHeadersOptional(t *testing.T) {
	rec := httptest.NewRecorder()
	SetCORSHeaders(rec, "POST", "content-type", "grpc-status, grpc-message")
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "grpc-status, grpc-message" {
		t.Errorf("Expose-Headers = %q", got)
	}
}

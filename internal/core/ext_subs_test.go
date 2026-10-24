package core

import (
	"bytes"
	"testing"
)

func TestInlineSubtitle_PutGet(t *testing.T) {
	a, ok := PutInlineSubtitle([]byte("one"))
	if !ok {
		t.Fatal("put failed")
	}
	if b, _ := PutInlineSubtitle([]byte("one")); b != a {
		t.Fatalf("same bytes, different url: %s %s", a, b)
	}
	if got, ok := InlineSubtitle(a); !ok || string(got) != "one" {
		t.Fatalf("get = %q %v", got, ok)
	}
	if _, ok := InlineSubtitle("https://x/a.srt"); ok {
		t.Fatal("a plain url is not inline")
	}
	if _, ok := PutInlineSubtitle(nil); ok {
		t.Fatal("empty accepted")
	}
	if _, ok := PutInlineSubtitle(make([]byte, MaxInlineSubtitleBytes+1)); ok {
		t.Fatal("oversized accepted")
	}
}

// Over the ceiling the least recently used goes first; the new one stays.
func TestInlineSubtitle_Bounded(t *testing.T) {
	var first string
	for i := 0; i < inlineSubsMaxBytes/MaxInlineSubtitleBytes+2; i++ {
		u, ok := PutInlineSubtitle(bytes.Repeat([]byte{byte('a' + i)}, MaxInlineSubtitleBytes))
		if !ok {
			t.Fatal("put failed")
		}
		if i == 0 {
			first = u
		}
		if _, ok := InlineSubtitle(u); !ok {
			t.Fatalf("just stored %d evicted", i)
		}
	}
	if _, ok := InlineSubtitle(first); ok {
		t.Fatal("oldest not evicted")
	}
	inlineSubs.Lock()
	defer inlineSubs.Unlock()
	if inlineSubs.bytes > inlineSubsMaxBytes {
		t.Fatalf("bytes %d over the ceiling", inlineSubs.bytes)
	}
}

package pileus

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/protobuf/proto"
)

// EpisodeInfo.extra is field 12, a string map: clients decode exactly these
// bytes (the Pileus side has the same fixture).
func TestEpisodeInfoExtra_Wire(t *testing.T) {
	b, err := proto.Marshal(&gen.EpisodeInfo{Extra: map[string]string{"lang": "jap"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(b), "620b0a046c616e6712036a6170"; got != want {
		t.Fatalf("wire = %s, want %s", got, want)
	}
}

// The plugin's extra reaches the episode as is; an empty Lua table ([])
// leaves it empty.
func TestLuaMetaToEpisodeInfo_Extra(t *testing.T) {
	var lm luaMeta
	if err := json.Unmarshal([]byte(`{"id":"e1","extra":{"lang":"jap"}}`), &lm); err != nil {
		t.Fatal(err)
	}
	if got := luaMetaToEpisodeInfo(lm).Extra["lang"]; got != "jap" {
		t.Fatalf("extra.lang = %q", got)
	}
	var empty luaMeta
	if err := json.Unmarshal([]byte(`{"id":"e2","extra":[]}`), &empty); err != nil {
		t.Fatal(err)
	}
	if n := len(luaMetaToEpisodeInfo(empty).Extra); n != 0 {
		t.Fatalf("extra from [] has %d keys", n)
	}
}

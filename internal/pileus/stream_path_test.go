package pileus

import (
	"context"
	"reflect"
	"testing"
)

// path reaches the client as given; odd shapes never lose the whole list.
func TestLuaPipeline_StreamPath(t *testing.T) {
	p := loadContractPlugin(t, "t.streampath", `
		function get_streams(args)
			return {
				{ id = "a", label = "Group A · Auto", path = { "Group A", "Auto" }, is_live = true },
				{ id = "b", label = "Server 2", path = { "Server 2" } },
				{ id = "c", label = "flat" },
				{ id = "d", label = "empty", path = {} },
				{ id = "e", label = "one", path = "Solo" },
				{ id = "f", label = "mixed", path = { "Group", 7, { x = 1 }, "" } },
			}
		end
		function resolve_stream(args) return { url = "http://x/p.m3u8" } end
	`)
	srcs, err := p.GetStreams(context.Background(), "x")
	if err != nil || len(srcs) != 6 {
		t.Fatalf("GetStreams = %v, %v", srcs, err)
	}
	want := map[string][]string{
		"a": {"Group A", "Auto"},
		"b": {"Server 2"},
		"c": nil,
		"d": nil,
		"e": {"Solo"},
		"f": {"Group", "7"},
	}
	for _, s := range srcs {
		if got := s.GetPath(); !reflect.DeepEqual(got, want[s.Id]) && !(len(got) == 0 && len(want[s.Id]) == 0) {
			t.Errorf("%s: path %q, want %q", s.Id, got, want[s.Id])
		}
	}
	if srcs[0].Label != "Group A · Auto" || !srcs[0].IsLive {
		t.Errorf("other fields lost: %+v", srcs[0])
	}
}

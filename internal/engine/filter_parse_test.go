package engine

import (
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// The plugins parse composite filter values (multiselect "a,b,c", range
// "lo..hi") with small local helpers copied into each plugin. Guard the exact
// Lua patterns here — a wrong `%.%.` or `[^,]+` fails silently as "no filter".
func TestPluginFilterParsers(t *testing.T) {
	const src = `
	function split_csv(s)
	  local out = {}
	  for part in tostring(s or ""):gmatch("[^,]+") do
	    part = part:match("^%s*(.-)%s*$")
	    if part ~= "" then out[#out+1] = part end
	  end
	  return out
	end
	function split_range(s)
	  s = tostring(s or "")
	  if s == "" then return nil, nil end
	  local lo, hi = s:match("^(.-)%.%.(.-)$")
	  if not lo then lo, hi = s, s end
	  return tonumber(lo), tonumber(hi)
	end

	function csv_join(s) return table.concat(split_csv(s), "|") end
	function rng(s) local a,b = split_range(s); return (a or "nil")..","..(b or "nil") end
	`

	L := lua.NewState()
	defer L.Close()
	if err := L.DoString(src); err != nil {
		t.Fatalf("load helpers: %v", err)
	}

	call := func(fn, arg string) string {
		if err := L.CallByParam(lua.P{Fn: L.GetGlobal(fn), NRet: 1, Protect: true}, lua.LString(arg)); err != nil {
			t.Fatalf("%s(%q): %v", fn, arg, err)
		}
		s := L.ToString(-1)
		L.Pop(1)
		return s
	}

	csv := map[string]string{
		"28,12,878": "28|12|878",
		" 28 , 12 ": "28|12",
		"28":        "28",
		"":          "",
		",,":        "",
	}
	for in_, want := range csv {
		if got := call("csv_join", in_); got != want {
			t.Errorf("split_csv(%q) = %q, want %q", in_, got, want)
		}
	}

	rng := map[string]string{
		"2000..2015": "2000,2015",
		"2000..":     "2000,nil",
		"..2015":     "nil,2015",
		"2010":       "2010,2010",
		"":           "nil,nil",
	}
	for in_, want := range rng {
		if got := call("rng", in_); got != want {
			t.Errorf("split_range(%q) = %q, want %q", in_, got, want)
		}
	}
}

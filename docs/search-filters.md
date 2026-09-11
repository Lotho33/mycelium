# Search filters

## Does a plugin have filters?

`ListPlugins` adds `"search_filters"` to a plugin's `PluginInfo.capabilities`
when its manifest declares the `search_filters` entrypoint. Pileus should show
the filter UI only when that capability is present — a plugin without it (e.g.
one backed by a personal media server) gets no filter button. The manifest's
`entrypoints` block is the single source of truth; nothing extra to declare.

## Filter definitions

A plugin's `get_search_filters` entrypoint returns a list of filter definitions.
The client (Pileus) renders one control per definition; on search it puts the
chosen values into `SearchRequest.filters`, a **`map<string,string>`**. Composite
values are string-encoded — the wire format never changes.

## Filter types

| `type`        | control                | `filters[id]` value                              |
|---------------|------------------------|--------------------------------------------------|
| `select`      | single-choice dropdown | one option id: `"28"`                             |
| `multiselect` | chips / checkbox list  | option ids, comma-joined: `"28,12,878"` (order irrelevant; empty = key omitted) |
| `range`       | dual-handle slider     | `"<lo>..<hi>"`; either side may be empty: `"2000.."`, `"..2015"`, `"2000..2015"`. Key omitted = no constraint |
| `bool`        | switch                 | `"true"` / `"false"` (key omitted = unset)       |
| `number`      | numeric field          | one integer: `"2019"`                            |

Only `select` / `bool` / `number` existed before; `multiselect` and `range` are
additive. An old client that doesn't know a type should skip that filter, not
break.

### `range` bounds

A `range` filter carries its slider bounds in `options`, by id:

```lua
{ id = "year", label = "Anno", type = "range", options = {
    { id = "min",  label = "1960" },
    { id = "max",  label = "2026" },   -- e.g. current year + 1
    { id = "step", label = "1"    },   -- optional, default "1"
} }
```

`label` holds the numeric value as a string (it doubles as the tick label). For a
rating slider use `min="0" max="10" step="0.5"`.

## Plugin side

`search_items(args)` receives `args.filters` as a Lua table of `string`→`string`.
Each plugin parses its own composite values — **no shared helper** (plugins stay
independent). Typical parsing:

```lua
-- multiselect → list of ids
local function split_csv(s)
  local out = {}
  for part in tostring(s or ""):gmatch("[^,]+") do out[#out+1] = part end
  return out
end

-- range "lo..hi" → lo, hi (either may be nil)
local function split_range(s)
  local lo, hi = tostring(s or ""):match("^(.-)%.%.(.-)$")
  if not lo and s and s ~= "" then lo, hi = s, s end        -- bare "2010"
  return tonumber(lo) or nil, tonumber(hi) or nil
end
```

## Example filter set

A typical VOD plugin exposes:

- `genre` — multiselect, mapped to the upstream catalogue's genre ids.
- `year` — range; applied as an upstream query param when supported, else
  post-filtered on the result page.
- `rating` — range.
- `order` — select (`popularity` / `rating` / `az` / `za`). Values the upstream
  API can't sort by natively are re-sorted client-side after a bounded prefetch.

When a text query is present, some upstream search endpoints ignore discovery
params, so the plugin applies genre/year/rating to the result page itself.

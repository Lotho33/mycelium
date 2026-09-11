-- jellyfin — Libreria personale da un server Jellyfin (API key, no scraping)
-- Richiede: jellyfin_url, jellyfin_api_key (global setting)

require("http")
require("catalog")
require("browse")
require("genres")

-- ─────────────────────────────────────────────────────────────────────────────
-- check_connection (task periodico)
-- ─────────────────────────────────────────────────────────────────────────────

function check_connection()
  local url = jf_base()
  if url == "" then
    mycelium.context.set_plugin_status("error", "jellyfin_url non configurato")
    error("[jellyfin] check_connection: url mancante")
  end
  if jf_key() == "" then
    mycelium.context.set_plugin_status("error", "jellyfin_api_key non configurata")
    error("[jellyfin] check_connection: api key mancante")
  end
  -- Endpoint pubblico, non richiede autenticazione: verifica solo che il
  -- server sia raggiungibile.
  local resp = mycelium.network.get(url .. "/System/Info/Public", { ["Accept"] = "application/json" })
  if resp.status_code ~= 200 then
    mycelium.context.set_plugin_status("error", "server non raggiungibile (status=" .. tostring(resp.status_code) .. ")")
    error("[jellyfin] check_connection: server non raggiungibile")
  end
  mycelium.context.set_plugin_status("ready", "")
end

-- ─────────────────────────────────────────────────────────────────────────────
-- get_catalog / browse
-- ─────────────────────────────────────────────────────────────────────────────

function get_catalog(args)
  local cid = args.catalog_id or "latest"

  if string.sub(cid, 1, 7) == "genre::" then
    return jf_genre_items(string.sub(cid, 8), args.page or 1)
  end
  if cid ~= "latest" then return {} end

  local uid = jf_user_id()
  local url = string.format(
    "%s/Users/%s/Items/Latest?Limit=25&Fields=Overview,Genres,CommunityRating,ProductionYear,BackdropImageTags,OfficialRating",
    jf_base(), uid)
  local resp = mycelium.network.get(url, jf_headers())
  if resp.status_code ~= 200 then return {} end
  local data, err = mycelium.json.parse(resp.body)
  if err or type(data) ~= "table" then return {} end

  local items = {}
  for _, it in ipairs(data) do
    local m = map_item(it)
    if m then table.insert(items, m) end
  end
  return items
end

function browse(args)
  local dir_id = args.directory_id or "__browser__"
  local page   = args.page or 1

  if dir_id == "__browser__" then
    local libs = {}
    for _, l in ipairs(jf_libraries()) do
      table.insert(libs, { id = "lib_" .. l.id, title = l.title, is_dir = true })
    end
    return libs
  elseif string.sub(dir_id, 1, 4) == "lib_" then
    local lib_id = string.sub(dir_id, 5)
    return jf_library_items(lib_id, page)
  end

  -- dir_id è un Id Jellyfin "grezzo": può essere una Series (→ stagioni) o
  -- una Season (→ episodi) — un fetch dell'item decide quale.
  local item = fetch_item(dir_id)
  if not item then return {} end
  if item.Type == "Series" then
    return jf_seasons(dir_id)
  elseif item.Type == "Season" then
    return jf_episodes(item.SeriesId, dir_id)
  end
  return {}
end

-- ─────────────────────────────────────────────────────────────────────────────
-- search_items
-- ─────────────────────────────────────────────────────────────────────────────

function search_items(args)
  local query = args.query or ""
  local page  = args.page or 1
  if query == "" then return {} end

  local uid = jf_user_id()
  local page_size = 25
  local start = (page - 1) * page_size
  local encoded = string.gsub(query, " ", "%%20")
  local url = string.format(
    "%s/Users/%s/Items?searchTerm=%s&Recursive=true&IncludeItemTypes=Movie,Series&Fields=Overview,Genres,CommunityRating,ProductionYear,BackdropImageTags,OfficialRating&StartIndex=%d&Limit=%d",
    jf_base(), uid, encoded, start, page_size)
  local resp = mycelium.network.get(url, jf_headers())
  if resp.status_code ~= 200 then return {} end
  local data, err = mycelium.json.parse(resp.body)
  if err or not data or not data.Items then return {} end

  local items = {}
  for _, it in ipairs(data.Items) do
    local m = map_item(it)
    if m then table.insert(items, m) end
  end
  local total = data.TotalRecordCount or #items
  return { items = items, has_more = (start + #items) < total }
end

-- ─────────────────────────────────────────────────────────────────────────────
-- get_details
-- ─────────────────────────────────────────────────────────────────────────────

function get_details(args)
  local media_id = args.media_id or ""
  if media_id == "" then return nil end

  local item = fetch_item(media_id)
  if not item then return nil end
  local mapped = map_item(item)
  if not mapped then return nil end

  if item.Type == "Series" then
    local seasons = jf_seasons(media_id)
    mapped.seasons = {}
    for _, s in ipairs(seasons) do
      table.insert(mapped.seasons, {
        number        = s.season_number,
        label         = s.title,
        directory_id  = s.id,
        id            = s.id,
        poster_url    = s.poster_url,
        year          = s.year,
        episode_count = 0,
        overview      = "",
        air_date      = "",
      })
    end
  end

  return mapped
end

-- ─────────────────────────────────────────────────────────────────────────────
-- get_streams / resolve_stream
-- ─────────────────────────────────────────────────────────────────────────────

function get_streams(args)
  local media_id = args.media_id or ""
  return {
    { id = media_id .. ":jf", label = "Diretta", quality = "Auto", is_live = false },
  }
end

-- resolve_stream
-- static=true → file originale senza transcoding, servito direttamente da
-- Jellyfin: adatto a un server personale su rete locale. L'api_key nella
-- query string basta come autenticazione, nessun header speciale da passare
-- al client.
function resolve_stream(args)
  local stream_id = args.stream_id or ""
  local item_id = string.gsub(stream_id, ":jf$", "")
  if item_id == "" then return nil end

  local url = string.format("%s/Videos/%s/stream?static=true&api_key=%s", jf_base(), item_id, jf_key())
  return {
    url     = url,
    is_live = false,
  }
end

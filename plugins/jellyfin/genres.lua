-- genres.lua — carosello dinamico per genere (catalog_list) + fetch items per genere

local FIELDS = "Overview,Genres,CommunityRating,ProductionYear,BackdropImageTags,OfficialRating"

CATALOG_LIST_CACHE_KEY = "jellyfin:catalog_list"
CATALOG_LIST_TTL       = 21600 -- 6h: i generi di una libreria cambiano di rado

-- get_catalog_list — entrypoint catalog_list del manifest.
-- Sostituisce per intero la lista statica del manifest (solo fallback in
-- caso di errore/timeout): "Aggiunti di Recente" + un carosello per ogni
-- genere presente nella libreria Jellyfin (film e serie).
function get_catalog_list()
  local cached, found = mycelium.cache.get(CATALOG_LIST_CACHE_KEY)
  if found and cached and cached ~= "" then
    local t = mycelium.json.parse(cached)
    if type(t) == "table" and #t > 0 then return t end
  end

  local catalogs = {
    { id = "latest", name = "Aggiunti di Recente", type = "movie", style_hint = "featured", cache_ttl_seconds = 300, auto_hide_when_empty = true },
  }

  local uid = jf_user_id()
  local url = string.format("%s/Genres?userId=%s&IncludeItemTypes=Movie,Series&Recursive=true", jf_base(), uid)
  local resp = mycelium.network.get(url, jf_headers())
  if resp.status_code == 200 then
    local data, err = mycelium.json.parse(resp.body)
    if not err and data and data.Items then
      for _, g in ipairs(data.Items) do
        if g.Name and g.Name ~= "" then
          table.insert(catalogs, {
            id                   = "genre::" .. g.Name,
            name                 = g.Name,
            type                 = "movie",
            cache_ttl_seconds    = 900,
            auto_hide_when_empty = true,
          })
        end
      end
    end
  else
    mycelium.log("[jellyfin] get_catalog_list: /Genres fallita status=" .. tostring(resp.status_code))
  end

  mycelium.cache.set(CATALOG_LIST_CACHE_KEY, mycelium.json.stringify(catalogs), CATALOG_LIST_TTL)
  return catalogs
end

-- jf_genre_items(genre, page) — usato da get_catalog per i cataloghi "genre::*".
-- Recursive=true: attraversa entrambe le librerie (film e serie) in un colpo solo.
function jf_genre_items(genre, page)
  local uid = jf_user_id()
  local page_size = 25
  local start = (page - 1) * page_size
  local url = string.format(
    "%s/Users/%s/Items?Genres=%s&Recursive=true&IncludeItemTypes=Movie,Series&SortBy=DateCreated&SortOrder=Descending&Fields=%s&StartIndex=%d&Limit=%d",
    jf_base(), uid, jf_url_encode(genre), FIELDS, start, page_size)
  local resp = mycelium.network.get(url, jf_headers())
  if resp.status_code ~= 200 then return {} end
  local data, err = mycelium.json.parse(resp.body)
  if err or not data or not data.Items then return {} end

  local items = {}
  for _, it in ipairs(data.Items) do
    local m = map_item(it)
    if m then table.insert(items, m) end
  end
  return items
end

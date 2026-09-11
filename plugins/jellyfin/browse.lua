-- browse.lua — libreria → show → stagione → episodi

local FIELDS = "Overview,Genres,CommunityRating,ProductionYear,BackdropImageTags,OfficialRating"

function jf_libraries()
  local uid = jf_user_id()
  local url = string.format("%s/Users/%s/Views", jf_base(), uid)
  local resp = mycelium.network.get(url, jf_headers())
  if resp.status_code ~= 200 then return {} end
  local data, err = mycelium.json.parse(resp.body)
  if err or not data or not data.Items then return {} end

  -- Solo film e serie: stesso ambito contenuti (VOD) del resto di mycelium,
  -- musica/altre collezioni Jellyfin restano fuori.
  local libs = {}
  for _, v in ipairs(data.Items) do
    if v.CollectionType == "movies" or v.CollectionType == "tvshows" then
      table.insert(libs, { id = v.Id, title = v.Name })
    end
  end
  return libs
end

function jf_library_items(lib_id, page)
  local uid = jf_user_id()
  local page_size = 25
  local start = (page - 1) * page_size
  local url = string.format(
    "%s/Users/%s/Items?ParentId=%s&Recursive=false&SortBy=SortName&IncludeItemTypes=Movie,Series&Fields=%s&StartIndex=%d&Limit=%d",
    jf_base(), uid, lib_id, FIELDS, start, page_size)
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

function jf_seasons(series_id)
  local uid = jf_user_id()
  local url = string.format("%s/Shows/%s/Seasons?userId=%s&Fields=Overview,ProductionYear", jf_base(), series_id, uid)
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

function jf_episodes(series_id, season_id)
  local uid = jf_user_id()
  local url = string.format("%s/Shows/%s/Episodes?seasonId=%s&userId=%s&Fields=Overview", jf_base(), series_id, season_id, uid)
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

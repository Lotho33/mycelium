-- catalog.lua — mapping Jellyfin → item, fetch generico per id

function jf_poster_url(item_id)
  return string.format("%s/Items/%s/Images/Primary?api_key=%s", jf_base(), item_id, jf_key())
end

function jf_backdrop_url(item)
  if item.BackdropImageTags and #item.BackdropImageTags > 0 then
    return string.format("%s/Items/%s/Images/Backdrop?api_key=%s", jf_base(), item.Id, jf_key())
  end
  return ""
end

-- jf_logo_url(item)
-- ImageTags.Logo è presente solo se il server ha un clearlogo per l'item
-- (Jellyfin lo scarica da provider tipo Fanart.tv se configurati) — a
-- differenza di poster/backdrop non c'è un fallback, quindi va controllato
-- prima di comporre l'URL.
function jf_logo_url(item)
  if item.ImageTags and item.ImageTags.Logo and item.ImageTags.Logo ~= "" then
    return string.format("%s/Items/%s/Images/Logo?api_key=%s", jf_base(), item.Id, jf_key())
  end
  return ""
end

-- map_item(item)
-- item arriva da /Users/{id}/Items, /Items/{id}, /Items/Latest oppure da
-- /Shows/*/Seasons|Episodes. Il campo Type di Jellyfin ("Movie"|"Series"|
-- "Season"|"Episode") decide il mapping.
function map_item(item)
  if not item or not item.Id or not item.Type then return nil end

  if item.Type == "Movie" then
    return {
      id             = item.Id,
      title          = item.Name or "",
      media_type     = "movie",
      poster_url     = jf_poster_url(item.Id),
      fanart_url     = jf_backdrop_url(item),
      logo_url       = jf_logo_url(item),
      plot           = item.Overview or "",
      year           = item.ProductionYear or 0,
      genres         = item.Genres or {},
      rating         = item.CommunityRating or 0.0,
      runtime        = item.RunTimeTicks and string.format("%d min", math.floor(item.RunTimeTicks / 600000000)) or nil,
      content_rating = item.OfficialRating or "",
    }
  elseif item.Type == "Series" then
    return {
      id             = item.Id,
      title          = item.Name or "",
      media_type     = "series",
      poster_url     = jf_poster_url(item.Id),
      fanart_url     = jf_backdrop_url(item),
      logo_url       = jf_logo_url(item),
      plot           = item.Overview or "",
      year           = item.ProductionYear or 0,
      genres         = item.Genres or {},
      rating         = item.CommunityRating or 0.0,
      is_dir         = true,
      status         = item.Status or "",
      content_rating = item.OfficialRating or "",
    }
  elseif item.Type == "Season" then
    return {
      id            = item.Id,
      title         = item.Name or "",
      media_type    = "series",
      poster_url    = jf_poster_url(item.Id),
      season_number = item.IndexNumber or 0,
      show_id       = item.SeriesId or "",
      parent_id     = item.SeriesId or "",
      is_dir        = true,
      year          = item.ProductionYear or 0,
    }
  elseif item.Type == "Episode" then
    return {
      id             = item.Id,
      title          = item.Name or "",
      media_type     = "episode",
      thumbnail_url  = jf_poster_url(item.Id),
      plot           = item.Overview or "",
      season_number  = item.ParentIndexNumber or 0,
      episode_number = item.IndexNumber or 0,
      show_id        = item.SeriesId or "",
      parent_id      = item.SeasonId or "",
      air_date       = item.PremiereDate or "",
      duration       = item.RunTimeTicks and math.floor(item.RunTimeTicks / 600000000) or 0,
    }
  end
  return nil
end

-- fetch_item(item_id)
-- Fetch generico per id, indipendente dal tipo — usato da get_details e dal
-- browse per capire su cosa si sta navigando (Series vs Season) prima di
-- decidere quale sotto-endpoint chiamare.
function fetch_item(item_id)
  local uid = jf_user_id()
  local url = string.format("%s/Users/%s/Items/%s", jf_base(), uid, item_id)
  local resp = mycelium.network.get(url, jf_headers())
  if resp.status_code ~= 200 then return nil end
  local data, err = mycelium.json.parse(resp.body)
  if err or not data then return nil end
  return data
end

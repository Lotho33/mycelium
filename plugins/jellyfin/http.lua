-- http.lua — header factory, config e utente corrente per il plugin jellyfin

function jf_base()
  local url = mycelium.context.get_global_setting("jellyfin_url") or ""
  -- Rimuove uno slash finale, se presente: evita "//" quando componiamo i path sotto.
  return (string.gsub(url, "/+$", ""))
end

function jf_key()
  return mycelium.context.get_global_setting("jellyfin_api_key")
end

function jf_url_encode(s)
  return (s:gsub(" ", "%%20"):gsub("[^%w%%%-_%.~]", function(c)
    return string.format("%%%02X", string.byte(c))
  end))
end

function jf_headers()
  return {
    ["X-Emby-Token"] = jf_key(),
    ["Accept"]       = "application/json",
  }
end

-- jf_user_id()
-- Le API di navigazione Jellyfin sono scoped a un utente (/Users/{id}/Items) —
-- con una API key server-wide non c'è un utente "corrente" implicito. Qui si
-- prende il primo utente amministratore (o il primo in assoluto se nessuno è
-- admin) e lo si cachia: sufficiente per un server personale a singolo
-- utente, che è il caso d'uso di questo plugin.
function jf_user_id()
  local cached, found = mycelium.cache.get("jf:user_id")
  if found and cached ~= "" then return cached end

  local resp = mycelium.network.get(jf_base() .. "/Users", jf_headers())
  if resp.status_code ~= 200 then return "" end
  local users, err = mycelium.json.parse(resp.body)
  if err or type(users) ~= "table" or #users == 0 then return "" end

  local chosen = users[1].Id
  for _, u in ipairs(users) do
    if u.Policy and u.Policy.IsAdministrator then
      chosen = u.Id
      break
    end
  end
  mycelium.cache.set("jf:user_id", chosen, 86400)
  return chosen
end

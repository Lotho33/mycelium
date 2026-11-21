---@meta mycelium
-- Type annotations of the mycelium.* plugin SDK for the Lua language server
-- (LuaLS / sumneko, e.g. the VS Code "Lua" extension): completion, hover docs
-- and type checks while writing a plugin. Not loaded by mycelium.
--
-- Use: point the language server at this file, e.g. in the plugin folder's
-- .luarc.json:  { "workspace.library": ["<mycelium-core>/docs"] }
--
-- docs/plugin-sdk.md is the reference; internal/engine/sdk_meta_test.go fails
-- when this file and the SDK the core registers drift apart.

---@class mycelium
mycelium = {}

-- ── network ─────────────────────────────────────────────────────────────────

---@class mycelium.Response
---@field status_code integer
---@field body string
---@field headers table<string,string> repeated headers joined by "\n"
---@field error string|nil transport error, nil on any HTTP answer

---@class mycelium.Request
---@field url string
---@field method? string
---@field body? string
---@field headers? table<string,string>
---@field timeout? integer seconds

---@class mycelium.network
mycelium.network = {}

---GET through the plugin's egress (proxy/VPN unless direct_egress). Transport
---failures are retried; timeout up to 90 s; a body over 32 MiB is an error.
---@param url string
---@param headers? table<string,string> `_proxy = true` opts a direct_egress plugin into the proxy
---@param timeout_seconds? integer
---@return mycelium.Response
function mycelium.network.get(url, headers, timeout_seconds) end

---@param url string
---@param body string
---@param headers? table<string,string>
---@param timeout_seconds? integer
---@return mycelium.Response
function mycelium.network.post(url, body, headers, timeout_seconds) end

---@param url string
---@param opts? {method?: string, body?: string, headers?: table<string,string>, timeout_seconds?: integer}
---@return string|nil body
---@return string|nil err
function mycelium.network.fetch(url, opts) end

---Requests in parallel (at most 32, `concurrency` at a time up to 16; 8 MiB
---per response); one response per request, same order.
---@param requests (string|mycelium.Request)[]
---@param opts? {concurrency?: integer, timeout?: integer, ratelimit?: string}
---@return mycelium.Response[]
function mycelium.network.get_many(requests, opts) end

-- ── browser ─────────────────────────────────────────────────────────────────

---Needs the optional browser service; otherwise err = "browser service not available".
---@class mycelium.browser
mycelium.browser = {}

---@param url string
---@param timeout_seconds? integer
---@return string|nil html
---@return string|nil final_url
---@return string|nil err
function mycelium.browser.navigate(url, timeout_seconds) end

---@param url string
---@param js_code string
---@param timeout_seconds? integer
---@return any result
---@return string|nil err
function mycelium.browser.eval(url, js_code, timeout_seconds) end

---First request whose URL matches url_pattern (glob) while trigger_url loads.
---@param trigger_url string
---@param url_pattern string
---@param timeout_seconds? integer
---@param opts? {referer?: string}
---@return string|nil hit_url
---@return table<string,string>|nil headers
---@return string|nil err
function mycelium.browser.sniff(trigger_url, url_pattern, timeout_seconds, opts) end

-- ── dom ─────────────────────────────────────────────────────────────────────

---@class mycelium.Node
---@field text string
---@field html string
---@field attr fun(name: string): string

---@class mycelium.dom
mycelium.dom = {}

---Text of the first match.
---@param html string
---@param css string
---@return string
function mycelium.dom.query(html, css) end

---@param html string
---@param css string
---@return string[]
function mycelium.dom.query_all(html, css) end

---Attribute of the first match.
---@param html string
---@param css string
---@param name string
---@return string
function mycelium.dom.attr(html, css, name) end

---@param html string
---@param css string
---@return mycelium.Node[]
function mycelium.dom.select(html, css) end

-- ── json / xml ──────────────────────────────────────────────────────────────

---@class mycelium.json
mycelium.json = {}

---@param str string
---@return any value
---@return string|nil err
function mycelium.json.parse(str) end

---@param value any
---@return string
function mycelium.json.stringify(value) end

---One value at a gjson path ("path.to[0].field") without parsing the rest.
---@param str string
---@param path string
---@return any
function mycelium.json.get(str, path) end

---@param str string
---@param path string
---@return any[]
function mycelium.json.get_array(str, path) end

---Walks the array at a dot path (nil = the document) one element at a time;
---the callback returning false stops.
---@param str string
---@param path string|nil
---@param fn fun(item: any, i: integer): boolean|nil
---@return integer count
---@return string|nil err
function mycelium.json.each(str, path, fn) end

---@class mycelium.xml
mycelium.xml = {}

---@param str string
---@return table|nil node `{ _tag, _text, _attrs, [1..n] children }`
---@return string|nil err
function mycelium.xml.parse(str) end

-- ── crypto ──────────────────────────────────────────────────────────────────

---@class mycelium.crypto
mycelium.crypto = {}

---Standard, URL-safe or raw base64.
---@param b64 string
---@return string|nil
---@return string|nil err
function mycelium.crypto.base64_decode(b64) end

---@param s string
---@return string
function mycelium.crypto.base64_encode(s) end

---AES-CBC 128/256.
---@param ciphertext_b64 string
---@param key_hex string
---@param iv_hex string
---@return string|nil
---@return string|nil err
function mycelium.crypto.aes_decrypt(ciphertext_b64, key_hex, iv_hex) end

-- ── cache ───────────────────────────────────────────────────────────────────

---Redis-backed, per plugin; no-ops (false) without Redis.
---@class mycelium.cache
mycelium.cache = {}

---@param key string
---@param value string
---@param ttl_seconds integer
---@return boolean ok
---@return string|nil err
function mycelium.cache.set(key, value, ttl_seconds) end

---A stored "" is ("", true); a missing key ("", false).
---@param key string
---@return string value
---@return boolean hit
function mycelium.cache.get(key) end

---@param key string
function mycelium.cache.del(key) end

---Read, or compute with fn and store (as JSON). fn may return a ttl for this
---result; nil stores nothing. States asking for a key being computed wait.
---@param key string
---@param ttl_seconds integer
---@param fn fun(): any, integer|nil
---@return any value
---@return boolean hit
function mycelium.cache.remember(key, ttl_seconds, fn) end

-- ── ratelimit ───────────────────────────────────────────────────────────────

---Named token buckets shared by every state of the plugin.
---@class mycelium.ratelimit
mycelium.ratelimit = {}

---@param name string
---@param per_second number
---@param burst integer
function mycelium.ratelimit.set(name, per_second, burst) end

---False when a token is more than max_wait seconds away (default 30, max 60).
---@param name string
---@param max_wait_seconds? number
---@return boolean
function mycelium.ratelimit.wait(name, max_wait_seconds) end

---No tokens for this long (e.g. after a 429).
---@param name string
---@param seconds number
function mycelium.ratelimit.pause(name, seconds) end

-- ── context ─────────────────────────────────────────────────────────────────

---@class mycelium.context
---@field plugin_id string this plugin's manifest id
mycelium.context = {}

---An admin-configured setting of this plugin ("" if unset).
---@param key string
---@return string
function mycelium.context.get_global_setting(key) end

---Per profile ("" if unset).
---@param key string
---@return string
function mycelium.context.get_secret(key) end

---@param key string
---@param value string
---@return boolean ok
---@return string|nil err
function mycelium.context.set_secret(key, value) end

---@return string
function mycelium.context.get_profile_id() end

---@param label "ready"|"syncing"|"needs_config"|"error"
---@param detail? string "" clears
function mycelium.context.set_plugin_status(label, detail) end

-- ── storage ─────────────────────────────────────────────────────────────────

---JSON files in the plugin's data area; *.json names only.
---@class mycelium.storage
mycelium.storage = {}

---@param name string
---@return any|nil value
---@return string|nil err
function mycelium.storage.read_json(name) end

---Sub-directories are created.
---@param name string
---@param value any
---@return string|nil err
function mycelium.storage.write_json(name, value) end

---@param name string
---@return boolean
function mycelium.storage.exists(name) end

---Missing = nil.
---@param name string
---@return string|nil err
function mycelium.storage.remove(name) end

---Like json.each on a file; missing file = 0, nil.
---@param name string
---@param path string|nil
---@param fn fun(item: any, i: integer): boolean|nil
---@return integer count
---@return string|nil err
function mycelium.storage.each_json(name, path, fn) end

-- ── index ───────────────────────────────────────────────────────────────────

---@class mycelium.IndexBuilder
local IndexBuilder = {}

---Adds value under every key (nil/"" keys skipped).
---@param value any
---@param ... string|number|nil keys
function IndexBuilder:add(value, ...) end

---Publishes the index under name, replacing the previous one for every state.
---@param name string
---@return integer|nil records
---@return string|nil err
function IndexBuilder:publish(name) end

---Read-only lookup tables kept in Go, shared by every state; memory only.
---@class mycelium.index
mycelium.index = {}

---@return mycelium.IndexBuilder
function mycelium.index.builder() end

---@param name string
---@return boolean
function mycelium.index.has(name) end

---First value added under key, or nil (a fresh copy).
---@param name string
---@param key string|number
---@return any
function mycelium.index.get(name, key) end

---Every value under key, in insertion order.
---@param name string
---@param key string|number
---@return any[]
function mycelium.index.get_all(name, key) end

---Sorted keys with this prefix — for tasks, not hot paths.
---@param name string
---@param prefix? string
---@return string[]
function mycelium.index.keys(name, prefix) end

---@param name string
---@return {records: integer, keys: integer, bytes: integer, built_at: integer}|nil
function mycelium.index.info(name) end

---@param name string
function mycelium.index.drop(name) end

-- ── image ───────────────────────────────────────────────────────────────────

---@class mycelium.LogoInfo
---@field is_dark boolean
---@field norm_w integer
---@field norm_h integer
---@field error? string

---@class mycelium.image
mycelium.image = {}

---Not needed for logo_url any more (the core measures logos); kept for
---existing plugins.
---@param url string
---@return mycelium.LogoInfo|nil
---@return string|nil err
function mycelium.image.analyze_logo(url) end

---Up to 32, 3 at a time.
---@param urls string[]
---@return mycelium.LogoInfo[]
function mycelium.image.analyze_logos(urls) end

---A 16:9 cover drawn from two badges ("mycelium:/img/versus?…"), or nil.
---@param home_badge_url string
---@param away_badge_url string
---@return string|nil
function mycelium.image.versus_url(home_badge_url, away_badge_url) end

-- ── misc ────────────────────────────────────────────────────────────────────

---Into the plugin's log buffer.
---@param message string
function mycelium.log(message) end

---Narrates a long call (e.g. a resolve) to the client.
---@param status ""|"loading"|"success"|"warning"|"error"
---@param message? string
function mycelium.progress(status, message) end

---Capped, and cut short when the call is abandoned.
---@param milliseconds integer
function mycelium.sleep(milliseconds) end

-- ── plugintest (only inside a test file run by cmd/plugintest) ──────────────

---@class plugintest
plugintest = {}

---The most recently added route whose Go regexp matches the URL answers.
---@param pattern string
---@param response string|{status?: integer, body?: string, headers?: table<string,string>}
function plugintest.route(pattern, response) end

---@return {method: string, url: string, headers: table<string,string>}[]
function plugintest.requests() end

function plugintest.clear() end

---Fails the test file when cond is false.
---@param cond any
---@param message? string
function check(cond, message) end

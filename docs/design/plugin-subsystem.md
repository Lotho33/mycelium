# Design — sottosistema plugin

> Stato: **proposta**, in attesa di approvazione. Nessun codice modificato.
> Contesto: preparazione alla pubblicazione di `mycelium` (vedi
> `~/.claude/plans/mighty-jumping-gadget.md`). Il codice non è ancora completo;
> questo documento fissa la direzione del sottosistema plugin prima di
> toccarlo.

---

## 1. Stato attuale — mappa

Oggi in `mycelium` **coesistono due sistemi di plugin distinti**, con due SDK,
due formati di manifest e due scheduler.

### 1.A — Plugin binari gRPC (`managers.Plugin`)

Il sistema **descritto dal README**. File:
`internal/managers/plugin.go`, `internal/managers/grpc_adapters.go`,
`internal/managers/plugin_resources*.go` (4 file, `//go:build` per OS),
`pkg/sdk/`, `pkg/models/`, `third_party/stipes-sdk/sdk/serve.go`
(`ServeProvider`/`ServeEnricher` + adapter provider/enricher).

- **Formato**: `manifest.json` (`features`, `cron_jobs` con
  `default_hours`/`default_minutes`, `settings[]`, `catalogs[]`,
  `min_core_version`) + un binario eseguibile nella stessa cartella.
- **Ciclo di vita**: `LoadPlugins()` scansiona `plugins/`, per ogni binario:
  `exec.Command` → legge la riga JSON di announcement su stdout → dial gRPC
  su `127.0.0.1:<porta>` → `Init` + `Setup`. Watchdog `watchPlugin` con
  backoff esponenziale (poll 15s, max 5m). Scheduler per-job
  `runCronScheduler` (intervallo riletto dai settings ogni ciclo, fire
  immediato al boot). Campionamento risorse via `/proc` (Linux) su ticker 5s.
- **Handshake magic**: il core lancia il subprocess con
  `MYCELIUM_MAGIC=mycelium_plugin_v2` (`plugin.go:98`). L'SDK Go
  (`third_party/stipes-sdk/sdk/serve.go:16-17`) verifica invece
  `FUNGOBOX_MAGIC == "fungobox_plugin_v2"` e fa `os.Exit(1)` se non combacia.
  **I due non combaciano**: un plugin compilato con l'SDK fornito si spegne
  all'avvio.
- **Uso reale**: **zero** plugin binari nel repo. `RegisterProvider` (il
  bridge Lua→registry gRPC, `plugin.go:1075`) **non è mai chiamato** (grep:
  solo la definizione). `managers.Plugin.GetAllProviders()` ritorna sempre
  `{}`; l'handler `/` logga "0 plugin loaded"; il ramo gRPC di
  `lookupBackend` (`internal/pileus/pipeline_backend.go:63`) è di fatto
  irraggiungibile. Il codice resta referenziato in ~30 punti
  (`internal/api/admin.go`, `proxy.go`, `catalog.go`, `media_handler.go`) ma
  ritorna sempre vuoto / not-found.

### 1.B — Plugin Lua (`engine.LuaPlugins`)

**Ciò che il prodotto usa davvero.** mycelium non spedisce nessun plugin: la
cartella `plugins/` è gitignorata e l'operatore ci mette i propri (o vi si
estrae uno `.zip` di release). I plugin sono file Lua. File:
`internal/engine/lua_plugin.go` (~1700 righe), `lua_pool.go`, `lua_scope.go`,
`lua_sdk.go`, `lua_hotreload.go`, `lua_manifest_test.go`,
`internal/api/lua_admin.go`, `internal/pileus/lua_pipeline.go`.

- **Formato**: `manifest.yaml` — `id`, `settings.global[]`
  (`{id,label,type,hidden,required}`), `exposes` (`capabilities[]`,
  `catalogs[]` con hint di layout), `entrypoints` (mappa
  nome-pipeline → nome-funzione-lua), `tasks[]`
  (`{function, cron, live_refresh, timeout_seconds}`), più flag di rete
  (`direct_egress`, `vpn_optional`, `direct_stream`, `pool_size`).
- **Runtime**: pool di N `*lua.LState` per plugin (`pool_size`, default 2),
  ognuno pre-caricato con `init.lua` + i `.lua` fratelli come moduli
  `require()` + `plugins/shared/*.lua`. `RegisterSDK` costruisce le tabelle
  `mycelium.*` **una volta per LState**; lo stato per-chiamata (profileID,
  requireProxy, proxyURL, onProgress) viaggia in un `callScope` nel registry
  Lua. `callWithTimeout` corre la chiamata contro il `context` (default 30s,
  override `timeout_seconds` per task, cap 15m); su timeout l'LState è
  abbandonato e sostituito (`DiscardAndReplace`) — Go non può uccidere una
  goroutine.
- **Ciclo di vita** (`RunState`): `waiting` (setting `required` mancante) →
  `stopped` (mai avviato o fermato) → `running`. Solo `running` schedula task
  / serve entrypoint / compare in `ListPlugins`. Start/stop = un flag nei
  settings riletto a ogni tick. `restart` = `UnloadPlugin` + `LoadPlugin`
  (rilegge manifest e script da disco).
- **Scheduler** (`runTasks`, una goroutine per plugin):
  - Al boot, per ogni task: `shouldRunNow` — `cron:""` ⇒ mai (manual-only,
    compare in dashboard con "Esegui"); altrimenti confronta l'ultima
    esecuzione (chiave Redis `mycelium:task:lastrun:<id>:<fn>`) con
    l'intervallo di `@every`. Se scaduto ⇒ `go run(t)` subito.
  - Poi un `robfig/cron` con un entry per ogni task con cron non vuoto.
  - Concorrenza: `bgTaskSem` (canale, **capacità 2**, process-wide su TUTTI i
    plugin) + `p.taskMu` (per-plugin, un task alla volta). Trigger manuale
    (`runLuaTask`) e `TriggerLiveRefresh` ri-usano gli stessi due lock via
    `TryAcquire*` (non bloccante).
  - `MarkTaskRan` **non** scrive il marker se `GetStatus(id).Label == error`
    (uno scrape andato a vuoto deve poter ritentare prima del prossimo tick).
  - Ogni `recover()` è manuale in ogni goroutine nuda (3 copie del pattern
    "bgTaskSem + taskMu + recover + RecordTaskResult + MarkTaskRan").
- **Salute / stato**:
  - `pluginHealth` (Redis): `{last_ok_unix, consecutive_failures}` —
    **condiviso fra tutti i task del plugin**, soglia 2 fallimenti
    consecutivi ⇒ "non raggiungibile".
  - `PluginStatus` (opt-in, via `mycelium.context.set_plugin_status`):
    `{label, detail}` libero. `statusFallback` deriva `syncing` per un plugin
    `running` con task schedulati che non ha ancora avuto un successo.
- **Hot-reload**: `fsnotify` (fallback: polling mtime 2s) sulla cartella del
  plugin ⇒ `pool.Reload()`.
- **Admin** (`/admin/lua-plugins/*`): list, settings get/save (allowlist =
  chiavi del manifest), `run-task`, `state` (start/stop/restart), `upload`
  (ZIP), `uninstall`, `logs` (+ SSE), `stats` (n. righe log + `loaded`),
  editor file (list/get/put, hot-reload raccoglie la modifica).
- **Superficie SDK** (`mycelium.*`, costruita in `lua_sdk.go`):

  | modulo | funzioni | note |
  |---|---|---|
  | `network` | `get` `post` `fetch` | retry ×3 su errori di trasporto; header `_proxy` = opt-in proxy per i plugin `direct_egress`; UA default Chrome/120 fisso |
  | `browser` | `sniff` `navigate` `eval` | via `browserAPI` → client HTTP verso il servizio browser/extractor esterno (`internal/managers/browser_client.go`) |
  | `dom` | `query` `query_all` `select` `attr` | goquery |
  | `json` | `parse` `stringify` `get` `get_array` | encoding/json + gjson |
  | `xml` | `parse` | → tabella lua |
  | `crypto` | `base64_decode` `aes_decrypt` | AES-CBC 128/256 |
  | `cache` | `set` `get` `del` | Redis TTL, namespace `mycelium:plugin:<id>:cache:` |
  | `context` | `get_secret` `set_secret` `get_profile_id` `get_global_setting` `set_plugin_status` `plugin_id` `profile_id` | secret per-profilo in Redis hash |
  | `storage` | `read_json` `write_json` `exists` | **file nella cartella del plugin** (path-traversal bloccato) |
  | `image` | `analyze_logo` | luminanza + dimensioni normalizzate |
  | — | `log` `progress` `sleep` | `progress(status,msg)` inoltrato a `onProgress` (usato da `resolve_stream`) |

---

## 2. Problemi

### 2.1 Due sistemi, uno morto
`pkg/sdk` + `pkg/models` + `managers/plugin.go` + `grpc_adapters.go` +
`plugin_resources*.go` + gli adapter `serve.go` + l'**intera** sezione
"Writing a plugin" del README descrivono un percorso che non funziona
(magic env disallineato) e non spedisce nulla. È superficie da mantenere,
da spiegare e da far revisionare, per zero valore attuale. Per un repo che
deve risultare "pulito e minimale" è il primo candidato alla rimozione.

### 2.2 Scheduler
- **Thundering herd**: `shouldRunNow` ritorna `true` per *ogni* task quando
  Redis è assente; al boot partono tutti, frenati solo da `bgTaskSem` (cap 2),
  senza jitter.
- **Salute per-plugin, non per-task**: un task leggero che passa maschera un
  task pesante rotto (il commento di `statusFallback` lo ammette). Nessun
  record per-task: la dashboard non può dire "task X: ultima esecuzione 3h fa,
  durata 40s, fallito, prossima fra 9h".
- **`stats` inutile**: ritorna solo numero di righe di log e `loaded`.
- **Retry accoppiato al plugin**: `MarkTaskRan` salta il marker in base a uno
  stato che il plugin stesso scrive — comportamento difficile da prevedere.
- **Tre copie** del dance "sem + lock + recover + record + mark"
  (`runTasks.run`, `TriggerLiveRefresh`, `runLuaTask`).
- **`warmupCatalogCounts`** gira dopo *ogni* task di *ogni* plugin e occupa
  uno slot di `bgTaskSem`, competendo col lavoro vero.
- **`cron:""` sovraccarico** come "manual-only": convenzione non documentata
  nello schema del manifest per chi scrive plugin.

### 2.3 Installazione / update
- Nome cartella = nome-file dello ZIP sanitizzato (`sanitizeName`, `filepath.Base`
  + strip `..`), **non** l'`id` del manifest ⇒ possibile disallineamento;
  `uninstall` deve poi scan-and-match per `id`.
- Nessuno `schema_version` del manifest; nessun `min_core_version` per i
  plugin Lua (il percorso gRPC ce l'ha, Lua no).
- Nessuna verifica di integrità/firma; l'update è "ricarica lo ZIP e
  sovrascrive"; nessun confronto di versione, nessun rollback.
- Nessuna dichiarazione di capability: un plugin Lua può chiamare qualunque
  modulo `mycelium.*`; l'admin non vede *prima* di abilitarlo cosa farà
  (rete? browser? storage su disco?).
- Nessun indice/catalogo: la discovery è "carica uno ZIP preso da qualche
  parte".

### 2.4 Superficie API ai plugin
- **Non versionata**: nessun `mycelium.api_version`; un plugin non può
  sapere quali funzioni esistono nel core su cui gira.
- **Non capability-gated** (vedi 2.3).
- **Non documentata** come contratto stabile: sparsa nei commenti di
  `lua_sdk.go`. Il README documenta l'SDK *gRPC*, non quello Lua reale.
- **`storage.write_json` scrive nella cartella del plugin** ⇒ muta l'albero
  installato a runtime (è la causa della lotta col `.gitignore` sui vari
  `catalog_cache.json`). Il percorso gRPC ha già `data/<id>/` + `__data_dir`
  iniettato: qui manca.
- `network`: UA fisso, nessun rate-limit condiviso per host, retry
  hardcoded; `log` è un solo `println` in un ring buffer, senza livelli.

---

## 3. Opzioni

### Opzione A — Core Lua-only (**consigliata**)
Rimuovere il sistema binario gRPC: `pkg/sdk` (interfacce `Provider`/`Enricher`),
`internal/managers/plugin.go`, `grpc_adapters.go`, `models_conv.go`,
`plugin_resources*.go` (4 file), `internal/pileus/grpc_pipeline.go`, gli
adapter provider/enricher in `third_party/stipes-sdk/sdk/serve.go`, la sezione
"Writing a plugin" del README e il ramo gRPC di `lookupBackend`.
**`pkg/models` resta**: `internal/pileus/lua_pipeline.go` (percorso Lua vivo)
lo usa per le costanti di tipo condivise (`models.TypeMovie`, `TypeEpisode`,
…). `third_party/mycelium-sdk` resta **solo** per il contratto proto con
`pileus` (`media.proto`/`auth.proto`/`mycelium.proto`) — il `ProviderService`
proto, se non serve a pileus, si sfoltisce.

Il budget di complessità liberato va nel sistema Lua:
1. **Scheduler centrale** (`internal/engine/scheduler.go` nuovo): una sola
   coda job + worker pool con concorrenza configurabile; lock per
   `(plugin, task)`; jitter sul fire di boot; un `recover` solo. Ogni run
   produce un **record per-task** persistito
   (`mycelium:task:<id>:<fn>` → `{last_start, last_end, duration_ms, ok,
   error, runs, next_at}`) esposto da `/admin/lua-plugins` e in dashboard.
   `bgTaskSem`/`taskMu`/le 3 copie del pattern spariscono.
2. **Salute per-task**: `consecutive_failures` per singolo task, non per
   plugin; `statusFallback` legge il peggiore.
3. **Manifest v2** (retro-compatibile): `schema_version`, `min_core_version`,
   `api_version` (versione della superficie `mycelium.*` richiesta),
   `needs: [network, browser, storage, cache, crypto, image]` — l'admin lo
   vede prima di avviare; il core può negare i moduli non dichiarati.
   `tasks[].manual: true` sostituisce il `cron:""` sovraccarico (il vecchio
   resta accettato).
4. **Dir dati per-plugin** fuori dall'albero installato:
   `data/plugins/<id>/`, iniettata come base di `mycelium.storage.*`. La
   cartella del plugin torna read-only a runtime.
5. **Contratto SDK documentato**: `docs/plugin-sdk.md` (l'unico "come si
   scrive un plugin" del README punta qui), generato/mantenuto vicino a
   `lua_sdk.go`; `mycelium.api_version` esposto ai plugin.

Costo: medio. Rimozione netta (poche centinaia di righe + 6 file) + ~1
modulo nuovo (scheduler) + manifest v2 additivo. Nessun impatto su `pileus`
(contratto proto invariato). Nessun impatto sui plugin Lua esistenti
(manifest v1 continua a caricare).

### Opzione B — Tenere entrambi, riparare il percorso binario
Allineare il magic env, spedire un plugin Go d'esempio funzionante,
documentare entrambi gli SDK. Raddoppia la superficie da mantenere e
revisionare. Ha senso **solo** se plugin out-of-process / in altri
linguaggi sono un obiettivo concreto a breve. Non lo sono oggi.

### Opzione C — Unificare sul registry `sdk.Provider`, un solo trasporto
Tenere `managers.Plugin` come unico registry di provider, far registrare i
plugin Lua lì dentro via `RegisterProvider` (il bridge che esiste ma è
inutilizzato), eliminare solo il trasporto subprocess/gRPC. Architettura
interna più pulita, sforzo medio, lascia una cucitura per un trasporto
futuro. Ma mantiene `pkg/sdk` + `pkg/models` + gli adapter e non riduce
molto la superficie visibile.

---

## 4. Raccomandazione

**Opzione A.** Il valore di `mycelium` per chi scrive plugin è la via Lua
in-process: nessuna toolchain, hot-reload, editor in dashboard — "fattibile
per tutti". Il sistema binario gRPC è costo puro finché non c'è una domanda
reale di plugin poliglotti; quando ci sarà, si reintroduce un trasporto
sopra un'interfaccia `Provider` già pulita, senza il debito attuale.

### Ordine di esecuzione proposto (dentro la finestra M1→M2)
1. **M2** — rimozione del sistema binario gRPC (parte di "M2.3 dead code").
   `pkg/models` resta (lo usa `lua_pipeline.go`); sfoltire il `ProviderService`
   in `mycelium.proto` se pileus non lo tocca.
2. **Post-M2 / iterazione dedicata** — scheduler centrale + record per-task +
   salute per-task.
3. **Post-M2** — manifest v2 additivo (`schema_version`, `min_core_version`,
   `api_version`, `needs`, `tasks[].manual`) + dir dati per-plugin +
   `docs/plugin-sdk.md`.

Punti 2 e 3 non bloccano la pubblicazione: il sistema Lua attuale funziona.
Sono il "meglio strutturato" che l'utente ha chiesto, da fare a valle della
messa in sicurezza.

---

## 5. Domande aperte per l'utente

1. **Rimozione del sistema binario gRPC**: ok procedere in M2? (elimina
   `pkg/sdk`, `pkg/models`?, `managers/plugin.go`, `grpc_adapters.go`,
   `plugin_resources*.go`, adapter in `serve.go`, sezione README).
2. **`data/plugins/<id>/` per lo storage dei plugin**: ok cambiare la
   semantica di `mycelium.storage.*` (con migrazione one-shot dei file
   esistenti nella cartella del plugin)?
3. **Manifest v2**: i campi proposti (`schema_version`, `min_core_version`,
   `api_version`, `needs`, `tasks[].manual`) coprono il fabbisogno o ne
   servono altri (es. `provides`, quote di risorse, priorità di scheduling)?
4. **`needs` enforced o solo informativo** in prima versione? (enforced =
   il core nega i moduli non dichiarati; informativo = solo mostrato in
   dashboard).

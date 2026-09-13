// ─── State ───────────────────────────────────────────────────────────────────
let _installing = false;
let _cachedClients = null;
const _pluginLogSSE = {};   // pid → EventSource
let _logEventSource = null;
let _editorPlugin = null;   // { id, name, dir }
let _editorDirty = false;
let _editorCurrentPath = null;
const _tabOnOpen = {};      // `${pid}:${tabId}` → callback fn (replaces eval)

// ─── Fetch helper ─────────────────────────────────────────────────────────────
async function apiFetch(url, opts = {}) {
    const res = await fetch(url, { credentials: 'include', ...opts });
    if (res.status === 401 || res.status === 403) { window.location.href = '/admin/login'; return null; }
    return res;
}

// apiErrorMessage estrae un messaggio d'errore utile dalla risposta di
// apiFetch invece di scartarlo — alcuni handler rispondono JSON ({"message":
// "..."}), altri testo semplice via http.Error(). Usata dai punti che prima
// mostravano solo un errore generico ("Errore salvataggio.") scartando
// qualunque dettaglio il backend avesse effettivamente fornito.
async function apiErrorMessage(r, fallback) {
    if (!r) return fallback;
    try {
        const text = await r.text();
        if (!text) return fallback;
        try {
            const d = JSON.parse(text);
            return d.message || d.error || fallback;
        } catch {
            return text.length < 200 ? text : fallback;
        }
    } catch {
        return fallback;
    }
}

// ─── Toast ────────────────────────────────────────────────────────────────────
function showToast(msg, type = 'success') {
    const colors = { success:'bg-emerald-800 border-emerald-600', error:'bg-red-900 border-red-700',
                     info:'bg-blue-900 border-blue-700', warn:'bg-amber-900 border-amber-700' };
    const el = document.createElement('div');
    el.className = `${colors[type]||colors.success} border text-white px-4 py-2.5 rounded-xl shadow-2xl text-xs font-medium
                    translate-x-full opacity-0 transition-all duration-300`;
    el.textContent = msg;
    document.getElementById('toast-container').appendChild(el);
    requestAnimationFrame(() => el.classList.remove('translate-x-full', 'opacity-0'));
    setTimeout(() => { el.classList.add('translate-x-full', 'opacity-0'); setTimeout(() => el.remove(), 300); }, 4000);
}

// ─── Overlay ──────────────────────────────────────────────────────────────────
function setInstalling(on) {
    _installing = on;
    document.getElementById('install-overlay')?.classList.toggle('hidden', !on);
}

// ─── Utils ────────────────────────────────────────────────────────────────────
// esc(): escaping SOLO per contesto testo/attributo HTML double-quoted.
// Include l'apice singolo per difesa in profondità, ma resta comunque
// SBAGLIATO usarla per costruire un onclick="fn('${...}')": un browser
// decodifica le entity HTML di un attributo PRIMA di valutarne il contenuto
// come JS, quindi anche &#39; torna un apice vero e chiude la stringa JS.
// Il fix corretto per quel contesto è il pattern data-*/listener delegato
// (vedi loadPileusDevices e buildCard) — mai onclick costruito per stringa.
const esc = str => String(str).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
function fmtBytes(b) { if (!b||b<=0) return '—'; return b<1048576?(b/1024).toFixed(0)+' KB':(b/1048576).toFixed(1)+' MB'; }
function fmtUptime(s) {
    if (!s||s<=0) return '—';
    const h=Math.floor(s/3600),m=Math.floor((s%3600)/60),ss=s%60;
    return h>0?`${h}h ${m}m`:m>0?`${m}m ${ss}s`:`${ss}s`;
}
function logColor(line) {
    if (!line) return 'text-gray-600';
    if (/ERROR|❌|💥/.test(line)) return 'text-red-400';
    if (/WARN|⚠/.test(line)) return 'text-yellow-400';
    if (/✅|INFO|🔌|📋|🚀/.test(line)) return 'text-emerald-400';
    if (/\[lua:[^\]]+\]/.test(line)) return 'text-sky-400';
    return 'text-gray-500';
}

async function logout() { await apiFetch('/admin/logout',{method:'POST'}); window.location.href='/admin/login'; }
async function clearCache() {
    const r = await apiFetch('/admin/cache/clear',{method:'POST'});
    showToast(r?.ok?'Cache svuotata.':'Errore flush cache.',r?.ok?'success':'error');
}

// ─── Danger zone ─────────────────────────────────────────────────────────────
async function _dangerPost(url, confirmMsg, okMsg) {
    if (!window.confirm(confirmMsg)) return;
    const pw = window.prompt('Password admin per confermare:');
    if (!pw) return;
    const r = await apiFetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: pw }),
    });
    if (!r) return;
    if (r.ok) {
        const d = await r.json().catch(() => ({}));
        showToast(typeof okMsg === 'function' ? okMsg(d) : okMsg, 'success');
    } else {
        showToast(await apiErrorMessage(r, 'Operazione fallita.'), 'error');
    }
}

function wipeProfiles() {
    _dangerPost(
        '/admin/profiles/wipe',
        'Cancellare TUTTI i profili, la loro cronologia e i secret per-profilo? I dispositivi restano accoppiati. Irreversibile.',
        d => `Profili cancellati: ${d.profiles ?? 0}, cronologia: ${d.history_rows ?? 0}, chiavi Redis: ${d.redis_keys ?? 0}.`,
    );
}

function wipePluginData() {
    _dangerPost(
        '/admin/plugins/wipe-data',
        'Cancellare TUTTI i dati dei plugin (FLUSHDB Redis + cache immagini WebP + cache su disco)? I plugin restano installati. Irreversibile.',
        d => {
            const extra = d.redis_error ? ` — errore Redis: ${d.redis_error}` : '';
            return `Dati plugin cancellati: ${d.webp_files ?? 0} immagini WebP, ${d.plugin_cache_files ?? 0} file cache${extra}.`;
        },
    );
}

// ─── Cambio password admin ────────────────────────────────────────────────────
async function changeAdminPassword(ev) {
    ev.preventDefault();
    const curEl = document.getElementById('pw-current');
    const newEl = document.getElementById('pw-new');
    const current = curEl.value;
    const next = newEl.value;
    if (next.length < 8) { showToast('La nuova password deve essere di almeno 8 caratteri.', 'error'); return; }

    const r = await apiFetch('/admin/password/change', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ current_password: current, new_password: next }),
    });
    if (!r) return;
    if (r.ok) {
        curEl.value = '';
        newEl.value = '';
        showToast('Password admin aggiornata.', 'success');
    } else {
        showToast(await apiErrorMessage(r, 'Cambio password fallito.'), 'error');
    }
}

// ─── Side drawer + pages ─────────────────────────────────────────────────────
const _pages = ['overview','plugins','settings','vpn','challenges','download','logs'];
const _pageTitles = {
    overview: 'Home', plugins: 'Plugin', settings: 'Impostazioni',
    vpn: 'VPN', challenges: 'Verifiche in sospeso', download: 'Download per iOS', logs: 'Log di Sistema',
};
let _activeTab = 'overview';

function openNav() {
    document.getElementById('sidebar')?.classList.remove('-translate-x-full');
    document.getElementById('nav-backdrop')?.classList.remove('hidden');
}
function closeNav() {
    // lg breakpoint keeps the drawer docked via the lg:translate-x-0 class;
    // re-adding -translate-x-full only matters below lg.
    document.getElementById('sidebar')?.classList.add('-translate-x-full');
    document.getElementById('nav-backdrop')?.classList.add('hidden');
}

function switchTab(id) {
    if (!_pages.includes(id)) id = 'overview';
    _activeTab = id;
    _pages.forEach(t => {
        document.getElementById(`tab-${t}`)?.classList.toggle('hidden', t !== id);
        document.getElementById(`nav-btn-${t}`)?.classList.toggle('active', t === id);
    });
    const tt = document.getElementById('topbar-title');
    if (tt) tt.textContent = _pageTitles[id] || 'Mycelium';
    if (id === 'logs') connectLogSSE(); else disconnectLogSSE();
    if (id === 'vpn') loadEgress();
    if (id === 'challenges') { loadEgress().then(loadJar); loadChallenges(); }
    if (id === 'overview') loadPileusDevices();
    closeNav();
}

// ─── Core stats ───────────────────────────────────────────────────────────────
async function pollResources() {
    const [coreRes, sysRes] = await Promise.all([
        apiFetch('/admin/core/resources'),
        apiFetch('/admin/system/status'),
    ]);
    let core = null, sys = null;
    if (coreRes?.ok) { core = await coreRes.json(); updateCoreRes(core); }
    if (sysRes?.ok)  { sys = await sysRes.json();  updateSysStatus(sys); }
    if (core && sys) updateAggregateStats(core, sys);
}
// updateAggregateStats mostra le metriche del processo core (CPU/RAM di
// extractor e redis non sono più lette dall'API Docker — vedi ov-*-ok per la
// loro readiness). Il secondo argomento (system status) non serve più al
// calcolo ma resta nella firma per compatibilità col chiamante.
function updateAggregateStats(core, s) {
    const cpu = core.cpu_percent || 0;
    const mem = core.mem_bytes || 0;
    const set = (id,v) => { const e=document.getElementById(id); if(e) e.textContent=v; };
    set('agg-cpu',    cpu.toFixed(1)+'%');
    set('agg-mem',    fmtBytes(mem));
    set('agg-uptime', fmtUptime(core.uptime_sec));
    // Mirror into the mobile top bar (always visible, outside the tab divs).
    set('top-cpu',    cpu.toFixed(0)+'%');
    set('top-mem',    fmtBytes(mem));
}
function updateCoreRes(s) {
    const set = (id,v) => { const e=document.getElementById(id); if(e) e.textContent=v; };
    set('ov-core-pid',    s.pid??'—');
    set('ov-core-mem',    fmtBytes(s.mem_bytes));
    set('ov-core-cpu',    s.cpu_percent!=null?s.cpu_percent.toFixed(1)+'%':'—');
    set('ov-core-uptime', fmtUptime(s.uptime_sec));
}
// setStatDot colors a status dot: 'ok' (emerald), 'down' (red, unexpected —
// a component that should normally be up), or 'off' (gray, an expected
// not-configured/not-applicable state — not an alarm).
function setStatDot(id, state, title) {
    const el = document.getElementById(id);
    if (!el) return;
    const color = state === 'ok' ? 'bg-emerald-400' : state === 'down' ? 'bg-red-500' : 'bg-gray-600';
    el.className = el.className.replace(/bg-\S+/, color);
    if (title) el.title = title;
}

// updateSysStatus popola la card "Sistema" unica: Core (metriche da
// /admin/core/resources via updateCoreRes), Extractor (cobweb) e Redis
// (readiness — CPU/RAM container non più lette dall'API Docker). Il dot di
// testata riassume: verde se tutto ok, rosso se un pezzo che dovrebbe essere
// su non risponde.
function updateSysStatus(s) {
    const set = (id,v) => { const e=document.getElementById(id); if(e) e.textContent=v; };

    // Extractor (cobweb) — "ok" dal suo /health.
    setStatDot('ov-browser-dot', s.browser?.ok ? 'ok' : 'down',
        s.browser?.ok ? 'Pronto (' + (s.browser.engine || '?') + ')' : 'Non raggiungibile');
    set('ov-browser-engine', s.browser?.engine || '—');

    // Redis — readiness.
    setStatDot('ov-redis-dot', s.redis?.ok ? 'ok' : 'down');
    set('ov-redis-addr', s.redis?.addr || '—');

    // Dot di testata: rosso se extractor o redis non rispondono.
    const allOk = (s.browser?.ok) && (s.redis?.ok);
    setStatDot('sys-dot', allOk ? 'ok' : 'down',
        allOk ? 'Tutti i servizi attivi' : 'Un servizio non risponde');
}
// setStatusBox colora un box di stato in base all'esito: null = neutro/caricamento,
// true = successo (verde), false = errore (rosso). Usato sia per l'upload che
// per il test di connessione, sempre visibile nella modale (non dipende dai
// toast, che possono sfuggire se l'utente ha lo sguardo altrove).
function setStatusBox(id, text, ok) {
    const el = document.getElementById(id);
    if (!el) return;
    el.classList.remove('hidden');
    const palette = ok === true ? 'bg-emerald-900/40 text-emerald-400'
                  : ok === false ? 'bg-red-900/40 text-red-400'
                  : 'bg-gray-800/60 text-gray-400';
    el.className = `text-xs mt-2 px-3 py-2 rounded-lg ${palette}`;
    el.textContent = text;
}
async function openManualSession() {
    document.getElementById('warp-modal').classList.remove('hidden');
    await reattachVPNSessionIfAny();
}
// L'id della sessione manuale vive solo qui in JS: un refresh della pagina o
// un vnc-ws caduto senza che l'admin se ne accorga lo perdono, lasciando sul
// backend una sessione aperta che nessun pulsante sa più chiudere (409 su
// ogni nuovo tentativo). Riaggancia automaticamente all'apertura del modale.
async function reattachVPNSessionIfAny() {
    if (_vpnSessionId) return; // già agganciati in questa stessa pagina
    const r = await apiFetch('/admin/vpn/session/current');
    if (!r?.ok) return;
    const d = await r.json();
    if (!d.id) return;
    showToast(`Sessione già aperta su ${d.target_url} — riaggancio in corso.`, 'info');
    attachVPNSession(d.id);
}
function attachVPNSession(id) {
    _vpnSessionId = id;
    const wsPath = `admin/vpn/session/${id}/vnc-ws`;
    const iframe = document.getElementById('vpn-session-iframe');
    const statusEl = document.getElementById('vpn-session-status');
    iframe.src = `/admin/vpn/novnc/vnc_lite.html?path=${encodeURIComponent(wsPath)}&scale=true`;
    statusEl.textContent = `Connessione a ${wsPath}…`;
    iframe.onload = () => { statusEl.textContent = `Pagina noVNC caricata — stato di connessione visibile nel riquadro sotto.`; };
    document.getElementById('warp-modal-card').classList.replace('max-w-sm', 'max-w-5xl');
    document.getElementById('vpn-session-start').classList.add('hidden');
    document.getElementById('vpn-session-live').classList.remove('hidden');
}
// ─── Sessione browser manuale (noVNC) ──────────────────────────────────────────
// Apre una sessione headed su cobweb (instradata sullo stesso proxy WARP dei
// plugin VPN-routed) e la mostra dentro un iframe puntato sul client noVNC
// vendorizzato — servito da mycelium via reverse-proxy, mai direttamente da
// cobweb (vedi internal/api/vpn_session.go). Un solo id alla volta: lo stato
// vive qui, non nel backend, perché è la UI a sapere quale iframe chiudere.
let _vpnSessionId = null;
async function startVPNSession() {
    const url = document.getElementById('vpn-session-url-input')?.value.trim();
    if (!url) { showToast('Inserisci un URL.', 'warn'); return; }
    const direct = document.getElementById('vpn-session-direct')?.checked || false;
    const r = await apiFetch('/admin/vpn/session/start', {
        method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify({url, direct}),
    });
    if (!r?.ok) {
        if (r?.status === 409) {
            // Una sessione era già aperta (id perso lato JS per un refresh o
            // un vnc-ws caduto senza chiusura pulita) — riaggancio invece di
            // lasciare l'admin bloccato senza un modo per chiuderla.
            showToast('Una sessione era già aperta — riaggancio in corso.', 'warn');
            await reattachVPNSessionIfAny();
            return;
        }
        showToast(await apiErrorMessage(r, 'Errore apertura sessione.'), 'error');
        return;
    }
    const d = await r.json();
    // vnc_lite.html (non vnc.html): client noVNC minimale pensato apposta per
    // essere incorporato in un iframe — mostra sempre un testo di stato
    // visibile ("Loading"/"Connecting"/errore), a differenza dell'app
    // completa vnc.html la cui UI può restare invisibile in uno spazio
    // piccolo senza un errore chiaro da diagnosticare.
    attachVPNSession(d.id);
}
function discardVPNSession() { return closeVPNSession(false); }

async function closeVPNSession(save = true) {
    if (!_vpnSessionId) return;
    const id = _vpnSessionId;
    _vpnSessionId = null;
    // Azzerare src prima della chiamata di chiusura stacca subito il websocket
    // lato client, invece di lasciarlo agganciato a una sessione che il
    // backend sta per smontare.
    document.getElementById('vpn-session-iframe').src = 'about:blank';
    document.getElementById('vpn-session-live').classList.add('hidden');
    document.getElementById('vpn-session-start').classList.remove('hidden');
    document.getElementById('warp-modal-card').classList.replace('max-w-5xl', 'max-w-sm');
    const path = save ? 'close' : 'discard';
    const r = await apiFetch(`/admin/vpn/session/${id}/${path}`, { method: 'POST' });
    if (r?.ok) showToast(save
        ? 'Sessione chiusa, cookie salvati per le richieste automatiche.'
        : 'Sessione chiusa senza salvare i cookie.', 'success');
    else showToast(await apiErrorMessage(r, 'Errore chiusura sessione.'), 'error');
}

// ─── Verifiche di sicurezza in sospeso ───────────────────────────────────────
async function loadChallenges() {
    const card = document.getElementById('challenges-card');
    const box = document.getElementById('challenges-list');
    const empty = document.getElementById('challenges-empty');
    if (!card || !box) return;
    const setEmpty = (on) => { card.classList.toggle('hidden', on); empty?.classList.toggle('hidden', !on); };
    const r = await apiFetch('/admin/vpn/challenges');
    if (!r?.ok) { setEmpty(true); return; }
    const { entries = [] } = await r.json();
    if (!entries.length) { setEmpty(true); return; }
    setEmpty(false);
    box.innerHTML = entries.map(e => `
        <div class="panel border border-gray-800 rounded-lg px-3 py-2 flex items-center gap-2 flex-wrap">
            <span class="font-mono text-gray-200">${esc(e.domain)}</span>
            <span class="text-gray-600">${esc(e.egress)}</span>
            <span class="text-gray-600">vista ${e.hits}× · ultima ${_fmtWhen(e.last_seen)}</span>
            <span class="ml-auto flex gap-2">
                <button data-domain="${esc(e.domain)}" data-action="resolve" class="challenge-btn text-amber-400 hover:text-amber-300 transition font-semibold">Risolvi</button>
                <button data-domain="${esc(e.domain)}" data-action="dismiss" class="challenge-btn text-gray-500 hover:text-gray-300 transition">Ignora</button>
            </span>
        </div>`).join('');
}
// Listener delegato, agganciato una volta sola: box.innerHTML viene
// riscritto a ogni loadChallenges(), niente onclick="fn('${domain}')"
// costruito per concatenazione — domain arriva da cobweb (dominio target
// di una challenge), non è testo digitato dall'admin.
document.getElementById('challenges-list')?.addEventListener('click', e => {
    const btn = e.target.closest('.challenge-btn');
    if (!btn) return;
    const domain = btn.dataset.domain;
    if (btn.dataset.action === 'resolve') resolveJarDomain(domain);
    else if (btn.dataset.action === 'dismiss') dismissChallenge(domain);
});
async function dismissChallenge(domain) {
    const r = await apiFetch(`/admin/vpn/challenges/${encodeURIComponent(domain)}`, { method: 'DELETE' });
    if (r?.ok) loadChallenges();
}

// ─── Jar cobweb: cookie & sfide risolte ───────────────────────────────────────
function _fmtWhen(iso) {
    if (!iso) return '—';
    const d = new Date(iso), now = Date.now();
    const diff = Math.round((d - now) / 1000);
    const ago = diff < 0;
    const s = Math.abs(diff);
    const txt = s < 60 ? `${s}s` : s < 3600 ? `${Math.round(s/60)}m` : s < 86400 ? `${Math.round(s/3600)}h` : `${Math.round(s/86400)}g`;
    return ago ? `${txt} fa` : `tra ${txt}`;
}
async function loadJar() {
    const box = document.getElementById('jar-list');
    if (!box) return;
    const r = await apiFetch('/admin/vpn/jar');
    if (!r?.ok) { box.textContent = 'Jar non disponibile (cobweb raggiungibile?).'; return; }
    const { entries = [] } = await r.json();
    if (!entries.length) { box.textContent = 'Nessuna sfida risolta finora.'; return; }
    box.innerHTML = entries.map(e => {
        const empty = !e.cookie_count;
        const bad = e.stale || e.fail_streak > 0;
        const badge = empty
            ? '<span class="text-gray-500 border border-gray-700 rounded px-1.5 py-0.5">vuota</span>'
            : bad
            ? '<span class="text-amber-400 border border-amber-500/30 rounded px-1.5 py-0.5">da risolvere</span>'
            : '<span class="text-emerald-400 border border-emerald-500/30 rounded px-1.5 py-0.5">valida</span>';
        const exp = e.stale ? `scaduta ${_fmtWhen(e.expires_at)}` : `scade ${_fmtWhen(e.expires_at)}`;
        return `<div class="panel border border-gray-800 rounded-lg px-3 py-2 flex items-center gap-2 flex-wrap ${empty ? 'opacity-60' : ''}">
            <span class="font-mono text-gray-200">${esc(e.domain)}</span>
            ${badge}
            <span class="text-gray-600">${esc(_egressLabel(e.egress))}</span>
            <span class="text-gray-600">${e.cookie_count} cookie · ${exp}</span>
            <span class="ml-auto flex gap-2">
                <button data-domain="${esc(e.domain)}" data-action="resolve" class="jar-btn text-sky-400 hover:text-sky-300 transition">Risolvi</button>
                <button data-domain="${esc(e.domain)}" data-egress="${esc(e.egress)}" data-action="delete" class="jar-btn text-gray-500 hover:text-red-400 transition">Elimina</button>
            </span>
        </div>`;
    }).join('');
}
// Listener delegato — domain/egress vengono dalla jar di cobweb, non da
// input testuale dell'admin: mai in un onclick costruito per stringa.
document.getElementById('jar-list')?.addEventListener('click', e => {
    const btn = e.target.closest('.jar-btn');
    if (!btn) return;
    const domain = btn.dataset.domain;
    if (btn.dataset.action === 'resolve') resolveJarDomain(domain);
    else if (btn.dataset.action === 'delete') deleteJarEntry(domain, btn.dataset.egress);
});
async function resolveJarDomain(domain) {
    await openManualSession();
    const inp = document.getElementById('vpn-session-url-input');
    if (inp) inp.value = `https://${domain}/`;
    startVPNSession();
}
async function deleteJarEntry(domain, egress) {
    const q = egress ? `?egress=${encodeURIComponent(egress)}` : '';
    const r = await apiFetch(`/admin/vpn/jar/${encodeURIComponent(domain)}${q}`, { method: 'DELETE' });
    if (r?.ok) { showToast(`Stato di ${domain} eliminato.`, 'success'); loadJar(); }
    else showToast(await apiErrorMessage(r, 'Eliminazione fallita.'), 'error');
}

// ─── Uscite di rete (egress registry) ────────────────────────────────────────
// Ultimo elenco profili, riusato per popolare la <select> per-plugin senza
// una seconda GET quando si apre la scheda Plugin.
let _egressProfiles = [{ name: 'direct', kind: 'direct', enabled: true }];

// cobweb keys its jar by a sanitised proxy string (socks5://127.0.0.1:1080 →
// "proxy-127.0.0.1-1080"). Map it back to the egress profile name when we can,
// so the jar list reads "warp" instead of that.
function _egressLabel(name) {
    if (!name || name === 'direct') return 'Diretta';
    for (const p of _egressProfiles) {
        if (!p.proxy_url) continue;
        const san = 'proxy-' + p.proxy_url.replace(/^[a-z0-9]+:\/\//i, '').replace(/[.:]/g, '-').replace(/\/$/, '');
        if (san === name) return p.name;
    }
    return name;
}

async function loadEgress() {
    const box = document.getElementById('egress-list');
    const r = await apiFetch('/admin/egress');
    if (!r?.ok) { if (box) box.textContent = 'Registry non disponibile.'; return; }
    const { profiles = [] } = await r.json();
    _egressProfiles = profiles;
    if (!box) return;
    box.innerHTML = profiles.map(p => {
        const isDirect = p.name === 'direct';
        const isWg = p.kind === 'wireproxy';
        // For wireproxy the dot reflects the actual sidecar state, not just the flag.
        const dot = isDirect ? 'bg-gray-500'
            : isWg ? (p.enabled && p.running ? 'bg-emerald-400' : p.enabled ? 'bg-amber-400 pulse-dot' : 'bg-gray-700')
            : p.enabled ? 'bg-emerald-400' : 'bg-gray-700';
        const right = isDirect ? '<span class="text-gray-700">sempre attiva</span>' : `
            <button data-name="${esc(p.name)}" data-action="test" class="egress-btn text-sky-400 hover:text-sky-300 transition">Prova</button>
            <button data-name="${esc(p.name)}" data-action="toggle" data-enabled="${p.enabled ? '1' : '0'}" class="egress-btn text-gray-400 hover:text-gray-200 transition">${p.enabled ? 'Disattiva' : 'Attiva'}</button>
            <button data-name="${esc(p.name)}" data-action="delete" class="egress-btn text-gray-500 hover:text-red-400 transition">Elimina</button>`;
        const kindTag = p.kind === 'warp' ? 'WARP'
            : isWg ? `WireGuard · :${p.port}${p.enabled && !p.running ? ' · avvio…' : ''}`
            : p.proxy_url ? p.proxy_url : '';
        return `<div class="panel border border-gray-800 rounded-lg px-3 py-2 flex items-center gap-2 flex-wrap">
            <span class="inline-block w-2 h-2 rounded-full ${dot}"></span>
            <span class="font-semibold text-gray-200">${esc(_egressLabel(p.name))}</span>
            ${kindTag ? `<span class="font-mono text-gray-600 truncate max-w-[14rem]">${esc(kindTag)}</span>` : ''}
            <span class="ml-auto flex gap-2">${right}</span>
        </div>`;
    }).join('');
}
// Listener delegato — p.name è il nome scelto dall'admin quando crea
// l'uscita, ma resta comunque preferibile non passarlo mai per stringa in
// un onclick (coerenza col resto del file + zero costo).
document.getElementById('egress-list')?.addEventListener('click', e => {
    const btn = e.target.closest('.egress-btn');
    if (!btn) return;
    const name = btn.dataset.name;
    switch (btn.dataset.action) {
        case 'test':   testEgress(name); break;
        case 'toggle': toggleEgress(name, btn.dataset.enabled !== '1'); break;
        case 'delete': deleteEgress(name); break;
    }
});
async function addEgress(ev) {
    ev.preventDefault();
    const name = document.getElementById('egress-name').value.trim();
    const proxy_url = document.getElementById('egress-url').value.trim();
    const r = await apiFetch('/admin/egress', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, proxy_url, kind: 'proxy' }),
    });
    if (r?.ok) { showToast(`Uscita "${name}" aggiunta.`, 'success'); document.getElementById('egress-name').value = ''; document.getElementById('egress-url').value = ''; loadEgress(); }
    else showToast(await apiErrorMessage(r, 'Aggiunta fallita.'), 'error');
}
async function toggleEgress(name, enabled) {
    const r = await apiFetch(`/admin/egress/${encodeURIComponent(name)}/enabled`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ enabled }),
    });
    if (r?.ok) loadEgress();
    else showToast(await apiErrorMessage(r, 'Operazione fallita.'), 'error');
}
async function deleteEgress(name) {
    if (!confirm(`Eliminare l'uscita "${name}"? I plugin che la usano torneranno a "Diretta".`)) return;
    const r = await apiFetch(`/admin/egress/${encodeURIComponent(name)}`, { method: 'DELETE' });
    if (r?.ok) { showToast(`Uscita "${name}" eliminata.`, 'success'); loadEgress(); }
    else showToast(await apiErrorMessage(r, 'Eliminazione fallita.'), 'error');
}
// WireGuard .conf → wireproxy sidecar (Fase B)
function wgDrop(ev) {
    ev.preventDefault();
    const el = document.getElementById('wg-drop');
    if (el) el.classList.remove('border-sky-500', 'text-sky-400');
    const f = ev.dataTransfer?.files?.[0];
    if (f) submitWgFile(f);
}
async function submitWgFile(file) {
    if (!file) return;
    if (file.size > 128 * 1024) { showToast('File troppo grande per essere una .conf WireGuard.', 'error'); return; }
    const name = (document.getElementById('wg-name')?.value || 'wg').trim();
    const fd = new FormData();
    fd.append('name', name);
    fd.append('conf', file, file.name || 'wg0.conf');
    showToast(`Carico "${file.name}" e avvio wireproxy…`, 'info');
    const r = await apiFetch('/admin/egress/wireproxy', { method: 'POST', body: fd });
    if (r?.ok) { showToast(`Uscita WireGuard "${name}" attiva. Prova la connessione per verificarla.`, 'success'); loadEgress(); }
    else showToast(await apiErrorMessage(r, 'Caricamento .conf fallito.'), 'error');
}

async function testEgress(name) {
    showToast(`Provo "${name}"…`, 'info');
    const r = await apiFetch(`/admin/egress/${encodeURIComponent(name)}/test`, { method: 'POST' });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Prova fallita.'), 'error'); return; }
    const d = await r.json();
    const warp = d.warp && d.warp !== 'off' ? ` · warp=${d.warp}` : '';
    if (d.leaking) showToast(`⚠️ "${name}" non instrada: IP identico a quello diretto (${d.exit_ip}).`, 'error');
    else showToast(`"${name}" ok — uscita ${d.exit_ip || '?'} ${d.exit_loc || ''}${warp}`, 'success');
}

// ─── Pairing Pileus (codice rotante invece della password admin) ──────────────
// L'UI vive inline nella card "Pileus" della Panoramica (niente più modale) —
// il codice è sempre a un click di distanza. Un polling breve (2s) dopo la
// generazione rileva sia lo scadere del codice (countdown) sia il momento in
// cui un dispositivo lo consuma (pairing-done) — l'unico modo per saperlo,
// dato che AuthorizeDevice è una RPC gRPC chiamata da Pileus, non un'azione
// che passa da questa dashboard.
let _pairingPollTimer = null;
let _pairingExpiresAt = 0;

async function generatePairingCode() {
    const r = await apiFetch('/admin/pileus/pairing/token', { method: 'POST' });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Generazione codice fallita.'), 'error'); return; }
    const d = await r.json();
    _pairingExpiresAt = d.expires_at * 1000;
    document.getElementById('pairing-code').textContent = d.code;
    document.getElementById('pairing-idle').classList.add('hidden');
    document.getElementById('pairing-done').classList.add('hidden');
    document.getElementById('pairing-active').classList.remove('hidden');
    tickPairingCountdown();
    if (_pairingPollTimer) clearInterval(_pairingPollTimer);
    _pairingPollTimer = setInterval(pollPairingStatus, 2000);
}

function tickPairingCountdown() {
    const el = document.getElementById('pairing-countdown');
    const secs = Math.max(0, Math.round((_pairingExpiresAt - Date.now()) / 1000));
    el.textContent = secs > 0 ? `Scade tra ${secs}s` : 'Scaduto — rigenera';
}

async function pollPairingStatus() {
    tickPairingCountdown();
    const r = await apiFetch('/admin/pileus/pairing/token');
    if (!r?.ok) return;
    const d = await r.json();
    if (d.used) {
        clearInterval(_pairingPollTimer); _pairingPollTimer = null;
        document.getElementById('pairing-active').classList.add('hidden');
        document.getElementById('pairing-done').classList.remove('hidden');
        showToast('Dispositivo Pileus accoppiato.', 'success');
        // Torna al pulsante "Genera" dopo qualche secondo, così è pronto per
        // il prossimo dispositivo senza ricaricare la pagina.
        setTimeout(() => {
            document.getElementById('pairing-done')?.classList.add('hidden');
            document.getElementById('pairing-idle')?.classList.remove('hidden');
        }, 6000);
    } else if (!d.active) {
        clearInterval(_pairingPollTimer); _pairingPollTimer = null;
        document.getElementById('pairing-active').classList.add('hidden');
        document.getElementById('pairing-idle').classList.remove('hidden');
    }
}

// ─── Dispositivi Pileus (revoca token) ───────────────────────────────────────
// escapeHtml: both device_id (a device can suggest any string when it first
// pairs — AuthorizeDevice doesn't format-check it) and label (a device can
// rename itself via the gRPC RenameDevice, or the admin here) are untrusted
// text rendered into the DOM below. Never build these rows with raw
// interpolation into innerHTML or an inline onclick(...) string.
function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

async function loadPileusDevices() {
    const box = document.getElementById('pileus-devices');
    if (!box) return;
    const r = await apiFetch('/admin/pileus/devices');
    if (!r?.ok) { box.textContent = 'Impossibile leggere i dispositivi.'; return; }
    const devs = (await r.json()).devices || [];
    if (!devs.length) { box.textContent = 'Nessun dispositivo accoppiato.'; return; }
    const fmt = s => s ? new Date(s * 1000).toLocaleString() : '—';
    box.innerHTML = devs.map(d => {
        const idAttr = escapeHtml(d.device_id);
        const name = escapeHtml(d.label || d.device_id);
        const revokeBtn = d.revoked
            ? `<button data-id="${idAttr}" data-action="unrevoke" class="pileus-dev-btn text-[11px] text-emerald-400 hover:text-emerald-300">Ripristina</button>`
            : `<button data-id="${idAttr}" data-action="revoke" class="pileus-dev-btn text-[11px] text-red-400 hover:text-red-300">Revoca</button>`;
        return `<div class="flex items-center gap-2 py-1.5 border-b border-gray-800 last:border-0">
            <div class="min-w-0">
              <div class="text-white truncate">${name}${d.revoked ? ' <span class="text-red-400 text-[10px]">(revocato)</span>' : ''}</div>
              <div class="text-[10px] text-gray-600">visto ${fmt(d.last_seen_at)}</div>
            </div>
            <span class="ml-auto flex items-center gap-2">
              <button data-id="${idAttr}" data-action="rename" class="pileus-dev-btn text-[11px] text-gray-400 hover:text-white" title="Rinomina">Rinomina</button>
              ${revokeBtn}
              <button data-id="${idAttr}" data-action="delete" class="pileus-dev-btn text-[11px] text-gray-500 hover:text-red-400" title="Dimentica del tutto">Elimina</button>
            </span>
        </div>`;
    }).join('');
}

// Un solo listener delegato sul box (sopravvive ai refresh via innerHTML) —
// niente onclick="fn('${id}')" con dati non fidati interpolati in una
// stringa JS. dataset.id arriva già decodificato dal browser.
document.getElementById('pileus-devices')?.addEventListener('click', e => {
    const btn = e.target.closest('.pileus-dev-btn');
    if (!btn) return;
    const id = btn.dataset.id;
    switch (btn.dataset.action) {
        case 'rename': renamePileusDevice(id); break;
        case 'revoke': setPileusDeviceRevoked(id, true); break;
        case 'unrevoke': setPileusDeviceRevoked(id, false); break;
        case 'delete': deletePileusDevice(id); break;
    }
});

async function renamePileusDevice(id) {
    const label = prompt('Nome per questo dispositivo (es. "TV salotto"):', '');
    if (label === null) return; // annullato
    const r = await apiFetch(`/admin/pileus/devices/${encodeURIComponent(id)}/rename`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ label }),
    });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Rinomina fallita.'), 'error'); return; }
    showToast('Dispositivo rinominato.', 'success');
    loadPileusDevices();
}

async function setPileusDeviceRevoked(id, revoke) {
    if (revoke && !confirm('Revocare questo dispositivo? Il player dovrà ri-abbinarsi con un nuovo codice.')) return;
    const r = await apiFetch(`/admin/pileus/devices/${encodeURIComponent(id)}/${revoke ? 'revoke' : 'unrevoke'}`, { method: 'POST' });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Operazione fallita.'), 'error'); return; }
    showToast(revoke ? 'Dispositivo revocato.' : 'Dispositivo ripristinato.', 'success');
    loadPileusDevices();
}

// Più netto della revoca: il device sparisce dal tutto dalla lista (utile se è
// rubato/dismesso, o solo per pulizia). I profili non sono toccati — sono
// condivisi tra tutti i dispositivi, non di proprietà di uno solo.
async function deletePileusDevice(id) {
    if (!confirm('Eliminare del tutto questo dispositivo? Sparirà dalla lista; i profili restano. Se torna a presentarsi con un codice valido, ricompare.')) return;
    const r = await apiFetch(`/admin/pileus/devices/${encodeURIComponent(id)}`, { method: 'DELETE' });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Eliminazione fallita.'), 'error'); return; }
    showToast('Dispositivo eliminato.', 'success');
    loadPileusDevices();
}

// ─── Tailscale ────────────────────────────────────────────────────────────────
async function openTailscaleModal() {
    document.getElementById('tailscale-modal').classList.remove('hidden');
    document.getElementById('ts-progress-list').innerHTML = '';
    document.getElementById('ts-auth-box').classList.add('hidden');
    const statusEl = document.getElementById('ts-modal-status');
    statusEl.textContent = 'Verifica in corso…';
    const res = await apiFetch('/admin/network/tailscale/status');
    if (!res?.ok) { statusEl.textContent = 'Impossibile leggere lo stato.'; return; }
    const d = await res.json();
    const ts = d.tailscale || {};
    statusEl.textContent = ts.connected
        ? `Connesso — IP ${ts.ip}${ts.hostname ? ' (' + ts.hostname + ')' : ''}`
        : 'Non connesso (o tailscaled non disponibile su questo deployment — vedi la nota nella card).';
}

async function startTailscaleSetup() {
    const key = document.getElementById('ts-authkey-input').value.trim();
    const btn = document.getElementById('ts-configure-btn');
    btn.disabled = true; btn.textContent = 'Configurazione in corso…';
    document.getElementById('ts-progress-list').innerHTML = '';
    document.getElementById('ts-auth-box').classList.add('hidden');

    const res = await apiFetch('/admin/network/tailscale', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ tailscale_auth_key: key }),
    });
    if (!res?.ok || !res.body) {
        btn.disabled = false; btn.textContent = 'Configura / riconnetti';
        showToast('Avvio configurazione Tailscale fallito.', 'error');
        return;
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        const lines = buf.split('\n');
        buf = lines.pop();
        for (const line of lines) {
            if (!line.startsWith('data: ')) continue;
            try { handleTsProgress(JSON.parse(line.slice(6))); } catch {}
        }
    }
    btn.disabled = false; btn.textContent = 'Configura / riconnetti';
}

function handleTsProgress(ev) {
    if (ev.step === 'tailscale_auth') {
        const box = document.getElementById('ts-auth-box');
        const link = document.getElementById('ts-auth-url');
        link.href = ev.message; link.textContent = ev.message;
        box.classList.remove('hidden');
        return;
    }
    const list = document.getElementById('ts-progress-list');
    let row = document.getElementById('ts-prog-' + ev.step);
    if (!row) {
        row = document.createElement('div');
        row.id = 'ts-prog-' + ev.step;
        row.className = 'flex items-center gap-2 text-xs';
        list.appendChild(row);
    }
    const icon = ev.error ? 'Errore' : ev.done ? 'OK' : '…';
    const color = ev.error ? 'text-red-400' : ev.done ? 'text-emerald-400' : 'text-gray-400';
    row.innerHTML = `<span class="${color} font-mono text-[10px] w-10 shrink-0">${icon}</span><span class="${color}">${esc(ev.error || ev.message)}</span>`;
    if (ev.step === 'done' && !ev.error) showToast('Tailscale configurato.', 'success');
}

function updatePluginRes(resources) {
    for (const [pid, s] of Object.entries(resources)) {
        const el = document.getElementById('plugin-res-'+pid.replace(/\./g,'-'));
        if (!el) continue;
        el.textContent = `PID ${s.pid} · ${fmtBytes(s.mem_bytes)} · CPU ${(s.cpu_percent??0).toFixed(1)}%`;
    }
}

// ─── Upload ───────────────────────────────────────────────────────────────────
const luaInput = document.getElementById('lua_plugin_input');
if (luaInput) {
    luaInput.addEventListener('change', e => {
        const has = e.target.files.length > 0;
        document.getElementById('lua_plugin_label').textContent = has ? e.target.files[0].name : 'Scegli plugin .zip …';
        document.getElementById('lua_upload_btn').disabled = !has;
    });
}
async function uploadLuaPlugin() {
    const file = luaInput?.files[0];
    if (!file || _installing) return;
    const btn = document.getElementById('lua_upload_btn'), label = document.getElementById('lua_plugin_label');
    setInstalling(true); btn.textContent = 'Installazione…'; btn.disabled = true;
    const fd = new FormData(); fd.append('plugin_file', file);
    try {
        const r = await apiFetch('/admin/lua-plugins/upload',{method:'POST',body:fd});
        if (!r) return;
        const d = await r.json();
        showToast(r.ok?(d.message||'Plugin installato.'): `Errore: ${d.message||'fallito'}`, r.ok?'success':'error');
        if (r.ok) setTimeout(loadPlugins, 600);
    } catch { showToast('Errore di rete.','error'); }
    finally {
        setInstalling(false); btn.textContent='Installa'; btn.disabled=false;
        if (luaInput) luaInput.value=''; label.textContent='Scegli plugin .zip …';
    }
}

// ─── Stremio modal ────────────────────────────────────────────────────────────
async function getClients() {
    if (_cachedClients !== null) return _cachedClients;
    const r = await apiFetch('/admin/clients');
    _cachedClients = r?.ok ? (await r.json()||[]) : [];
    return _cachedClients;
}
async function showPluginStremioURLs(pid, name) {
    const clients = await getClients();
    const base = `${location.protocol}//${location.host}/addon`;
    const rows = clients.length === 0
        ? buildURLRow('Accesso libero', `${base}/default/${pid}/manifest.json`)
        : clients.map(c => buildURLRow(c.client_id, `${base}/${c.stremio_token}/${pid}/manifest.json`)).join('');
    document.getElementById('stremio-url-modal-body').innerHTML =
        `<p class="text-xs text-gray-500 mb-3">URL per <span class="text-white font-semibold">${esc(name)}</span></p><div class="space-y-2">${rows}</div>`;
    document.getElementById('stremio-url-modal').classList.remove('hidden');
}
function buildURLRow(label, url) {
    const safe = esc(url), jUrl = esc(JSON.stringify(url));
    const jStremio = esc(JSON.stringify('stremio://'+url.replace(/^https?:\/\//,'')));
    return `<div class="bg-gray-900/60 rounded-xl border border-gray-800 p-3">
        <div class="flex items-center justify-between mb-1.5">
            <span class="text-[10px] font-bold text-gray-500 uppercase tracking-wide">${esc(label)}</span>
            <div class="flex gap-1.5">
                <button onclick="navigator.clipboard.writeText(${jUrl}).then(()=>showToast('Copiato!','info'))"
                    class="bg-gray-800 hover:bg-gray-700 text-gray-300 text-xs px-2.5 py-1 rounded-lg transition">Copia</button>
                <button onclick="window.location.href=${jStremio}"
                    class="bg-indigo-800 hover:bg-indigo-700 text-white text-xs px-2.5 py-1 rounded-lg transition">Apri</button>
            </div>
        </div>
        <input readonly value="${safe}" onclick="this.select()"
            class="w-full bg-black/40 text-blue-300 text-xs font-mono px-2.5 py-1.5 rounded-lg border border-gray-800 focus:outline-none cursor-text">
    </div>`;
}

// ─── Plugin list ──────────────────────────────────────────────────────────────
async function loadPlugins() {
    _cachedClients = null;
    await loadEgress(); // popola _egressProfiles per la <select> "Uscita" delle card
    const [infoR, bindR] = await Promise.all([
        apiFetch('/admin/plugins/info'),
        apiFetch('/admin/enricher-bindings'),
    ]);
    if (!infoR?.ok) return;
    const schemas = await infoR.json();
    const bindData = bindR?.ok ? await bindR.json() : { bindings:{}, providers:[] };
    const enricherBindings = bindData.bindings || {};
    const availProviders   = bindData.providers || [];

    const cont = document.getElementById('plugins-container');
    cont.innerHTML = '';
    if (!Object.keys(schemas).length) {
        cont.innerHTML = `<p class="text-gray-600 text-sm text-center py-8">Nessun plugin installato.</p>`;
        return;
    }

    const lua = {}, providers = {}, enrichers = {};
    for (const [pid, d] of Object.entries(schemas)) {
        if (d.plugin_type === 'lua') lua[pid] = d;
        else if (d.plugin_type === 'enricher') enrichers[pid] = d;
        else providers[pid] = d;
    }

    const secHdr = (label, color) => {
        const d = document.createElement('div');
        d.className = 'flex items-center gap-3 pt-2 pb-1';
        d.innerHTML = `<span class="text-[10px] font-bold uppercase tracking-widest ${color}">${label}</span><div class="flex-1 border-t border-gray-800/80"></div>`;
        return d;
    };

    if (Object.keys(lua).length)       { cont.appendChild(secHdr('Plugin Lua','text-sky-500')); for (const [p,d] of Object.entries(lua))       cont.appendChild(buildCard(p,d,false,enricherBindings,availProviders)); }
    if (Object.keys(providers).length) { cont.appendChild(secHdr('Provider','text-blue-500'));    for (const [p,d] of Object.entries(providers)) cont.appendChild(buildCard(p,d,false,enricherBindings,availProviders)); }
    if (Object.keys(enrichers).length) { cont.appendChild(secHdr('Enricher','text-purple-500'));  for (const [p,d] of Object.entries(enrichers)) cont.appendChild(buildCard(p,d,true, enricherBindings,availProviders)); }
}

// ─── Card builder ─────────────────────────────────────────────────────────────
function buildCard(pid, data, isEnricher, enricherBindings, availProviders) {
    const isLua     = data.plugin_type === 'lua';
    const isEnabled = data.is_active !== false;
    const isReady   = data.ready === true;
    const isProcess = data.process === true;
    const cfgStatus = data.status || 'not_configured';
    const fields    = data.fields || [];
    const tasks     = data.tasks  || [];

    // Runtime status
    const rs     = data.runtime_status || {};
    const rLabel = rs.label  || (isReady ? 'ready' : isProcess ? 'running' : 'stopped');
    const rDetail= rs.detail || '';

    const dotCls = { ready:'bg-emerald-400 shadow shadow-emerald-400/40',
                     syncing:'bg-yellow-400 shadow shadow-yellow-400/40 pulse-dot',
                     needs_config:'bg-amber-500', error:'bg-red-500' }[rLabel]
                || (isReady?'bg-emerald-400 shadow shadow-emerald-400/40': isProcess?'bg-yellow-400':'bg-gray-600');
    // Il dot porta lo stato (colore) nel titolo al passaggio del mouse — niente
    // etichetta di testo sotto, badge o riga di dettaglio nell'intestazione:
    // ridondanti col colore stesso, tolti per tenere la card compatta.
    const dotTitle = rDetail ? `${rLabel}: ${rDetail}` : rLabel;

    const borderCls = !isEnabled ? 'border-gray-800/60'
        : cfgStatus === 'env_error' ? 'border-red-900/60'
        : isLua ? 'border-sky-900/60'
        : isEnricher ? 'border-purple-900/60' : 'border-gray-800';


    // ── Panel content builders ────────────────────────────────────────────────

    // LOG panel
    const logEndpoint    = isLua ? `/admin/lua-plugins/logs?plugin_id=${encodeURIComponent(pid)}`
                                 : `/admin/plugins/logs?plugin_id=${encodeURIComponent(pid)}`;
    const logSSEEndpoint = isLua ? `/admin/lua-plugins/logs/stream?plugin_id=${encodeURIComponent(pid)}`
                                 : `/admin/plugins/logs/stream?plugin_id=${encodeURIComponent(pid)}`;

    const logPanelHTML = `
        <div class="space-y-2">
            <div class="flex items-center justify-between">
                <span class="flex items-center gap-1.5 text-xs text-gray-500">
                    Live <span id="log-dot-${esc(pid.replace(/\./g,'-'))}" class="inline-block w-1.5 h-1.5 rounded-full bg-gray-700"></span>
                </span>
                <div class="flex gap-2">
                    <button data-action="clear-log" class="text-[10px] text-gray-600 hover:text-gray-400 transition">Pulisci</button>
                    <button data-action="refresh-log" class="text-[10px] text-gray-600 hover:text-gray-400 transition">Ricarica</button>
                </div>
            </div>
            <pre id="plugin-log-output-${esc(pid)}"
                class="panel rounded-lg p-3 text-xs font-mono h-48 overflow-y-auto whitespace-pre-wrap text-gray-500 border border-gray-800/60 leading-relaxed">In attesa di log…</pre>
        </div>`;

    // SETTINGS panel
    const settingsPanelHTML = fields.length > 0 ? `
        <div class="space-y-3">
            ${fields.map(f => {
                const fkey   = f.key || f.id  || '';
                const flabel = f.label        || fkey;
                const ftype  = f.type         || 'text';
                const fval   = f.current_value|| '';
                const isPass = ftype === 'password';
                const isBool = ftype === 'bool';
                const isSet  = isPass && (f.is_set || false);
                if (isBool) {
                    return `<div class="flex items-center justify-between gap-2 py-1">
                        <label class="text-[11px] text-gray-400 flex items-center gap-1.5">
                            ${esc(flabel)}
                        </label>
                        <input type="checkbox"
                            id="pluginfield_${esc(pid)}_${fkey}"
                            data-bool="1"
                            ${fval === 'true' ? 'checked' : ''}
                            class="w-4 h-4 accent-blue-600 disabled:opacity-30 disabled:cursor-not-allowed">
                    </div>`;
                }
                return `<div>
                    <label class="block text-[11px] text-gray-400 mb-1 flex items-center gap-1.5">
                        ${esc(flabel)}
                        ${f.required ? '<span class="text-red-500/70 text-[9px]">*</span>' : ''}
                    </label>
                    <input type="${isPass?'password':ftype==='number'?'number':'text'}"
                        id="pluginfield_${esc(pid)}_${fkey}"
                        value="${isPass?'':esc(fval)}"
                        placeholder="${isPass&&isSet?'••••••••  (già impostato)':esc(f.placeholder||f.Placeholder||'')}"
                        class="w-full panel border border-gray-800 rounded-lg px-3 py-2 text-xs text-gray-200 font-mono
                               focus:border-blue-600 focus:outline-none disabled:opacity-30 disabled:cursor-not-allowed transition">
                </div>`;
            }).join('')}
            <button data-action="save-settings"
                class="bg-blue-700 hover:bg-blue-600 text-white text-xs px-4 py-1.5 rounded-lg font-semibold transition">
                Salva${isLua?'':' e riavvia'}
            </button>
        </div>` : `<p class="text-xs text-gray-600">Nessuna impostazione configurabile.</p>`;

    // TASKS panel (Lua only)
    const tasksPanelHTML = tasks.length > 0 ? `
        <div class="space-y-2">
            ${tasks.map(t => `
            <div class="flex items-center justify-between panel border border-gray-800/60 rounded-lg px-3 py-2">
                <div>
                    <span class="text-xs font-mono text-gray-300">${esc(t.function||t.Function)}</span>
                    ${t.cron ? `<span class="ml-2 text-[10px] font-mono text-gray-600">${esc(t.cron)}</span>` : ''}
                </div>
                <button data-action="run-task" data-task="${esc(t.function||t.Function)}"
                    class="bg-gray-800 hover:bg-sky-900/60 text-gray-400 hover:text-sky-300 text-[10px] px-2.5 py-1 rounded-lg transition font-mono">
                    Esegui
                </button>
            </div>`).join('')}
        </div>` : `<p class="text-xs text-gray-600">Nessun task manuale disponibile.</p>`;

    // BINDINGS panel (enricher)
    const currentBound = enricherBindings[pid] || [];
    const bindingsPanelHTML = isEnricher ? `
        <div class="space-y-2">
            <p class="text-[11px] text-gray-500 mb-2">Provider collegati a questo enricher:</p>
            ${availProviders.length === 0 ? `<p class="text-xs text-gray-600">Nessun provider attivo.</p>` :
              availProviders.map(provId => `
            <label class="flex items-center gap-2 cursor-pointer">
                <input type="checkbox" id="bind_${esc(pid)}_${provId.replace(/\./g,'__')}" ${currentBound.includes(provId)?'checked':''}
                    class="rounded accent-purple-500">
                <span class="text-xs font-mono text-gray-300">${esc(provId)}</span>
            </label>`).join('')}
            <button data-action="save-binding"
                class="mt-1 bg-purple-700 hover:bg-purple-600 text-white text-xs px-4 py-1.5 rounded-lg font-semibold transition">
                Salva
            </button>
        </div>` : '';

    // CATALOGS panel
    const caps = data.capabilities || [];
    const hasCatalogs = !isEnricher && (caps.includes('static_catalog') || caps.includes('sections'));
    const catalogsPanelHTML = hasCatalogs ? `
        <div>
            <div id="catalogs-list-${esc(pid)}" class="space-y-1.5 mb-3"><p class="text-xs text-gray-600">Clicca per caricare…</p></div>
            <button data-action="save-catalogs"
                class="bg-teal-700 hover:bg-teal-600 text-white text-xs px-4 py-1.5 rounded-lg font-semibold transition">
                Salva ordine
            </button>
        </div>` : '';

    // EGRESS panel (Lua only) — da quale uscita di rete passa il flusso video
    const curEgress = data.egress || 'direct';
    const egressPanelHTML = !isLua ? '' : `
        <div class="space-y-2">
            <p class="text-[11px] text-gray-500 mb-2">Uscita di rete per il flusso video di questo plugin. Le uscite si gestiscono in <b>VPN → Uscite di rete</b>.</p>
            <select id="pluginegress_${esc(pid)}"
                class="w-full panel border border-gray-800 rounded-lg px-3 py-2 text-xs text-gray-200
                       focus:border-blue-600 focus:outline-none transition">
                ${_egressProfiles.map(p => `<option value="${esc(p.name)}" ${p.name===curEgress?'selected':''} ${(!p.enabled&&p.name!=='direct')?'disabled':''}>${esc(_egressLabel(p.name))}${(!p.enabled&&p.name!=='direct')?' (disattivata)':''}</option>`).join('')}
            </select>
            <button data-action="save-egress"
                class="bg-blue-700 hover:bg-blue-600 text-white text-xs px-4 py-1.5 rounded-lg font-semibold transition">Salva</button>
        </div>`;

    // ── Determine available panel tabs ────────────────────────────────────────
    const panelTabs = [
        { id:'log',      label:'Log',         html: logPanelHTML,      always: true },
        { id:'settings', label:'Impostazioni', html: settingsPanelHTML, always: true },
        ...(tasks.length > 0            ? [{ id:'tasks',    label:'Task',    html: tasksPanelHTML    }] : []),
        ...(isLua                      ? [{ id:'egress',   label:'Uscita',  html: egressPanelHTML   }] : []),
        ...(isEnricher                  ? [{ id:'bindings', label:'Binding', html: bindingsPanelHTML }] : []),
        ...(hasCatalogs                 ? [{ id:'catalogs', label:'Cataloghi',html: catalogsPanelHTML, onOpen: () => loadCatalogsPanel(pid) }] : []),
    ];

    // ── Assemble card ─────────────────────────────────────────────────────────
    const card = document.createElement('div');
    card.id = `card-${pid.replace(/\./g,'-')}`;
    // A stopped/disabled plugin: dim ONLY the header (name/status), never the
    // drawer — its settings must stay perfectly readable so you can configure
    // it before turning it on. (isOff computed after rState, below.)
    card.className = `card border ${borderCls} rounded-xl overflow-hidden transition-opacity`;

    const resId     = 'plugin-res-'     + esc(pid.replace(/\./g,'-'));

    const stremioBtn = (!isEnricher && isReady && !isLua)
        ? `<button data-action="stremio-urls"
               class="text-[10px] text-indigo-400 hover:text-indigo-300 font-mono transition">URL Stremio</button>` : '';

    const editBtn = isLua
        ? `<button data-action="open-editor"
               class="text-[10px] text-gray-500 hover:text-gray-300 transition">Codice</button>` : '';

    // Toggle abilita/disabilita: solo per i plugin non-Lua. I plugin Lua
    // usano il ciclo di vita a stati (luaOps, sotto).
    const toggleBtn = isLua ? '' : `<button data-action="toggle-status"
               class="text-[10px] ${isEnabled?'text-gray-600 hover:text-gray-400':'text-emerald-600 hover:text-emerald-400'} transition">
               ${isEnabled?'Disabilita':'Abilita'}</button>`;
    const uninstallBtn = `<button data-action="uninstall"
               class="text-[10px] text-gray-700 hover:text-red-400 transition" title="Disinstalla">Elimina</button>`;

    const gRPCOps = isLua ? '' : isProcess
        ? `<button data-action="stop"    class="text-[10px] text-gray-600 hover:text-red-400 transition">Stop</button>
           <button data-action="restart" class="text-[10px] text-gray-600 hover:text-yellow-400 transition">Riavvia</button>`
        : `<button data-action="start" class="text-[10px] text-gray-600 hover:text-emerald-400 transition">Avvia</button>`;

    // Lua lifecycle: un solo tasto Avvia/Ferma che cambia stato, + Riavvia
    // (disabilitato se il plugin è fermo o in attesa di configurazione).
    const rState = data.run_state || (isEnabled ? 'running' : 'stopped');
    const isOff = isLua ? (rState === 'stopped') : !isEnabled;
    const luaOps = !isLua ? '' : (() => {
        const startStop = rState === 'running'
            ? `<button data-action="lua-stop"
                   class="text-[10px] text-gray-600 hover:text-red-400 transition">Ferma</button>`
            : `<button ${rState==='waiting'?'disabled title="Compila i campi obbligatori nelle Impostazioni"':'data-action="lua-start"'}
                   class="text-[10px] ${rState==='waiting'?'text-gray-700 cursor-not-allowed':'text-emerald-600 hover:text-emerald-400'} transition">Avvia</button>`;
        const restart = `<button ${rState==='running'?'data-action="lua-restart"':'disabled'}
                   class="text-[10px] ${rState==='running'?'text-gray-600 hover:text-yellow-400':'text-gray-800 cursor-not-allowed'} transition">Riavvia</button>`;
        return startStop + restart;
    })();

    // Register tab onOpen callbacks in the registry (avoids eval).
    panelTabs.forEach(t => {
        if (t.onOpen) _tabOnOpen[`${pid}:${t.id}`] = t.onOpen;
    });

    // Build tab bar HTML
    const tabBarHTML = panelTabs.map((t, i) => `
        <button id="ptab-${esc(pid)}-${t.id}" data-action="switch-tab" data-tab="${esc(t.id)}"
            class="px-3 py-1.5 text-[11px] font-semibold transition ${i===0?'text-blue-400 border-b border-blue-500':'text-gray-600 hover:text-gray-400'}">
            ${t.label}
        </button>`).join('');

    const panelsHTML = panelTabs.map((t, i) => `
        <div id="ppanel-${esc(pid)}-${t.id}" class="${i>0?'hidden':''}" ${t.id==='log'?`data-is-lua="${isLua}"`:''}>
            ${t.html}
        </div>`).join('');

    card.innerHTML = `
        <!-- Header row -->
        <div class="flex items-start gap-3 px-4 py-3 flex-wrap">
            <!-- Status dot: colore = stato, dettaglio nel title al passaggio del mouse -->
            <div class="pt-1.5 shrink-0">
                <span class="inline-block w-2 h-2 rounded-full ${dotCls}" title="${esc(dotTitle)}"></span>
            </div>
            <!-- Plugin info -->
            <div class="flex-1 min-w-0 ${isOff ? 'opacity-55' : ''}">
                <span class="text-sm font-bold text-white">${esc(data.plugin_name||pid)}</span>
                ${isOff ? '<span class="ml-2 text-[9px] uppercase tracking-wide text-gray-500 border border-gray-700 rounded px-1.5 py-0.5 align-middle">disabilitato</span>' : ''}
                <p class="text-[10px] text-gray-600 font-mono mt-0.5">${esc(pid)}</p>
                ${isLua && rState === 'waiting' ? `<p class="text-[10px] text-amber-500 mt-0.5">configurazione richiesta</p>` : ''}
                ${isLua && rState === 'stopped' ? `<p class="text-[10px] text-gray-500 mt-0.5">fermo</p>` : ''}
                <!-- PID/RAM/CPU: solo per i plugin a processo separato (gRPC) — i
                     plugin Lua girano nel processo principale, non hanno risorse
                     proprie da mostrare. Popolato da updatePluginRes(). -->
                ${isProcess && !isLua ? `<p id="${resId}" class="text-[10px] font-mono mt-0.5 text-gray-600">—</p>` : ''}
            </div>
            <!-- Actions -->
            <div class="flex items-center gap-x-2.5 gap-y-1 shrink-0 flex-wrap justify-end ml-auto">
                ${gRPCOps}${luaOps}${stremioBtn}${editBtn}${toggleBtn}${uninstallBtn}
            </div>
        </div>
        <!-- Expandable drawer -->
        <div id="drawer-${esc(pid)}" class="hidden border-t border-gray-800/60">
            <!-- Inner tab bar -->
            <div class="flex items-center gap-0 px-2 border-b border-gray-800/40 bg-black/20">
                ${tabBarHTML}
            </div>
            <!-- Panel content -->
            <div class="p-4">
                ${panelsHTML}
            </div>
        </div>`;

    // Toggle drawer on header click (but not on buttons)
    card.querySelector('.flex.items-start').addEventListener('click', e => {
        if (e.target.closest('button')) return;
        const drawer = document.getElementById(`drawer-${pid}`);
        if (!drawer) return;
        const opening = drawer.classList.contains('hidden');
        drawer.classList.toggle('hidden');
        if (opening) {
            // Auto-open log tab and connect SSE
            openPluginLog(pid, logEndpoint, logSSEEndpoint);
        } else {
            disconnectPluginLogSSE(pid);
        }
    });

    // Un solo listener delegato per TUTTI i bottoni della card (header +
    // pannelli del drawer) — niente onclick="fn('${pid}')" costruito per
    // concatenazione: pid è mf.ID del manifest plugin, mai sanificato lato
    // Go, quindi potenzialmente ostile. pid/isLua/isEnabled/logEndpoint/data
    // restano disponibili per chiusura, non serve portarli in data-*.
    card.addEventListener('click', e => {
        const btn = e.target.closest('[data-action]');
        if (!btn || !card.contains(btn)) return;
        switch (btn.dataset.action) {
            case 'clear-log':    clearPluginLog(pid); break;
            case 'refresh-log':  refreshPluginLogs(pid, logEndpoint); break;
            case 'save-settings':  savePluginSettings(pid, isLua, btn); break;
            case 'run-task':       runLuaTask(pid, btn.dataset.task); break;
            case 'save-binding':   saveEnricherBinding(pid, btn); break;
            case 'save-catalogs':  saveCatalogs(pid, btn); break;
            case 'save-egress':    savePluginEgress(pid, btn); break;
            case 'switch-tab':     switchPluginTab(pid, btn.dataset.tab); break;
            case 'stremio-urls':   showPluginStremioURLs(pid, data.plugin_name || pid); break;
            case 'open-editor':    openEditor(pid, data.plugin_name || pid); break;
            case 'toggle-status':  if (!_installing) togglePluginStatus(pid, !isEnabled); break;
            case 'uninstall':      if (!_installing) doUninstallPlugin(pid, isLua); break;
            case 'stop':           if (!_installing) doStopPlugin(pid); break;
            case 'restart':        if (!_installing) doRestartPlugin(pid); break;
            case 'start':          if (!_installing) doRestartPlugin(pid, true); break;
            case 'lua-start':      if (!_installing) luaPluginState(pid, 'start'); break;
            case 'lua-stop':       if (!_installing) luaPluginState(pid, 'stop'); break;
            case 'lua-restart':    if (!_installing) luaPluginState(pid, 'restart'); break;
        }
    });

    return card;
}

// ─── Plugin tab switch ────────────────────────────────────────────────────────
function switchPluginTab(pid, tabId) {
    // Hide all panels
    document.querySelectorAll(`[id^="ppanel-${pid}-"]`).forEach(el => el.classList.add('hidden'));
    // Reset all tab buttons
    document.querySelectorAll(`[id^="ptab-${pid}-"]`).forEach(btn => {
        btn.className = 'px-3 py-1.5 text-[11px] font-semibold transition text-gray-600 hover:text-gray-400';
    });
    // Show target
    document.getElementById(`ppanel-${pid}-${tabId}`)?.classList.remove('hidden');
    const activeBtn = document.getElementById(`ptab-${pid}-${tabId}`);
    if (activeBtn) activeBtn.className = 'px-3 py-1.5 text-[11px] font-semibold transition text-blue-400 border-b border-blue-500';
    // SSE: connect on log, disconnect otherwise
    if (tabId === 'log') {
        const logEndpoint    = `/admin/${document.getElementById(`ppanel-${pid}-log`)?.dataset.isLua==='true' ? 'lua-plugins' : 'plugins'}/logs?plugin_id=${encodeURIComponent(pid)}`;
        const logSSEEndpoint = logEndpoint.replace('/logs?', '/logs/stream?');
        openPluginLog(pid, logEndpoint, logSSEEndpoint);
    } else {
        disconnectPluginLogSSE(pid);
    }
    // Fire onOpen callback from registry (no eval).
    const cb = _tabOnOpen[`${pid}:${tabId}`];
    if (cb) cb();
}

// ─── Plugin log ───────────────────────────────────────────────────────────────
function openPluginLog(pid, logUrl, sseUrl) {
    refreshPluginLogs(pid, logUrl);
    connectPluginLogSSE(pid, sseUrl);
}
function clearPluginLog(pid) {
    const el = document.getElementById(`plugin-log-output-${pid}`);
    if (el) el.innerHTML = '';
}
async function refreshPluginLogs(pid, logUrl) {
    const out = document.getElementById(`plugin-log-output-${pid}`);
    if (!out) return;
    const r = await apiFetch(logUrl);
    if (!r?.ok) { out.textContent = 'Errore caricamento log.'; return; }
    const { logs=[] } = await r.json();
    if (!logs.length) { out.textContent = 'Nessun log disponibile.'; return; }
    renderLogLines(out, logs);
}
function renderLogLines(el, lines) {
    el.innerHTML = lines.map(l => `<span class="log-line ${logColor(l)}">${esc(l)}</span>`).join('\n');
    el.scrollTop = el.scrollHeight;
}
function connectPluginLogSSE(pid, sseUrl) {
    disconnectPluginLogSSE(pid);
    const out = document.getElementById(`plugin-log-output-${pid}`);
    const dot = document.getElementById(`log-dot-${pid.replace(/\./g,'-')}`);
    if (!out) return;
    const es = new EventSource(sseUrl);
    _pluginLogSSE[pid] = es;
    es.onopen = () => {
        if (dot) dot.className = 'inline-block w-1.5 h-1.5 rounded-full bg-emerald-400 shadow shadow-emerald-400/50';
    };
    es.onmessage = e => {
        const line = e.data;
        if (!line || line.startsWith(':')) return;
        if (out.textContent === 'In attesa di log…' || out.textContent === 'Nessun log disponibile.') out.innerHTML = '';
        if (out.childNodes.length > 0) out.appendChild(document.createTextNode('\n'));
        const span = document.createElement('span');
        span.className = `log-line ${logColor(line)}`;
        span.textContent = line;
        out.appendChild(span);
        while (out.childElementCount > 600) out.removeChild(out.firstChild);
        out.scrollTop = out.scrollHeight;
    };
    es.onerror = () => {
        if (dot) dot.className = 'inline-block w-1.5 h-1.5 rounded-full bg-red-500';
        es.close(); delete _pluginLogSSE[pid];
        setTimeout(() => {
            const drawer = document.getElementById(`drawer-${pid}`);
            const panel  = document.getElementById(`ppanel-${pid}-log`);
            if (drawer && !drawer.classList.contains('hidden') && panel && !panel.classList.contains('hidden')) {
                connectPluginLogSSE(pid, sseUrl);
            }
        }, 4000);
    };
}
function disconnectPluginLogSSE(pid) {
    if (_pluginLogSSE[pid]) { _pluginLogSSE[pid].close(); delete _pluginLogSSE[pid]; }
    const dot = document.getElementById(`log-dot-${pid.replace(/\./g,'-')}`);
    if (dot) dot.className = 'inline-block w-1.5 h-1.5 rounded-full bg-gray-700';
}

// ─── Lua task ─────────────────────────────────────────────────────────────────
async function runLuaTask(pid, task) {
    const r = await apiFetch(`/admin/lua-plugins/run-task/${encodeURIComponent(pid)}/${encodeURIComponent(task)}`,{method:'POST'});
    if (r?.ok) { showToast(`Task "${task}" avviato.`, 'info'); return; }
    // 409 (bgTaskSem occupato) porta un messaggio specifico nel body — mostralo
    // invece del generico "Errore avvio task.", così l'utente sa che non è
    // fallito ma solo in coda dietro un altro task.
    let msg = 'Errore avvio task.';
    try { const body = await r.json(); if (body?.message) msg = body.message; } catch {}
    showToast(msg, 'warn');
}

// ─── Catalogs ─────────────────────────────────────────────────────────────────
async function loadCatalogsPanel(pid) {
    const list = document.getElementById(`catalogs-list-${pid}`);
    if (!list || list.dataset.loaded === pid) return;
    list.innerHTML = '<p class="text-xs text-gray-600">Caricamento…</p>';
    const r = await apiFetch(`/admin/plugins/catalogs?plugin_id=${encodeURIComponent(pid)}`);
    if (!r?.ok) { list.innerHTML = '<p class="text-xs text-red-500">Errore.</p>'; return; }
    const { catalogs=[] } = await r.json();
    if (!catalogs.length) { list.innerHTML = '<p class="text-xs text-gray-600">Nessun catalogo.</p>'; return; }
    catalogs.sort((a,b) => a.order-b.order);
    list.innerHTML = '';
    catalogs.forEach(c => {
        const row = document.createElement('div');
        row.className = 'catalog-row flex items-center gap-2 panel border border-gray-800/60 rounded-lg px-3 py-1.5';
        row.dataset.id = c.id;
        row.innerHTML = `
            <div class="flex flex-col gap-0"><button onclick="moveCatalogRow(this,-1)" class="text-gray-700 hover:text-white text-[9px] leading-none">▲</button>
            <button onclick="moveCatalogRow(this,1)" class="text-gray-700 hover:text-white text-[9px] leading-none">▼</button></div>
            <label class="flex items-center gap-2 flex-1 cursor-pointer min-w-0">
                <input type="checkbox" class="catalog-toggle accent-teal-500 shrink-0" ${c.enabled?'checked':''}>
                <span class="text-xs text-gray-300 font-medium truncate">${esc(c.name)}</span>
                <span class="text-[10px] text-gray-600 font-mono shrink-0">${esc(c.id)}</span>
            </label>`;
        list.appendChild(row);
    });
    list.dataset.loaded = pid;
}
function moveCatalogRow(btn, dir) {
    const row = btn.closest('.catalog-row'), list = row.parentElement;
    if (dir===-1 && row.previousElementSibling) list.insertBefore(row, row.previousElementSibling);
    else if (dir===1 && row.nextElementSibling) list.insertBefore(row.nextElementSibling, row);
}
async function saveCatalogs(pid, btn) {
    const list = document.getElementById(`catalogs-list-${pid}`);
    if (!list) return;
    const enabled=[], order=[];
    list.querySelectorAll('.catalog-row').forEach(r => {
        order.push(r.dataset.id);
        if (r.querySelector('.catalog-toggle').checked) enabled.push(r.dataset.id);
    });
    if (btn) btn.disabled = true;
    try {
        const r = await apiFetch('/admin/plugins/catalogs',{method:'PATCH',headers:{'Content-Type':'application/json'},body:JSON.stringify({plugin_id:pid,enabled,order})});
        if (r?.ok) { showToast('Cataloghi salvati.','success'); list.dataset.loaded=''; }
        else showToast(await apiErrorMessage(r, 'Errore salvataggio cataloghi.'), 'error');
    } finally {
        if (btn) btn.disabled = false;
    }
}

// ─── Settings / binding ───────────────────────────────────────────────────────
async function saveEnricherBinding(eid, btn) {
    const bound = [];
    document.querySelectorAll(`[id^="bind_${eid}_"]`).forEach(cb => {
        if (cb.checked) bound.push(cb.id.slice(`bind_${eid}_`.length).replace(/__/g,'.'));
    });
    if (btn) btn.disabled = true;
    try {
        const r1 = await apiFetch('/admin/enricher-bindings');
        if (!r1?.ok) { showToast(await apiErrorMessage(r1, 'Errore lettura binding.'), 'error'); return; }
        const { bindings={} } = await r1.json(); bindings[eid] = bound;
        const r = await apiFetch('/admin/settings/save',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({enricher_bindings:JSON.stringify(bindings)})});
        showToast(r?.ok?'Collegamento salvato.':await apiErrorMessage(r, 'Errore salvataggio.'), r?.ok?'success':'error');
    } finally {
        if (btn) btn.disabled = false;
    }
}
async function savePluginSettings(pid, isLua, btn) {
    const payload = {};
    document.querySelectorAll(`[id^="pluginfield_${pid}_"]`).forEach(el => {
        if (el.disabled) return;
        const key = el.id.replace(`pluginfield_${pid}_`,'');
        if (el.dataset.bool === '1') {
            // Sempre incluso (anche "false"): a differenza dei campi testo, uno
            // switch disattivato è un valore esplicito, non "non ho scelto".
            payload[key] = el.checked ? 'true' : 'false';
        } else if (el.value.trim()) {
            payload[key] = el.value.trim();
        }
    });
    if (!Object.keys(payload).length) { showToast('Nessun valore da salvare.','warn'); return; }
    const url = isLua ? `/admin/lua-plugins/settings/${encodeURIComponent(pid)}` : '/admin/settings/save';
    if (btn) btn.disabled = true;
    try {
        const r = await apiFetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload)});
        if (r?.ok) { showToast('Impostazioni salvate.','success'); if (!isLua) await doRestartPlugin(pid, true); }
        else showToast(await apiErrorMessage(r, 'Errore salvataggio.'), 'error');
    } finally {
        if (btn) btn.disabled = false;
    }
}

async function savePluginEgress(pid, btn) {
    const sel = document.getElementById(`pluginegress_${pid}`);
    if (!sel) return;
    if (btn) btn.disabled = true;
    try {
        const r = await apiFetch(`/admin/lua-plugins/egress/${encodeURIComponent(pid)}`, {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ egress: sel.value }),
        });
        if (r?.ok) {
            const { egress } = await r.json();
            showToast(`Uscita di "${pid}": ${_egressLabel(egress)}.`, 'success');
        } else showToast(await apiErrorMessage(r, 'Errore salvataggio uscita.'), 'error');
    } finally {
        if (btn) btn.disabled = false;
    }
}

// ─── gRPC plugin ops ──────────────────────────────────────────────────────────
async function doStopPlugin(pid) {
    // Su un media server uno stop può interrompere uno stream in corso per
    // chi lo sta guardando in quel momento — stessa cautela già usata per
    // Uninstall, prima mancava qui.
    if (!confirm(`Fermare "${pid}"? Se qualcuno lo sta usando per uno stream, si interrompe.`)) return;
    const r = await apiFetch('/admin/plugins/stop',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({plugin_id:pid})});
    if (r?.ok) { showToast('Plugin fermato.','info'); disconnectPluginLogSSE(pid); loadPlugins(); }
    else showToast(await apiErrorMessage(r, 'Errore stop.'), 'error');
}
async function doRestartPlugin(pid, skipConfirm=false) {
    if (!skipConfirm && !confirm(`Riavviare "${pid}"? Se qualcuno lo sta usando per uno stream, si interrompe.`)) return;
    disconnectPluginLogSSE(pid);
    const r = await apiFetch('/admin/plugins/restart',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({plugin_id:pid})});
    if (r?.ok) { showToast('Plugin riavviato.','success'); setTimeout(loadPlugins, 900); }
    else showToast(await apiErrorMessage(r, 'Errore riavvio.'), 'error');
}
async function doUninstallPlugin(pid, isLua=false) {
    if (_installing) return;
    if (!confirm(`Disinstallare "${pid}"? Operazione irreversibile.`)) return;
    disconnectPluginLogSSE(pid);
    const url = isLua ? '/admin/lua-plugins/uninstall' : '/admin/plugins/uninstall';
    const r = await apiFetch(url,{method:'DELETE',headers:{'Content-Type':'application/json'},body:JSON.stringify({plugin_id:pid})});
    if (r?.ok) { showToast('Plugin disinstallato.','info'); loadPlugins(); }
    else showToast(await apiErrorMessage(r, 'Errore disinstallazione.'), 'error');
}
async function togglePluginStatus(pid, turnOn) {
    const r = await apiFetch('/admin/plugins/toggle',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({plugin_id:pid,active:turnOn})});
    if (r?.ok) { showToast(`Plugin ${turnOn?'attivato':'disattivato'}.`,'info'); loadPlugins(); }
    else showToast(await apiErrorMessage(r, 'Errore cambio stato.'), 'error');
}
// Ciclo di vita dei plugin Lua: start | stop | restart. `start` è rifiutato
// (409) se restano campi obbligatori vuoti; `restart` solo su plugin attivo.
async function luaPluginState(pid, action) {
    if ((action === 'stop' || action === 'restart') &&
        !confirm(`${action === 'stop' ? 'Fermare' : 'Riavviare'} "${pid}"? Se è in uso per uno stream, si interrompe.`)) return;
    if (action !== 'start') disconnectPluginLogSSE(pid);
    const r = await apiFetch(`/admin/lua-plugins/state/${encodeURIComponent(pid)}`,
        {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action})});
    const d = r ? await r.json().catch(() => ({})) : {};
    if (r?.ok && d.ok !== false) {
        showToast({start:'Plugin avviato.', stop:'Plugin fermato.', restart:'Plugin riavviato.'}[action],
                  action === 'stop' ? 'info' : 'success');
        setTimeout(loadPlugins, action === 'restart' ? 900 : 300);
    } else {
        showToast(d.message || await apiErrorMessage(r, 'Operazione non riuscita.'), 'error');
    }
}
function togglePluginPanel(panelId, ...rest) {
    rest.forEach(id => document.getElementById(id)?.classList.add('hidden'));
    document.getElementById(panelId)?.classList.toggle('hidden');
}

// ─── Code editor ──────────────────────────────────────────────────────────────
async function openEditor(pid, name) {
    _editorPlugin = { id: pid, name };
    _editorDirty = false;
    _editorCurrentPath = null;
    document.getElementById('editor-plugin-name').textContent = name;
    document.getElementById('editor-plugin-id').textContent = pid;
    document.getElementById('editor-save-status').textContent = '';
    document.getElementById('editor-info-bar').classList.remove('hidden');
    document.getElementById('editor-modal').classList.remove('hidden');
    document.getElementById('editor-textarea').value = 'Caricamento file…';

    // Load file list
    const r = await apiFetch(`/admin/lua-plugins/files/${encodeURIComponent(pid)}`);
    const sel = document.getElementById('editor-file-select');
    sel.innerHTML = '';
    if (r?.ok) {
        const files = await r.json();
        for (const f of files) {
            const opt = document.createElement('option');
            opt.value = f.path; opt.textContent = f.path;
            sel.appendChild(opt);
        }
    } else {
        const opt = document.createElement('option'); opt.value='init.lua'; opt.textContent='init.lua'; sel.appendChild(opt);
    }
    await loadEditorFile();
}

async function loadEditorFile() {
    const pid = _editorPlugin?.id;
    if (!pid) return;
    const sel = document.getElementById('editor-file-select');
    const path = sel.value || 'init.lua';
    // Cambio file dal menu a tendina: se il buffer corrente ha modifiche non
    // salvate, chiedere prima di scartarle invece di perderle in silenzio.
    if (_editorDirty && path !== _editorCurrentPath) {
        if (!confirm(`Ci sono modifiche non salvate su "${_editorCurrentPath}". Cambiare file e scartarle?`)) {
            sel.value = _editorCurrentPath;
            return;
        }
    }
    const r = await apiFetch(`/admin/lua-plugins/file/${encodeURIComponent(pid)}?path=${encodeURIComponent(path)}`);
    const ta = document.getElementById('editor-textarea');
    if (r?.ok) {
        const { content } = await r.json();
        ta.value = content;
        document.getElementById('editor-save-status').textContent = '';
    } else {
        ta.value = '-- Errore caricamento file';
    }
    _editorCurrentPath = path;
    _editorDirty = false;
}

async function saveEditorFile() {
    const pid = _editorPlugin?.id;
    if (!pid) return;
    const path = document.getElementById('editor-file-select').value || 'init.lua';
    const content = document.getElementById('editor-textarea').value;
    const statusEl = document.getElementById('editor-save-status');
    statusEl.textContent = 'Salvataggio…'; statusEl.className = 'text-gray-500';
    const r = await apiFetch(`/admin/lua-plugins/file/${encodeURIComponent(pid)}?path=${encodeURIComponent(path)}`, {
        method: 'PUT', headers: {'Content-Type':'application/json'},
        body: JSON.stringify({ content }),
    });
    if (r?.ok) {
        statusEl.textContent = 'Salvato — hot-reload in corso'; statusEl.className = 'text-emerald-400';
        showToast('File salvato. Hot-reload attivo.','success');
        _editorDirty = false;
        setTimeout(() => { statusEl.textContent = ''; }, 4000);
    } else {
        statusEl.textContent = 'Errore salvataggio'; statusEl.className = 'text-red-400';
        showToast(await apiErrorMessage(r, 'Errore salvataggio file.'), 'error');
    }
}

// Tab to insert spaces in editor
document.getElementById('editor-textarea')?.addEventListener('keydown', e => {
    if (e.key === 'Tab') {
        e.preventDefault();
        const ta = e.target, s = ta.selectionStart, end = ta.selectionEnd;
        ta.value = ta.value.substring(0,s) + '    ' + ta.value.substring(end);
        ta.selectionStart = ta.selectionEnd = s + 4;
        _editorDirty = true; // .value impostato da JS non genera un evento 'input'
    }
    if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); saveEditorFile(); }
});
document.getElementById('editor-textarea')?.addEventListener('input', () => { _editorDirty = true; });

function closeEditor() {
    if (_editorDirty && !confirm('Ci sono modifiche non salvate. Chiudere comunque?')) return;
    document.getElementById('editor-modal').classList.add('hidden');
    _editorPlugin = null;
    _editorDirty = false;
}

// ─── System logs SSE ──────────────────────────────────────────────────────────
function connectLogSSE() {
    if (_logEventSource) { _logEventSource.close(); _logEventSource = null; }
    const container = document.getElementById('log-output');
    const dot = document.getElementById('log-live-dot');
    if (!container) return;
    container.innerHTML = '';
    if (dot) dot.className = 'inline-block w-2 h-2 rounded-full bg-yellow-400';
    _logEventSource = new EventSource('/admin/logs/stream');
    _logEventSource.onopen = () => {
        if (dot) dot.className = 'inline-block w-2 h-2 rounded-full bg-emerald-400 shadow shadow-emerald-400/50';
    };
    _logEventSource.onmessage = e => {
        const line = e.data;
        if (!line || line.startsWith(':')) return;
        if (container.textContent === 'Caricamento log…') container.innerHTML = '';
        if (container.childNodes.length > 0) container.appendChild(document.createTextNode('\n'));
        const span = document.createElement('span');
        span.className = `log-line ${logColor(line)}`;
        span.textContent = line;
        container.appendChild(span);
        while (container.childElementCount > 500) container.removeChild(container.firstChild);
        container.scrollTop = container.scrollHeight;
    };
    _logEventSource.onerror = () => {
        if (dot) dot.className = 'inline-block w-2 h-2 rounded-full bg-red-500';
        _logEventSource.close(); _logEventSource = null;
        setTimeout(() => {
            const tab = document.getElementById('tab-logs');
            if (tab && !tab.classList.contains('hidden')) connectLogSSE();
        }, 5000);
    };
}
function disconnectLogSSE() {
    if (_logEventSource) { _logEventSource.close(); _logEventSource = null; }
    const dot = document.getElementById('log-live-dot');
    if (dot) dot.className = 'inline-block w-2 h-2 rounded-full bg-gray-700';
}
function reconnectLogSSE() { connectLogSSE(); }

// ─── Version check ────────────────────────────────────────────────────────────
// normalizeVersion mirra core.normalizeVersion (Go): toglie un eventuale
// prefisso "v"/"V" iniziale, così "1.3.2" e "v1.3.2" risultano la stessa
// versione a prescindere da quale lato (in esecuzione vs. ultima su GitHub)
// porta il prefisso — i tag git sono sempre "vX.Y.Z", core.Version no.
function normalizeVersion(s) {
    return String(s || '').replace(/^[vV]/, '');
}
async function checkCoreVersion() {
    const r = await apiFetch('/admin/info');
    if (!r?.ok) return;
    const d = await r.json();
    const ve = document.getElementById('core-version');
    if (ve && d.version) ve.textContent = d.version;
    const ove = document.getElementById('ov-core-version');
    if (ove && d.version) ove.textContent = d.version;

    // App web Pileus: precompila il repo salvato e abilita il bottone di
    // aggiornamento solo se un repo è configurato (vedi pileus_web_repo in
    // getAdminInfo). Non tocca il campo se l'admin ci sta scrivendo dentro.
    const repoInput = document.getElementById('pileus-web-repo');
    if (repoInput && document.activeElement !== repoInput) repoInput.value = d.pileus_web_repo || '';
    const pwBtn = document.getElementById('pileus-web-update-btn');
    if (pwBtn) pwBtn.disabled = !d.pileus_web_repo;
    const pwStatus = document.getElementById('pileus-web-status');
    if (pwStatus && !pwStatus.dataset.busy) {
        pwStatus.textContent = d.pileus_web_repo ? '' : "Configura il repo Pileus qui sopra per abilitare l'aggiornamento.";
    }

    if (!d.latest_version || normalizeVersion(d.latest_version) === normalizeVersion(d.version)) return;
    document.getElementById('update-banner')?.classList.remove('hidden');
    const vs = document.getElementById('update-version');
    if (vs) vs.textContent = d.latest_version;
}
async function triggerCoreUpdate() {
    const btn = document.getElementById('update-btn');
    if (btn) { btn.disabled=true; btn.textContent='…'; }
    const r = await apiFetch('/admin/core/update',{method:'POST'});
    if (!r) { if (btn) { btn.disabled=false; btn.textContent='Aggiorna'; } return; }
    const d = await r.json();
    if (d.status === 'up_to_date') { showToast('Già aggiornato.','info'); document.getElementById('update-banner')?.classList.add('hidden'); }
    else if (d.status === 'updating') showToast('Aggiornamento — il servizio si riavvierà.','success');
    else { showToast(d.detail||'Errore.','error'); if(btn){btn.disabled=false;btn.textContent='Aggiorna';} }
}

// ─── App web Pileus ──────────────────────────────────────────────────────────
async function savePileusWebRepo(event) {
    event.preventDefault();
    const val = document.getElementById('pileus-web-repo').value.trim();
    const r = await apiFetch('/admin/settings/save', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ pileus_web_repo: val }),
    });
    if (r?.ok) { showToast('Repo Pileus salvato.', 'success'); checkCoreVersion(); }
    else showToast(await apiErrorMessage(r, 'Salvataggio fallito.'), 'error');
}

// updatePileusWebApp(force) calls POST /admin/pileus-web/update. On a
// version-mismatch warning (ok:false + warning, no force requested yet) it
// asks for native confirm() before retrying with force:true — same
// "explicit confirmation for a risky action" pattern as _dangerPost above,
// just without the admin-password re-entry (a version mismatch is a
// warning, not a destructive action).
async function updatePileusWebApp(force) {
    const btn = document.getElementById('pileus-web-update-btn');
    const status = document.getElementById('pileus-web-status');
    if (btn) { btn.disabled = true; btn.textContent = 'Aggiorno…'; }
    if (status) { status.dataset.busy = '1'; status.textContent = 'Aggiornamento in corso…'; }
    const r = await apiFetch('/admin/pileus-web/update', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ force: !!force }),
    });
    if (btn) { btn.disabled = false; btn.textContent = 'Aggiorna app web Pileus'; }
    if (status) delete status.dataset.busy;
    if (!r) return;
    const d = await r.json().catch(() => ({}));
    if (!r.ok) {
        showToast(d.detail || 'Errore aggiornamento.', 'error');
        if (status) status.textContent = d.detail || 'Errore aggiornamento.';
        return;
    }
    if (d.ok === false && d.warning) {
        if (window.confirm(d.warning + '\n\nProcedere comunque?')) {
            await updatePileusWebApp(true);
        } else if (status) {
            status.textContent = d.warning;
        }
        return;
    }
    if (d.ok) {
        // textContent, mai innerHTML: version/asset arrivano da GitHub via
        // il repo configurato — non fidati, ma qui non c'è comunque bisogno
        // di HTML, testContent non li interpreta mai come markup.
        if (status) status.textContent = `Aggiornata a ${d.version} (${d.asset}).`;
        showToast(`App web Pileus aggiornata a ${d.version}.`, 'success');
    } else {
        showToast(d.detail || 'Errore aggiornamento.', 'error');
        if (status) status.textContent = d.detail || 'Errore aggiornamento.';
    }
}

// ─── Init ─────────────────────────────────────────────────────────────────────
// pollResources alimenta anche l'header aggregato (agg-cpu/agg-mem/agg-uptime,
// sempre visibile fuori dai div dei tab) — non si può fermare del tutto sui
// tab Plugin/Log senza far restare l'header stantio. Rallentato invece a 20s
// quando il tab attivo non è Panoramica (dove il dettaglio per-servizio è
// visibile e vale la pena tenerlo fresco a 5s), invece dei 5s fissi di sempre.
function scheduleResourcePoll() {
    pollResources();
    setTimeout(scheduleResourcePoll, _activeTab === 'overview' ? 5000 : 20000);
}

window.onload = () => {
    loadPlugins();
    checkCoreVersion();
    scheduleResourcePoll();
    loadPileusDevices();
    setInterval(checkCoreVersion, 3_600_000);

    // Drawer: Esc closes it (mobile); full URL on the iOS-download link.
    document.addEventListener('keydown', e => { if (e.key === 'Escape') closeNav(); });
    const dl = document.getElementById('dl-app-link');
    if (dl) { const u = window.location.origin + '/app/'; dl.href = u; dl.textContent = u; }
};

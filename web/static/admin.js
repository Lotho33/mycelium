// ─── State ───────────────────────────────────────────────────────────────────
let _installing = false;
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

// apiErrorMessage extracts a useful error message from an apiFetch response:
// some handlers answer JSON ({"message": "..."}), others plain text.
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

// esc(): escaping for HTML text and double-quoted attributes only. Never use
// it to build onclick="fn('${...}')": attribute entities are decoded before
// the JS runs. Use data-* attributes and delegated listeners instead.
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

// Not built on _dangerPost: this wipes the admin identity, so success must
// leave the tab on /setup.
async function factoryReset() {
    if (!window.confirm(
        'Reset di fabbrica: cancella TUTTO — profili, dispositivi accoppiati, dati e cache dei plugin, i plugin stessi, e la password admin. ' +
        'Tornerai alla pagina di setup come alla primissima installazione. Questa azione è irreversibile. Continuare?'
    )) return;
    const pw = window.prompt('Password admin per confermare:');
    if (!pw) return;
    const r = await apiFetch('/admin/factory-reset', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: pw }),
    });
    if (!r) return;
    if (!r.ok) { showToast(await apiErrorMessage(r, 'Reset fallito.'), 'error'); return; }
    window.location.href = '/setup';
}

// One plugin's cache (Redis mycelium:plugin:<id>:cache:* and its on-disk
// cache files). name only appears in native dialogs, never in the DOM.
function doWipePluginData(pid, name) {
    _dangerPost(
        `/admin/lua-plugins/wipe-data/${encodeURIComponent(pid)}`,
        `Cancellare la cache/i dati di "${name}" (chiavi Redis del plugin + file cache su disco)? Il plugin resta installato e configurato — solo i dati raccolti/scaricati vengono cancellati. Irreversibile.`,
        d => {
            const extra = d.redis_error ? ` — errore Redis: ${d.redis_error}` : '';
            return `Dati di "${name}" cancellati: ${d.redis_keys ?? 0} chiavi Redis, ${d.cache_files ?? 0} file cache${extra}.`;
        },
    );
}

// ─── Admin password change ──────────────────────────────────────────────────
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
const _pages = ['plugins','devices','users','offline','vpn','settings','logs','download'];
const _pageTitles = {
    plugins: 'Plugin', devices: 'Dispositivi', users: 'Utenti',
    offline: 'Download offline', vpn: 'Rete', settings: 'Impostazioni',
    logs: 'Log di Sistema', download: 'iOS / macOS',
};
let _activeTab = 'plugins';

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
    if (!_pages.includes(id)) id = 'plugins';
    _activeTab = id;
    _pages.forEach(t => {
        document.getElementById(`tab-${t}`)?.classList.toggle('hidden', t !== id);
        document.getElementById(`nav-btn-${t}`)?.classList.toggle('active', t === id);
    });
    const tt = document.getElementById('topbar-title');
    if (tt) tt.textContent = _pageTitles[id] || 'Mycelium';
    if (id === 'logs') connectLogSSE(); else disconnectLogSSE();
    if (id === 'vpn') { loadEgress(); }
    if (id === 'devices') loadPileusDevices();
    if (id === 'users') loadPileusProfiles();
    if (id === 'offline') loadDownloadsAdmin();
    closeNav();
}

// ─── Core stats ───────────────────────────────────────────────────────────────
async function pollResources() {
    const r = await apiFetch('/admin/core/resources');
    if (!r?.ok) return;
    const core = await r.json();
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
// setStatusBox colors a status box: null = neutral/loading, true = success,
// false = error. Always visible in the modal (unlike toasts).
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
// ─── Network exits (egress registry) ────────────────────────────────────────
// Last profile list, reused for the per-plugin <select>.
let _egressProfiles = [{ name: 'direct', kind: 'direct', enabled: true }];

// A sanitised proxy string (socks5://127.0.0.1:1080 → "proxy-127.0.0.1-1080")
// can show up in place of a name in a couple of spots — map it back to the
// egress profile name when we can, so the UI reads "warp" instead of that.
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
        // For wireproxy the dot shows the actual process state.
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
// Delegated listener: never pass names through an onclick string.
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
// WireGuard .conf → wireproxy
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

// ─── Pileus pairing (rotating code) ──────────────────────────────────────────
// Inline in the Overview's Pileus card. A short poll (2s) after generating
// detects both the code's expiry and a device consuming it (pairing happens
// over gRPC, not through this dashboard).
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
        // Back to the "Genera" button after a few seconds, ready for the next
        // device.
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

// ─── Pileus devices (token revocation) ───────────────────────────────────────
// escapeHtml: device_id (chosen by the device) and label (a device can
// rename itself) are untrusted text. Never build these rows with raw
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

// One delegated listener on the box (survives innerHTML refreshes): no
// untrusted data in JS strings. dataset.id comes back decoded.
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

// Stronger than revoking: the device disappears from the list. Profiles are
// untouched (they are shared by every device).
async function deletePileusDevice(id) {
    if (!confirm('Eliminare del tutto questo dispositivo? Sparirà dalla lista; i profili restano. Se torna a presentarsi con un codice valido, ricompare.')) return;
    const r = await apiFetch(`/admin/pileus/devices/${encodeURIComponent(id)}`, { method: 'DELETE' });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Eliminazione fallita.'), 'error'); return; }
    showToast('Dispositivo eliminato.', 'success');
    loadPileusDevices();
}

// ─── Users (Pileus profiles, shared by every device) ─────────────────────────
async function loadPileusProfiles() {
    const box = document.getElementById('pileus-profiles');
    if (!box) return;
    const r = await apiFetch('/admin/pileus/profiles');
    if (!r?.ok) { box.textContent = 'Impossibile leggere i profili.'; return; }
    const profiles = (await r.json()).profiles || [];
    if (!profiles.length) { box.textContent = 'Nessun profilo creato.'; return; }
    box.innerHTML = profiles.map(p => {
        const idAttr = escapeHtml(p.profile_id);
        const name = escapeHtml(p.name);
        const trusted = (p.trusted_devices || []).map(t => {
            const label = escapeHtml(t.label || t.device_id.slice(0, 8));
            return `<span class="inline-flex items-center gap-1 mr-2">${label}
                <button data-id="${idAttr}" data-device="${escapeHtml(t.device_id)}" data-action="untrust" class="pileus-profile-btn text-gray-500 hover:text-red-400" title="Revoca la fiducia di questo dispositivo">✕</button></span>`;
        }).join('');
        const pinInfo = p.pin_protected
            ? `<div class="text-[11px] text-gray-500 mt-0.5">🔒 PIN attivo${trusted ? ' · dispositivi fidati: ' + trusted : ' · nessun dispositivo fidato'}</div>`
            : '';
        return `<div class="flex items-center gap-2 py-1.5 border-b border-gray-800 last:border-0">
            <div class="min-w-0">
              <div class="text-white truncate">${name}</div>
              ${pinInfo}
            </div>
            <span class="ml-auto flex items-center gap-2">
              <button data-id="${idAttr}" data-action="pin" class="pileus-profile-btn text-[11px] text-gray-400 hover:text-white" title="Imposta o cambia il PIN">${p.pin_protected ? 'Cambia PIN' : 'Imposta PIN'}</button>
              ${p.pin_protected ? `<button data-id="${idAttr}" data-action="unpin" class="pileus-profile-btn text-[11px] text-gray-500 hover:text-red-400" title="Rimuovi il PIN">Rimuovi PIN</button>` : ''}
              <button data-id="${idAttr}" data-action="rename" class="pileus-profile-btn text-[11px] text-gray-400 hover:text-white" title="Rinomina">Rinomina</button>
              <button data-id="${idAttr}" data-action="delete" class="pileus-profile-btn text-[11px] text-gray-500 hover:text-red-400" title="Elimina">Elimina</button>
            </span>
        </div>`;
    }).join('');
}

document.getElementById('pileus-profiles')?.addEventListener('click', e => {
    const btn = e.target.closest('.pileus-profile-btn');
    if (!btn) return;
    const id = btn.dataset.id;
    switch (btn.dataset.action) {
        case 'rename': renamePileusProfile(id); break;
        case 'delete': deletePileusProfile(id); break;
        case 'pin': setPileusProfilePin(id); break;
        case 'unpin': removePileusProfilePin(id); break;
        case 'untrust': untrustPileusProfileDevice(id, btn.dataset.device); break;
    }
});

// Profile PIN: the admin doesn't need the current PIN (recovery path). Every
// change revokes the profile's trusted devices and sessions.
async function setPileusProfilePin(id) {
    const pin = prompt('Nuovo PIN per questo profilo (4-8 cifre). Tutti i dispositivi dovranno reinserirlo:');
    if (pin === null) return;
    if (!/^[0-9]{4,8}$/.test(pin.trim())) { showToast('Il PIN deve essere di 4-8 cifre.', 'error'); return; }
    await postPileusProfilePin(id, pin.trim(), 'PIN impostato.');
}

async function removePileusProfilePin(id) {
    if (!confirm('Rimuovere il PIN? Il profilo tornerà utilizzabile da qualunque dispositivo associato.')) return;
    await postPileusProfilePin(id, '', 'PIN rimosso.');
}

async function postPileusProfilePin(id, pin, okMsg) {
    const r = await apiFetch(`/admin/pileus/profiles/${encodeURIComponent(id)}/pin`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ pin }),
    });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Operazione fallita.'), 'error'); return; }
    showToast(okMsg, 'success');
    loadPileusProfiles();
}

async function untrustPileusProfileDevice(id, deviceId) {
    if (!confirm('Revocare la fiducia di questo dispositivo? Dovrà reinserire il PIN.')) return;
    const r = await apiFetch(`/admin/pileus/profiles/${encodeURIComponent(id)}/trust/${encodeURIComponent(deviceId)}`, { method: 'DELETE' });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Operazione fallita.'), 'error'); return; }
    showToast('Fiducia revocata.', 'success');
    loadPileusProfiles();
}

async function createPileusProfile() {
    const nameEl = document.getElementById('new-profile-name');
    const name = nameEl.value.trim();
    if (!name) { showToast('Serve un nome.', 'error'); return; }
    const r = await apiFetch('/admin/pileus/profiles', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, avatar_url: '' }),
    });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Creazione fallita.'), 'error'); return; }
    nameEl.value = '';
    showToast('Profilo creato.', 'success');
    loadPileusProfiles();
}

// findPileusProfile re-reads the current list, so a rename starts from the
// server's current values.
async function findPileusProfile(id) {
    const r = await apiFetch('/admin/pileus/profiles');
    if (!r?.ok) return null;
    const profiles = (await r.json()).profiles || [];
    return profiles.find(p => p.profile_id === id) || null;
}

async function renamePileusProfile(id) {
    const current = await findPileusProfile(id);
    if (!current) { showToast('Profilo non trovato.', 'error'); return; }
    const name = prompt('Nuovo nome per questo profilo:', current.name);
    if (name === null || name.trim() === '') return; // annullato
    const r = await apiFetch(`/admin/pileus/profiles/${encodeURIComponent(id)}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: name.trim(), avatar_url: current.avatar_url }),
    });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Rinomina fallita.'), 'error'); return; }
    showToast('Profilo rinominato.', 'success');
    loadPileusProfiles();
}

async function deletePileusProfile(id) {
    if (!confirm('Eliminare questo profilo? La cronologia visione associata resta salvata ma orfana, non è recuperabile da nessun profilo.')) return;
    const r = await apiFetch(`/admin/pileus/profiles/${encodeURIComponent(id)}`, { method: 'DELETE' });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Eliminazione fallita.'), 'error'); return; }
    showToast('Profilo eliminato.', 'success');
    loadPileusProfiles();
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

// ─── Plugin list ──────────────────────────────────────────────────────────────
async function loadPlugins() {
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
    // The dot carries the state (color), with the detail in its title.
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

    // EGRESS panel (Lua only): the network exit of the video flow
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

    const editBtn = isLua
        ? `<button data-action="open-editor"
               class="text-[10px] text-gray-500 hover:text-gray-300 transition">Codice</button>` : '';

    // Enable/disable toggle: non-Lua plugins only; Lua plugins have the
    // start/stop lifecycle (luaOps).
    const toggleBtn = isLua ? '' : `<button data-action="toggle-status"
               class="text-[10px] ${isEnabled?'text-gray-600 hover:text-gray-400':'text-emerald-600 hover:text-emerald-400'} transition">
               ${isEnabled?'Disabilita':'Abilita'}</button>`;
    const uninstallBtn = `<button data-action="uninstall"
               class="text-[10px] text-gray-700 hover:text-red-400 transition" title="Disinstalla">Elimina</button>`;
    // Lua only: clears the plugin's data (it stays installed), unlike "Elimina".
    const wipeDataBtn = isLua ? `<button data-action="wipe-data"
               class="text-[10px] text-gray-700 hover:text-amber-400 transition" title="Cancella cache/dati di questo plugin (Redis + file su disco), senza disinstallarlo">Cancella dati</button>` : '';

    const gRPCOps = isLua ? '' : isProcess
        ? `<button data-action="stop"    class="text-[10px] text-gray-600 hover:text-red-400 transition">Stop</button>
           <button data-action="restart" class="text-[10px] text-gray-600 hover:text-yellow-400 transition">Riavvia</button>`
        : `<button data-action="start" class="text-[10px] text-gray-600 hover:text-emerald-400 transition">Avvia</button>`;

    // Lua lifecycle: one Avvia/Ferma toggle, plus Riavvia (disabled while
    // stopped or waiting for configuration).
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
                <!-- PID/RAM/CPU for out-of-process plugins, filled by updatePluginRes() -->
                ${isProcess && !isLua ? `<p id="${resId}" class="text-[10px] font-mono mt-0.5 text-gray-600">—</p>` : ''}
            </div>
            <!-- Actions -->
            <div class="flex items-center gap-x-2.5 gap-y-1 shrink-0 flex-wrap justify-end ml-auto">
                ${gRPCOps}${luaOps}${editBtn}${toggleBtn}${wipeDataBtn}${uninstallBtn}
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

    // One delegated listener for every button of the card: pid comes from the
    // plugin manifest and is never interpolated into a JS string.
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
            case 'open-editor':    openEditor(pid, data.plugin_name || pid); break;
            case 'toggle-status':  if (!_installing) togglePluginStatus(pid, !isEnabled); break;
            case 'uninstall':      if (!_installing) doUninstallPlugin(pid, isLua); break;
            case 'wipe-data':      if (!_installing) doWipePluginData(pid, data.plugin_name || pid); break;
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
    // 409 (another task running) carries a specific message: show it.
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
            // Always sent (even "false"): an unchecked switch is an explicit value.
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
    // Stopping a plugin may interrupt a stream someone is watching: confirm.
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
// Lua plugin lifecycle: start | stop | restart. start is refused (409)
// while required settings are empty; restart only on a running plugin.
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
    // Switching file with unsaved changes asks before discarding them.
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

// "latest is really newer" is decided server-side (d.update_available).
async function checkCoreVersion() {
    const r = await apiFetch('/admin/info');
    if (!r?.ok) return;
    const d = await r.json();
    const ve = document.getElementById('core-version');
    if (ve && d.version) ve.textContent = d.version;
    const ove = document.getElementById('ov-core-version');
    if (ove && d.version) ove.textContent = d.version;

    // Pileus web app: prefill the saved repo and enable the update button only
    // when one is configured. Don't touch the field while it's being edited.
    const repoInput = document.getElementById('pileus-web-repo');
    if (repoInput && document.activeElement !== repoInput) repoInput.value = d.pileus_web_repo || '';
    const pwBtn = document.getElementById('pileus-web-update-btn');
    if (pwBtn) pwBtn.disabled = !d.pileus_web_repo;
    const ipv6 = document.getElementById('egress-ipv6');
    if (ipv6 && typeof d.egress_ipv6 === 'boolean') ipv6.checked = d.egress_ipv6;

    const dlSet = (id, v, def) => { const el = document.getElementById(id); if (el && document.activeElement !== el) el.value = v ?? def; };
    dlSet('dl-quota', d.download_quota_gb, 20);
    dlSet('dl-minfree', d.download_min_free_gb, 3);
    dlSet('dl-retention', d.download_retention_days, 7);
    const dlEn = document.getElementById('dl-enabled');
    if (dlEn && typeof d.download_enabled === 'boolean') dlEn.checked = d.download_enabled;
    dlSet('dl-maxmbps', d.download_max_mbps, 0);
    dlSet('dl-pace', d.download_pace, 4);
    const dlPause = document.getElementById('dl-pause');
    if (dlPause && typeof d.download_pause_while_streaming === 'boolean') dlPause.checked = d.download_pause_while_streaming;
    const dlDel = document.getElementById('dl-delete-fetched');
    if (dlDel && typeof d.download_delete_after_fetch === 'boolean') dlDel.checked = d.download_delete_after_fetch;

    // Download tab (iOS/macOS): link to the web app on the dashboard's origin.
    const secureOrigin = window.location.origin;
    const dlApp = document.getElementById('dl-app-link');
    if (dlApp) { const u = secureOrigin + '/app/'; dlApp.href = u; dlApp.textContent = u; }

    // Installed web build version (version.json of data/pileus-web).
    const pwVer = document.getElementById('pileus-web-version');
    if (pwVer) {
        pwVer.textContent = d.pileus_web_version
            ? d.pileus_web_version + (d.pileus_web_build ? ` (build ${d.pileus_web_build})` : '')
            : 'nessuna app web installata';
    }
    const pwStatus = document.getElementById('pileus-web-status');
    if (pwStatus && !pwStatus.dataset.busy) {
        pwStatus.textContent = d.pileus_web_repo ? '' : "Configura il repo Pileus qui sopra per abilitare l'aggiornamento.";
    }

    if (!d.latest_version || !d.update_available) return;
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
// version-mismatch warning it asks for confirmation before retrying with
// force:true.
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
        // textContent, never innerHTML: version/asset come from GitHub.
        const integrity = d.checksum_verified ? ' Checksum verificato.' : (d.warning ? ' ' + d.warning : '');
        if (status) status.textContent = `Aggiornata a ${d.version} (${d.asset}).${integrity}`;
        showToast(`App web Pileus aggiornata a ${d.version}.`, d.checksum_verified ? 'success' : 'warn');
        checkCoreVersion(); // aggiorna la riga "Versione installata"
    } else {
        showToast(d.detail || 'Errore aggiornamento.', 'error');
        if (status) status.textContent = d.detail || 'Errore aggiornamento.';
    }
}

// Network → "Prefer IPv6": saved as a setting, applied live.
async function saveEgressIPv6(cb) {
    const r = await apiFetch('/admin/settings/save', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ egress_ipv6: cb.checked ? '1' : '0' }),
    });
    if (r?.ok) {
        showToast(cb.checked ? 'IPv6 preferito in uscita.' : 'IPv4 preferito in uscita.', 'success');
    } else {
        cb.checked = !cb.checked;
        showToast('Salvataggio non riuscito.', 'error');
    }
}

// Network → "Restart": the server exits cleanly and the container restart
// policy brings it back; the page reloads by itself.
async function restartService() {
    if (!confirm('Il servizio verrà riavviato: streaming e connessioni attive si interrompono per qualche secondo, poi tutto riparte da solo. Continuare?')) return;
    const r = await apiFetch('/admin/system/restart', { method: 'POST' });
    if (!r?.ok) {
        showToast('Riavvio non riuscito.', 'error');
        return;
    }
    showToast('Riavvio in corso…', 'success');
    setTimeout(() => window.location.reload(), 6000);
}

// pollResources also feeds the always-visible header, so it never stops:
// every 5s on the Plugin page, 20s elsewhere.
function scheduleResourcePoll() {
    pollResources();
    setTimeout(scheduleResourcePoll, _activeTab === 'plugins' ? 5000 : 20000);
}

window.onload = () => {
    loadPlugins();
    checkCoreVersion();
    scheduleResourcePoll();
    loadPileusDevices();
    setInterval(checkCoreVersion, 3_600_000);

    // Drawer: Esc closes it (mobile).
    document.addEventListener('keydown', e => { if (e.key === 'Escape') closeNav(); });
};


// Files are prepared by the server (internal/downloads); here only space,
// retention rules and the queue of every profile.
function fmtBytes(b) {
    if (!b || b < 0) return '0 MB';
    if (b >= 1e9) return (b / 1e9).toFixed(1) + ' GB';
    return Math.round(b / 1e6) + ' MB';
}

const _dlStatus = {
    queued: 'in coda', scheduled: 'programmato', running: 'in corso', paused: 'in pausa (qualcuno guarda)',
    completed: 'pronto', failed: 'fallito', canceled: 'annullato',
};

async function loadDownloadsAdmin() {
    const box = document.getElementById('dl-list');
    const usage = document.getElementById('dl-usage');
    if (!box) return;
    const r = await apiFetch('/admin/downloads');
    if (!r?.ok) { box.textContent = 'Impossibile leggere i download.'; return; }
    const d = await r.json();
    if (!d.enabled) { usage.textContent = 'Download non attivi su questo server.'; box.textContent = '—'; return; }
    usage.textContent = `Usati ${fmtBytes(d.used_bytes)} su ${fmtBytes(d.quota_bytes)} · ancora disponibili ${fmtBytes(d.available_bytes)}` +
        (d.ffmpeg ? '' : ' · ⚠️ ffmpeg non trovato: i download non possono partire');
    const rows = d.downloads || [];
    if (!rows.length) { box.textContent = 'Nessun download.'; return; }
    box.innerHTML = rows.map(x => {
        let name = x.series_title ? `${x.series_title}${x.season_number ? ` S${x.season_number}E${x.episode_number}` : ''} — ${x.title}` : x.title;
        name = escapeHtml(name || x.download_id);
        let state = _dlStatus[x.status] || x.status;
        if (x.status === 'running') state += ` ${Math.round((x.progress || 0) * 100)}%`;
        if (x.status === 'scheduled' && x.scheduled_for) state += ' alle ' + new Date(x.scheduled_for * 1000).toLocaleString('it-IT', { dateStyle: 'short', timeStyle: 'short' });
        const size = x.status === 'completed' ? fmtBytes(x.file_bytes) : '~' + fmtBytes(x.estimated_bytes);
        if (x.shared) state += ' · condiviso';
        if (x.upgrade_pending) state += ' · cerco qualità migliore';
        if (x.fetched) state += ' · sul dispositivo';
        const err = x.error ? `<div class="text-[11px] text-red-400/80 mt-0.5">${escapeHtml(x.error)}</div>` : '';
        return `<div class="flex items-center gap-2 py-1.5 border-b border-gray-800 last:border-0">
            <div class="min-w-0">
              <div class="text-white truncate">${name}</div>
              <div class="text-[11px] text-gray-500">${escapeHtml(x.plugin_id)} · ${escapeHtml(x.quality_label || '')} · ${size} · ${escapeHtml(state)}</div>
              ${err}
            </div>
            <button data-id="${escapeHtml(x.download_id)}" class="dl-del-btn ml-auto text-[11px] text-gray-500 hover:text-red-400" title="Elimina">Elimina</button>
        </div>`;
    }).join('');
}

document.getElementById('dl-list')?.addEventListener('click', async e => {
    const btn = e.target.closest('.dl-del-btn');
    if (!btn) return;
    if (!confirm('Eliminare questo download? Il file sparisce dal server (non dai dispositivi che l\'hanno già scaricato).')) return;
    const r = await apiFetch(`/admin/downloads/${encodeURIComponent(btn.dataset.id)}`, { method: 'DELETE' });
    if (!r?.ok) { showToast(await apiErrorMessage(r, 'Eliminazione fallita.'), 'error'); return; }
    showToast('Download eliminato.', 'success');
    loadDownloadsAdmin();
});

async function saveDownloadSettings() {
    const num = (id, def) => {
        const v = parseFloat(document.getElementById(id).value);
        return isNaN(v) || v < 0 ? String(def) : String(v);
    };
    const r = await apiFetch('/admin/settings/save', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            download_enabled: document.getElementById('dl-enabled').checked ? '1' : '0',
            download_quota_gb: num('dl-quota', 20),
            download_min_free_gb: num('dl-minfree', 3),
            download_retention_days: num('dl-retention', 7),
            download_pause_while_streaming: document.getElementById('dl-pause').checked ? '1' : '0',
            download_delete_after_fetch: document.getElementById('dl-delete-fetched').checked ? '1' : '0',
            download_max_mbps: num('dl-maxmbps', 0),
            download_pace: num('dl-pace', 4),
        }),
    });
    if (r?.ok) { showToast('Salvato.', 'success'); loadDownloadsAdmin(); }
    else showToast('Salvataggio non riuscito.', 'error');
}

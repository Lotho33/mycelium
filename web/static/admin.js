'use strict';
// Mycelium dashboard. Plain JS, no build step.
//
// Pages are <section data-page> blocks in admin.html, routed by the URL hash
// (#/plugins/<id>/log). Every dynamic piece of HTML goes through the html``
// tag below, which escapes each interpolated value unless it is wrapped in
// raw(): plugin ids, device names and log lines are untrusted text.
// Every visible string goes through t() (i18n.js): the source language is
// Italian, English comes from I18N_EN.

// ─── Helpers ─────────────────────────────────────────────────────────────────
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];

class Raw { constructor(s) { this.s = s; } }
const raw = s => new Raw(String(s));
const esc = s => String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
function part(v) {
    if (v instanceof Raw) return v.s;
    if (Array.isArray(v)) return v.map(part).join('');
    if (v === null || v === undefined || v === false) return '';
    return esc(v);
}
function html(strings, ...vals) {
    let out = strings[0];
    vals.forEach((v, i) => { out += part(v) + strings[i + 1]; });
    return new Raw(out);
}
function mount(el, content) {
    if (typeof el === 'string') el = $(el);
    if (el) {
        el.innerHTML = part(content);
        fixIcons(el);
    }
    return el;
}
const icon = (name, cls = '') => raw(`<svg class="i ${cls}"><use href="#i-${name}"/></svg>`);
// tr() is t() for a translation that carries trusted markup (<b>, <code>).
const tr = (s, vars) => raw(t(s, vars));

function fmtBytes(b) {
    if (!b || b <= 0) return '0 MB';
    if (b >= 1e9) return (b / 1e9).toFixed(1) + ' GB';
    if (b >= 1e6) return Math.round(b / 1e6) + ' MB';
    return Math.max(1, Math.round(b / 1e3)) + ' KB';
}
function fmtMem(b) {
    if (!b || b <= 0) return '0 KB';
    if (b < 1048576) return (b / 1024).toFixed(0) + ' KB';
    if (b < 1073741824) return (b / 1048576).toFixed(1) + ' MB';
    return (b / 1073741824).toFixed(2) + ' GB';
}
function fmtDuration(s) {
    if (!s || s <= 0) return '—';
    const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
    if (d > 0) return t('{d} g {h} h', { d, h });
    if (h > 0) return `${h} h ${m} min`;
    if (m > 0) return `${m} min`;
    return `${Math.floor(s)} s`;
}
function ago(ts) {
    if (!ts) return t('mai');
    const s = Math.max(0, Date.now() / 1000 - ts);
    if (s < 60) return t('adesso');
    if (s < 3600) return t('{n} min fa', { n: Math.floor(s / 60) });
    if (s < 7200) return t('1 ora fa');
    if (s < 86400) return t('{n} ore fa', { n: Math.floor(s / 3600) });
    if (s < 172800) return t('ieri');
    if (s < 30 * 86400) return t('{n} giorni fa', { n: Math.floor(s / 86400) });
    return fmtDate(ts);
}
const fmtDate = ts => ts ? new Date(ts * 1000).toLocaleDateString(locale(), { day: 'numeric', month: 'short', year: 'numeric' }) : '—';
const fmtDateTime = ts => ts ? new Date(ts * 1000).toLocaleString(locale(), { dateStyle: 'short', timeStyle: 'short' }) : '—';
const truthy = v => ['1', 'true', 'yes', 'on'].includes(String(v ?? '').trim().toLowerCase());

// A stable colour per name, for avatars without an image.
function hue(s) {
    let h = 0;
    for (const c of String(s)) h = (h * 31 + c.charCodeAt(0)) >>> 0;
    return h % 360;
}
// Icons that answered 404 once: not asked for again on every refresh.
const missingIcons = new Set();
const initial = name => (String(name || '?').trim()[0] || '?').toUpperCase();
// A person: their picture, or the initial on a colour of their own.
function avatar(name, { cls = '', src = '' } = {}) {
    return html`<span class="avatar ${cls}" style="background:hsl(${hue(name)} 42% 30%)">${src
        ? html`<img src="${src}" alt="" loading="lazy">` : initial(name)}</span>`;
}
// The picture a profile shows: one set here (mycelium:/…) or an image URL a
// client set; built-in client avatars have no image here.
function profilePicture(p) {
    const u = p.avatar_url || '';
    if (u.startsWith('mycelium:/')) return '/' + u.slice('mycelium:/'.length);
    if (/^https?:\/\//.test(u)) return u;
    return '';
}
// A plugin: its icon as a light grey silhouette on a neutral tile (a CSS
// mask, so every icon looks the same whatever colours it was drawn with),
// or its initial when it has none.
function pluginAvatar(pid, name) {
    const url = `/plugin-icon/${encodeURIComponent(pid)}`;
    if (missingIcons.has(url)) return html`<span class="avatar plugin-avatar">${initial(name)}</span>`;
    return html`<span class="avatar plugin-avatar" data-icon="${url}" data-fallback="${initial(name)}"><i class="plugin-glyph" style="--icon:url('${url}')"></i></span>`;
}
// Plugin icons are optional (404 without one): fall back to the initial.
const iconProbes = new Map();
function fixIcons(root) {
    $$('[data-icon]', root).forEach(el => {
        const url = el.dataset.icon;
        if (!iconProbes.has(url)) {
            iconProbes.set(url, new Promise(ok => {
                const img = new Image();
                img.onload = () => ok(true);
                img.onerror = () => ok(false);
                img.src = url;
            }));
        }
        iconProbes.get(url).then(found => {
            if (found) return;
            missingIcons.add(url);
            el.textContent = el.dataset.fallback;
        });
    });
}

// ─── API ─────────────────────────────────────────────────────────────────────
// api() never throws: { ok, status, data, error }. A 401 means the session is
// over: back to the login page.
async function api(path, { method = 'GET', json, body } = {}) {
    const opts = { method, credentials: 'include', headers: {} };
    if (json !== undefined) {
        opts.headers['Content-Type'] = 'application/json';
        opts.body = JSON.stringify(json);
    } else if (body !== undefined) {
        opts.body = body;
    }
    let res;
    try {
        res = await fetch(path, opts);
    } catch {
        return { ok: false, status: 0, data: null, error: t('Server non raggiungibile.') };
    }
    if (res.status === 401) {
        location.href = '/admin/login';
        return new Promise(() => {});
    }
    const text = await res.text().catch(() => '');
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch { /* plain text */ }
    let error = null;
    if (!res.ok) {
        error = (data && (data.detail || data.message || data.error))
            || (text && text.length < 240 && !text.trim().startsWith('<') ? text.trim() : '')
            || t('Errore {status}', { status: res.status });
    }
    return { ok: res.ok, status: res.status, data, error };
}
const post = (path, json) => api(path, { method: 'POST', json });
const saveSettings = values => post('/admin/settings/save', values);

// ─── Toasts ──────────────────────────────────────────────────────────────────
// msg is already translated (or comes from the server).
function toast(msg, type = 'ok') {
    const icons = { ok: 'check', err: 'alert', warn: 'alert', info: 'info' };
    const el = document.createElement('div');
    el.className = `toast ${type}`;
    mount(el, html`${icon(icons[type] || 'info')}<span>${msg}</span>`);
    $('#toasts').appendChild(el);
    setTimeout(() => { el.classList.add('out'); setTimeout(() => el.remove(), 220); }, type === 'err' ? 6500 : 4000);
}

// ─── Dialogs ─────────────────────────────────────────────────────────────────
// dialog() resolves with the form values, or null when cancelled. With
// onSubmit the dialog stays open while it runs; a returned string is shown
// as an error and keeps it open (a wrong password, a name already taken).
// Every text passed in is already translated.
function dialog(o) {
    const dlg = $('#dialog');
    if (dlg.open) dlg.close();
    const token = {};
    dlg._token = token;
    return new Promise(resolve => {
        let settled = false;
        const finish = v => {
            if (settled) return;
            settled = true;
            o.onClose?.(dlg);
            resolve(v);
        };
        const field = f => f.type === 'switch' ? html`
            <label class="row-switch"><span><b>${f.label}</b>${f.hint ? html`<small>${f.hint}</small>` : ''}</span>
                <input type="checkbox" class="switch" name="${f.name}" ${raw(f.value ? 'checked' : '')}></label>` : html`
            <label class="field"><span>${f.label}</span>
                <input class="input ${f.mono ? 'mono' : ''}" name="${f.name}" type="${f.type || 'text'}" value="${f.value ?? ''}"
                    placeholder="${f.placeholder || ''}" autocomplete="${f.autocomplete || 'off'}"
                    ${raw(f.required ? 'required' : '')} ${raw(f.pattern ? `pattern="${esc(f.pattern)}"` : '')}
                    ${raw(f.inputmode ? `inputmode="${esc(f.inputmode)}"` : '')} ${raw(f.maxlength ? `maxlength="${+f.maxlength}"` : '')}>
                ${f.hint ? html`<small>${f.hint}</small>` : ''}
            </label>`;
        mount(dlg, html`<form novalidate>
            <h3>${o.title}</h3>
            ${o.body ? html`<div class="body">${o.body}</div>` : ''}
            ${(o.fields || []).map(field)}
            ${o.after || ''}
            <div class="error" role="alert"></div>
            <div class="form-actions">
                ${o.cancel === false ? '' : html`<button type="button" class="btn btn-ghost" data-cancel>${o.cancel || t('Annulla')}</button>`}
                ${o.confirm === false ? '' : html`<button type="submit" class="btn ${o.danger ? 'btn-danger' : 'btn-primary'}">${o.confirm || t('Conferma')}</button>`}
            </div>
        </form>`);
        const form = $('form', dlg);
        const errEl = $('.error', dlg);
        const submit = $('[type=submit]', dlg);
        form.addEventListener('submit', async e => {
            e.preventDefault();
            errEl.textContent = '';
            const bad = $$('input', form).find(i => !i.checkValidity());
            if (bad) {
                errEl.textContent = o.invalid?.(bad.name) || bad.validationMessage;
                bad.focus();
                return;
            }
            const values = Object.fromEntries(new FormData(form).entries());
            if (o.onSubmit) {
                submit.disabled = true;
                let err;
                try { err = await o.onSubmit(values, dlg); } finally { submit.disabled = false; }
                if (err) { errEl.textContent = err; return; }
            }
            finish(values);
            dlg.close();
        });
        $('[data-cancel]', dlg)?.addEventListener('click', () => { finish(null); dlg.close(); });
        // close() fires its event asynchronously: one queued by a previous
        // dialog must not settle this one.
        const onClose = () => {
            if (dlg._token !== token) return;
            dlg.removeEventListener('close', onClose);
            finish(null);
        };
        dlg.addEventListener('close', onClose);
        dlg.showModal();
        o.onOpen?.(dlg);
        const first = $('input', dlg);
        if (first) first.focus();
        else submit?.focus();
    });
}
const confirmDialog = (title, body, { confirm = t('Conferma'), danger = false } = {}) =>
    dialog({ title, body, confirm, danger }).then(v => v !== null);

// dangerAction asks for the admin password and runs the request inside the
// dialog: a wrong password keeps it open with the server's message.
async function dangerAction({ title, body, confirm, url, done }) {
    let result = null;
    const ok = await dialog({
        title, body, confirm, danger: true,
        fields: [{ name: 'password', label: t('Password amministratore'), type: 'password', required: true, autocomplete: 'current-password' }],
        onSubmit: async v => {
            const r = await post(url, { password: v.password });
            if (!r.ok) return r.error;
            result = r.data || {};
        },
    });
    if (ok) done?.(result);
}

// ─── Forms: fill, read, dirty state ──────────────────────────────────────────
function formValues(form) {
    const out = {};
    $$('input[name], select[name], textarea[name]', form).forEach(el => {
        if (el.type === 'radio') { if (el.checked) out[el.name] = el.value; return; }
        out[el.name] = el.type === 'checkbox' ? el.checked : el.value;
    });
    return out;
}
function fillForm(form, values) {
    $$('input[name], select[name], textarea[name]', form).forEach(el => {
        if (!(el.name in values)) return;
        const v = values[el.name];
        if (el.type === 'checkbox') el.checked = !!v;
        else if (el.type === 'radio') el.checked = el.value === v;
        else if (el.type !== 'password') el.value = v ?? '';
    });
    markClean(form);
}
function markClean(form) {
    form._initial = JSON.stringify(formValues(form));
    form.classList.add('clean');
}
const isDirty = form => !!form._initial && JSON.stringify(formValues(form)) !== form._initial;
const isEditing = el => !!el && (el.contains(document.activeElement) && /^(INPUT|SELECT|TEXTAREA)$/.test(document.activeElement.tagName));
// A form that can be refreshed from the server without losing typing.
const canRefill = form => form && !isDirty(form) && !isEditing(form);
for (const ev of ['input', 'change']) {
    document.addEventListener(ev, e => {
        const form = e.target.closest('form');
        if (form && form._initial) form.classList.toggle('clean', !isDirty(form));
    });
}
document.addEventListener('click', e => {
    const reset = e.target.closest('[data-form-reset]');
    if (!reset) return;
    const form = reset.closest('form');
    if (form?._initial) fillForm(form, JSON.parse(form._initial));
});

// ─── State shared by pages ───────────────────────────────────────────────────
const state = {
    info: null,                // GET /admin/info
    plugins: null,             // GET /admin/plugins/info: pid → data
    egress: [{ name: 'direct', kind: 'direct', enabled: true }],
    devices: null,
    profiles: null,
    downloads: null,
    warm: null,
    sources: {},               // plugin id → where it is installed/updated from
    cpu: [], mem: [],          // resource samples for the overview sparklines
};

async function loadInfo() {
    const r = await api('/admin/info');
    if (!r.ok) return;
    state.info = r.data;
    $('#brand-version').textContent = r.data.version ? 'v' + String(r.data.version).replace(/^v/, '') : '';
    $('#nav-dot-update').hidden = !(r.data.update_available && r.data.latest_version);
}
async function loadPlugins() {
    const r = await api('/admin/plugins/info');
    if (!r.ok) return false;
    state.plugins = r.data || {};
    const n = Object.values(state.plugins).filter(d => pluginState(d).attention).length;
    const badge = $('#nav-badge-plugins');
    badge.hidden = n === 0;
    badge.textContent = n;
    return true;
}
async function loadSources() {
    const r = await api('/admin/lua-plugins/sources');
    if (r.ok) state.sources = r.data?.sources || {};
}
const shortVer = v => (v && /^[0-9a-f]{40,}$/.test(v) ? v.slice(0, 12) : v || '');
const sourceLink = s => (/^[\w.-]+\/[\w.-]+$/.test(s) ? `https://github.com/${s}` : s);
async function loadEgress() {
    const r = await api('/admin/egress');
    if (r.ok) state.egress = r.data?.profiles || state.egress;
    return r.ok;
}
async function loadDevices() {
    const r = await api('/admin/pileus/devices');
    state.devices = r.ok ? (r.data?.devices || []) : null;
}
async function loadProfiles() {
    const r = await api('/admin/pileus/profiles');
    state.profiles = r.ok ? (r.data?.profiles || []) : null;
}
async function loadDownloads() {
    const r = await api('/admin/downloads');
    state.downloads = r.ok ? r.data : null;
}
async function loadWarm() {
    const r = await api('/admin/warm-stats');
    state.warm = r.ok ? r.data : null;
}

// ─── Plugin state model ──────────────────────────────────────────────────────
function pluginState(d) {
    const run = d.run_state || (d.is_active ? 'running' : 'stopped');
    const label = d.runtime_status?.label || '';
    if (run === 'stopped') return { key: 'stopped', text: t('Fermo'), cls: '' };
    if (run === 'waiting') return { key: 'waiting', text: t('Da configurare'), cls: 'warn', attention: true };
    if (label === 'error') return { key: 'error', text: t('Errore'), cls: 'err', attention: true };
    if (label === 'needs_config') return { key: 'needs_config', text: t('Da configurare'), cls: 'warn', attention: true };
    if (label === 'syncing') return { key: 'syncing', text: t('Sincronizzazione'), cls: 'info pulse' };
    return { key: 'running', text: t('Attivo'), cls: 'ok' };
}
const pluginName = (pid, d) => d?.plugin_name || pid;
function missingLabels(d) {
    const byKey = Object.fromEntries((d.fields || []).map(f => [f.key, f.label || f.key]));
    return (d.missing_required || []).map(k => byKey[k] || k);
}

// The egress registry may report a proxy as its sanitised URL: map it back to
// the profile name when possible.
function egressLabel(name) {
    if (!name || name === 'direct') return t('Diretta');
    for (const p of state.egress) {
        if (!p.proxy_url) continue;
        const san = 'proxy-' + p.proxy_url.replace(/^[a-z0-9]+:\/\//i, '').replace(/[.:]/g, '-').replace(/\/$/, '');
        if (san === name) return p.name;
    }
    return name;
}
function egressKind(p) {
    if (p.name === 'direct') return t('IP del server');
    if (p.kind === 'warp') return 'Cloudflare WARP';
    if (p.kind === 'wireproxy') return t('WireGuard · porta {port}', { port: p.port });
    return p.proxy_url || 'Proxy';
}

// ─── Router ──────────────────────────────────────────────────────────────────
const pages = {};
let current = null;
const titles = {
    overview: 'Panoramica', plugins: 'Plugin', plugin: 'Plugin', downloads: 'Download offline',
    profiles: 'Profili', devices: 'Dispositivi', apps: 'App Pileus', network: 'Rete', system: 'Sistema', logs: 'Log',
};

function parseRoute() {
    const parts = location.hash.replace(/^#\/?/, '').split('/').filter(Boolean).map(decodeURIComponent);
    if (parts[0] === 'plugins' && parts[1]) return { name: 'plugin', params: { id: parts[1], tab: parts[2] || '' } };
    const name = parts[0] && pages[parts[0]] ? parts[0] : 'overview';
    return { name, params: {} };
}

function route() {
    const next = parseRoute();
    if (current && current.name === next.name && pages[next.name].update && pages[next.name].update(next.params) !== false) {
        current = next;
        return;
    }
    if (current) pages[current.name].leave?.();
    current = next;
    $$('.page').forEach(p => { p.hidden = p.dataset.page !== next.name; });
    const navName = next.name === 'plugin' ? 'plugins' : next.name;
    $$('.nav a').forEach(a => a.classList.toggle('active', a.dataset.nav === navName));
    const title = t(titles[next.name] || 'Mycelium');
    $('#topbar-title').textContent = title;
    document.title = `${title} · Mycelium`;
    // The tour decides itself whether the mobile menu is open.
    if (!tourState.active) document.body.classList.remove('nav-open');
    window.scrollTo(0, 0);
    pages[next.name].enter?.(next.params);
}
window.addEventListener('hashchange', route);

// ─── Language ────────────────────────────────────────────────────────────────
function paintLangSwitch() {
    $$('[data-lang]').forEach(b => b.classList.toggle('on', b.dataset.lang === getLang()));
}
// setLang re-translates the fixed HTML and renders the current page again.
function setLang(lang) {
    if (!I18N_LANGS.includes(lang)) return;
    storeLang(lang);
    document.documentElement.lang = lang;
    translateStatic();
    paintLangSwitch();
    if (current) {
        pages[current.name].leave?.();
        current = null;
        route();
    }
    if (tourState.active) showTourStep();
}
document.addEventListener('click', e => {
    const b = e.target.closest('[data-lang]');
    if (b) setLang(b.dataset.lang);
});

// ─── Live updates ────────────────────────────────────────────────────────────
// /admin/events names the section whose data changed; the active page reloads
// just that. "*" (sent on connect) means everything.
let liveES = null;
const liveTimers = {};
const liveTopics = ['plugins', 'egress', 'devices', 'profiles', 'downloads'];
function onLive(topic) {
    clearTimeout(liveTimers[topic]);
    liveTimers[topic] = setTimeout(async () => {
        // The plugin badge in the menu is always kept current.
        if (topic === 'plugins') await loadPlugins();
        const page = current && pages[current.name];
        page?.topics?.[topic]?.();
    }, 150);
}
function connectLive() {
    if (liveES || document.hidden) return;
    liveES = new EventSource('/admin/events');
    liveES.onmessage = e => {
        let topic;
        try { topic = JSON.parse(e.data).topic; } catch { return; }
        (topic === '*' ? liveTopics : [topic]).forEach(onLive);
    };
}
function disconnectLive() {
    liveES?.close();
    liveES = null;
}
document.addEventListener('visibilitychange', () => (document.hidden ? disconnectLive() : connectLive()));

// ─── Log viewer (system log and plugin log) ──────────────────────────────────
function lineClass(l) {
    if (/ERROR|❌|💥|panic/i.test(l)) return 'l-err';
    if (/WARN|⚠/.test(l)) return 'l-warn';
    if (/✅|🚀/.test(l)) return 'l-ok';
    if (/\[lua[:\]]/.test(l)) return 'l-lua';
    return '';
}
function createLogView(host, { historyUrl, streamUrl, max = 800, short = false }) {
    mount(host, html`<div class="log-box">
        <div class="log-bar">
            <label class="search">${icon('search')}<input type="search" placeholder="${t('Filtra…')}" data-log="q"></label>
            <div class="segmented" data-log="level">
                <button data-level="all" class="on">${t('Tutto')}</button>
                <button data-level="warn">${t('Avvisi')}</button>
                <button data-level="err">${t('Errori')}</button>
            </div>
            <button class="btn btn-ghost btn-sm" data-log="pause">${icon('pause')}<span>${t('Pausa')}</span></button>
            <button class="btn btn-ghost btn-sm" data-log="clear">${icon('eraser')}<span>${t('Pulisci')}</span></button>
            <span class="log-status" data-log="status">${t('connessione…')}</span>
        </div>
        <pre class="log-out ${short ? 'short' : ''}" data-log="out"></pre>
    </div>`);
    const out = $('[data-log=out]', host);
    const status = $('[data-log=status]', host);
    const pauseBtn = $('[data-log=pause]', host);
    let lines = [], pending = [], q = '', level = 'all', paused = false, es = null, alive = true, retry = null;

    const matches = l => {
        if (level === 'err' && lineClass(l) !== 'l-err') return false;
        if (level === 'warn' && !['l-err', 'l-warn'].includes(lineClass(l))) return false;
        return !q || l.toLowerCase().includes(q);
    };
    const nearBottom = () => out.scrollHeight - out.scrollTop - out.clientHeight < 40;
    const lineEl = l => {
        const d = document.createElement('div');
        d.className = lineClass(l);
        d.textContent = l;
        return d;
    };
    const renderAll = () => {
        const shown = lines.filter(matches);
        out.replaceChildren(...shown.map(lineEl));
        if (!shown.length) {
            const e = document.createElement('div');
            e.className = 'l-empty';
            e.textContent = lines.length ? t('Nessuna riga corrisponde al filtro.') : t('Ancora nessuna riga.');
            out.appendChild(e);
        }
        out.scrollTop = out.scrollHeight;
    };
    const add = l => {
        lines.push(l);
        if (lines.length > max) lines.splice(0, lines.length - max);
        if (!matches(l)) return;
        const stick = nearBottom();
        $('.l-empty', out)?.remove();
        out.appendChild(lineEl(l));
        while (out.childElementCount > max) out.firstChild.remove();
        if (stick) out.scrollTop = out.scrollHeight;
    };
    const setStatus = (cls, text) => { status.className = `log-status ${cls}`; status.textContent = text; };
    const liveStatus = () => setStatus(paused ? 'paused' : 'live', paused ? t('in pausa') : t('in diretta'));
    const connect = () => {
        if (!alive) return;
        es = new EventSource(streamUrl);
        es.onopen = liveStatus;
        es.onmessage = e => {
            const l = e.data;
            if (!l || l.startsWith(':')) return;
            if (paused) pending.push(l); else add(l);
        };
        es.onerror = () => {
            es.close();
            es = null;
            setStatus('down', t('disconnesso, riprovo…'));
            retry = setTimeout(connect, 4000);
        };
    };

    $('[data-log=q]', host).addEventListener('input', e => { q = e.target.value.trim().toLowerCase(); renderAll(); });
    $('[data-log=level]', host).addEventListener('click', e => {
        const b = e.target.closest('[data-level]');
        if (!b) return;
        level = b.dataset.level;
        $$('[data-level]', host).forEach(x => x.classList.toggle('on', x === b));
        renderAll();
    });
    pauseBtn.addEventListener('click', () => {
        paused = !paused;
        mount(pauseBtn, paused ? html`${icon('play')}<span>${t('Riprendi')}</span>` : html`${icon('pause')}<span>${t('Pausa')}</span>`);
        if (!paused) { pending.forEach(add); pending = []; }
        if (es) liveStatus();
    });
    $('[data-log=clear]', host).addEventListener('click', () => { lines = []; pending = []; renderAll(); });

    (async () => {
        if (historyUrl) {
            const r = await api(historyUrl);
            if (r.ok && alive) (r.data?.logs || []).forEach(l => lines.push(l));
        }
        renderAll();
        connect();
    })();

    return {
        destroy() {
            alive = false;
            clearTimeout(retry);
            es?.close();
        },
    };
}

// ─── Overview ────────────────────────────────────────────────────────────────
let resTimer = null;
function sparkline(svg, values, max) {
    if (!svg) return;
    if (values.length < 2) { svg.innerHTML = ''; return; }
    const top = Math.max(max ?? Math.max(...values), 1e-9);
    const pts = values.map((v, i) => [(i / (values.length - 1)) * 120, 30 - (Math.min(v, top) / top) * 28]);
    const line = pts.map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(1)} ${p[1].toFixed(1)}`).join(' ');
    svg.innerHTML = `<path class="area" d="${line} L120 32 L0 32 Z"/><path class="line" d="${line}"/>`;
}
async function pollResources() {
    const r = await api('/admin/core/resources');
    if (r.ok && current?.name === 'overview') {
        const cpu = r.data.cpu_percent || 0, mem = r.data.mem_bytes || 0;
        state.cpu.push(cpu); state.mem.push(mem);
        if (state.cpu.length > 60) { state.cpu.shift(); state.mem.shift(); }
        $('#ov-cpu').textContent = cpu.toFixed(cpu < 10 ? 1 : 0) + '%';
        $('#ov-mem').textContent = fmtMem(mem);
        $('#ov-uptime').textContent = fmtDuration(r.data.uptime_sec);
        sparkline($('#ov-cpu-spark'), state.cpu, Math.max(100, ...state.cpu));
        sparkline($('#ov-mem-spark'), state.mem, Math.max(...state.mem) * 1.15);
    }
    resTimer = setTimeout(pollResources, 5000);
}

function renderOverview() {
    const info = state.info || {};
    $('#ov-version').textContent = info.version ? `mycelium v${String(info.version).replace(/^v/, '')}` : '';

    // Things that want a look, on top.
    const notes = [];
    if (info.update_available && info.latest_version) {
        notes.push(html`<div class="callout info">${icon('sparkle')}<div><b>${t('È disponibile mycelium {v}', { v: info.latest_version })}</b><span>${t('Stai usando la {v}.', { v: info.version })}</span></div><a class="btn btn-secondary btn-sm" href="#/system">${t('Dettagli')}</a></div>`);
    }
    const plugins = Object.entries(state.plugins || {});
    const attention = plugins.filter(([, d]) => pluginState(d).attention);
    if (attention.length) {
        notes.push(html`<div class="callout warn">${icon('alert')}<div><b>${attention.length === 1 ? t('Un plugin ha bisogno di attenzione') : t('{n} plugin hanno bisogno di attenzione', { n: attention.length })}</b>
            <span>${attention.map(([pid, d], i) => html`${i ? ', ' : ''}<a href="#/plugins/${encodeURIComponent(pid)}">${pluginName(pid, d)}</a>`)}</span></div></div>`);
    }
    if (state.downloads?.enabled && state.downloads.ffmpeg === false) {
        notes.push(html`<div class="callout err">${icon('alert')}<div><b>${t('ffmpeg non trovato')}</b><span>${t('I download offline non possono partire.')}</span></div></div>`);
    }
    mount('#ov-attention', notes);

    // Plugins.
    if (state.plugins) {
        const count = k => plugins.filter(([, d]) => pluginState(d).key === k).length;
        const running = plugins.filter(([, d]) => ['running', 'syncing'].includes(pluginState(d).key)).length;
        const rest = [
            [count('waiting') + count('needs_config'), '{n} da configurare'],
            [count('error'), '{n} con errori'],
            [count('stopped'), '{n} fermi'],
        ].filter(([n]) => n > 0).map(([n, s]) => t(s, { n })).join(' · ');
        mount('#ov-plugins', plugins.length
            ? html`<span class="ov-big">${t('{n} di {total} attivi', { n: running, total: plugins.length })}</span><span>${rest || t('Tutto in ordine.')}</span>`
            : html`<span class="ov-big">${t('Nessun plugin')}</span><span>${t('Installane uno per iniziare.')}</span>`);
    }

    // Devices.
    if (state.devices) {
        const active = state.devices.filter(d => !d.revoked);
        const last = [...active].sort((a, b) => (b.last_seen_at || 0) - (a.last_seen_at || 0))[0];
        mount('#ov-devices', active.length
            ? html`<span class="ov-big">${active.length === 1 ? t('1 abbinato') : t('{n} abbinati', { n: active.length })}</span>
                <span>${t('Ultimo attivo: {name}, {when}', { name: last.label || last.device_id, when: ago(last.last_seen_at) })}</span>`
            : html`<span class="ov-big">${t('Nessuno')}</span><span>${t('Abbina la prima TV o il primo telefono.')}</span>`);
    }

    // Downloads.
    const dl = state.downloads;
    if (dl) {
        if (!dl.enabled) {
            mount('#ov-downloads', html`<span class="ov-big">${t('Spenti')}</span><span>${t('Si attivano dalla pagina dei download.')}</span>`);
        } else {
            const rows = dl.downloads || [];
            const busy = rows.filter(x => ['running', 'queued', 'scheduled', 'paused'].includes(x.status)).length;
            const ready = rows.filter(x => x.status === 'completed').length;
            const pct = dl.quota_bytes ? Math.min(100, 100 * dl.used_bytes / dl.quota_bytes) : 0;
            mount('#ov-downloads', html`<span class="ov-big">${fmtBytes(dl.used_bytes)} <span class="muted small">${t('di {total}', { total: fmtBytes(dl.quota_bytes) })}</span></span>
                <div class="progress ${pct > 90 ? 'warn' : ''}"><i style="width:${pct.toFixed(1)}%"></i></div>
                <span>${t('{busy} in lavorazione · {ready} pronti', { busy, ready })}</span>`);
        }
    }

    // Warm resolve.
    const w = state.warm;
    if (state.info && w) {
        const on = info.warm_resolve_enabled !== false;
        const ok = w.warm_completed_ok || 0;
        const hits = w.warm_hit || 0;
        mount('#ov-warm', on
            ? html`<span class="ov-big">${t('{n} Play più rapidi', { n: hits })}</span>
                <span>${ok ? t('{n} estrazioni completate, {pct}% usate dall\'avvio', { n: ok, pct: Math.round(100 * hits / ok) }) : t('Nessuna estrazione completata dall\'avvio')}</span>`
            : html`<span class="ov-big">${t('Spenta')}</span><span>${t('Si attiva dalla pagina Sistema.')}</span>`);
    }
}

pages.overview = {
    async enter() {
        renderOverview();
        clearTimeout(resTimer);
        pollResources();
        await Promise.all([loadInfo(), loadPlugins(), loadDevices(), loadDownloads(), loadWarm()]);
        if (current?.name === 'overview') renderOverview();
    },
    leave() { clearTimeout(resTimer); },
    topics: {
        plugins: renderOverview,
        devices: async () => { await loadDevices(); renderOverview(); },
        downloads: async () => { await loadDownloads(); renderOverview(); },
    },
};

// ─── Plugins: list ───────────────────────────────────────────────────────────
// One column, in three groups: running, needing setup (or failing), off.
const pluginGroups = [
    { key: 'running', title: 'Attivi', match: d => ['running', 'syncing'].includes(pluginState(d).key) },
    { key: 'attention', title: 'Da configurare', match: d => pluginState(d).attention,
      hint: 'Mancano impostazioni obbligatorie o il plugin segnala un errore.' },
    { key: 'stopped', title: 'Spenti', match: d => pluginState(d).key === 'stopped' },
];

function pluginRow(pid, d) {
    const st = pluginState(d);
    const missing = st.key === 'waiting' ? missingLabels(d) : [];
    const detail = missing.length ? t('Manca: {fields}', { fields: missing.join(', ') }) : (d.runtime_status?.detail || '');
    const egress = d.egress && d.egress !== 'direct' ? egressLabel(d.egress) : '';
    const auto = state.sources[pid]?.auto_update;
    return html`<a class="card plugin-row ${st.key === 'stopped' ? 'off' : ''}" href="#/plugins/${encodeURIComponent(pid)}">
        ${pluginAvatar(pid, pluginName(pid, d))}
        <div class="body">
            <div class="top"><span class="name">${pluginName(pid, d)}</span><span class="faint mono small">${pid}</span></div>
            ${d.description ? html`<p class="desc">${d.description}</p>` : ''}
            ${detail ? html`<span class="detail ${st.cls.split(' ')[0]}">${detail}</span>` : ''}
        </div>
        <div class="meta">
            ${(d.capabilities || []).slice(0, 4).map(c => html`<span class="chip">${capLabel(c)}</span>`)}
            ${egress ? html`<span class="chip">${icon('shield')}${egress}</span>` : ''}
            ${auto ? html`<span class="chip" title="${t('Aggiornamento automatico')}">${icon('refresh')}${t('Auto')}</span>` : ''}
        </div>
        <span class="pill ${st.cls}">${st.text}</span>
        ${icon('chevron', 'muted chev')}
    </a>`;
}

function renderPluginList() {
    const host = $('#plugin-list');
    if (!state.plugins) return;
    const entries = Object.entries(state.plugins).sort((a, b) => pluginName(...a).localeCompare(pluginName(...b)));
    if (!entries.length) {
        mount(host, html`<div class="card empty">${icon('plug')}
            <b>${t('Nessun plugin installato')}</b><span>${tr('I plugin aggiungono cataloghi e sorgenti. Installa il primo da un file <code>.zip</code>.')}</span></div>`);
        return;
    }
    mount(host, pluginGroups.map(g => {
        const rows = entries.filter(([, d]) => g.match(d));
        if (!rows.length) return '';
        return html`<section class="plugin-group" data-group="${g.key}">
            <div class="group-head"><h2>${t(g.title)}</h2><span class="count">${rows.length}</span>${g.hint ? html`<span class="faint small">${t(g.hint)}</span>` : ''}</div>
            <div class="plugin-rows">${rows.map(([pid, d]) => pluginRow(pid, d))}</div>
        </section>`;
    }));
}
function capLabel(c) {
    const labels = { search: 'Ricerca', browse: 'Sfoglia', catalog: 'Cataloghi', live: 'Dirette', streams: 'Stream', download: 'Download' };
    return labels[c] ? t(labels[c]) : c;
}

pages.plugins = {
    async enter() {
        renderPluginList();
        await Promise.all([loadPlugins(), loadEgress(), loadSources()]);
        if (current?.name === 'plugins') renderPluginList();
    },
    topics: {
        plugins: async () => { await loadSources(); renderPluginList(); },
        egress: async () => { await loadEgress(); renderPluginList(); },
    },
};

// Install / update from one or more .zip files: button, or drop anywhere on
// a plugin page. One at a time (the server serialises uploads anyway), then
// a single summary.
let installing = false;
async function installPlugins(fileList) {
    const files = [...(fileList || [])];
    if (!files.length || installing) return;
    const zips = files.filter(f => /\.zip$/i.test(f.name));
    if (zips.length < files.length) toast(t('Ignorati {n} file che non sono .zip.', { n: files.length - zips.length }), 'warn');
    if (!zips.length) return;
    installing = true;
    const done = [], failed = [];
    for (const [i, file] of zips.entries()) {
        toast(zips.length > 1
            ? t('Installo {file} ({i} di {n})…', { file: file.name, i: i + 1, n: zips.length })
            : t('Installo {file}…', { file: file.name }), 'info');
        const fd = new FormData();
        fd.append('plugin_file', file);
        const r = await api('/admin/lua-plugins/upload', { method: 'POST', body: fd });
        if (r.ok) done.push({ id: r.data?.id, update: r.data?.update === 'true' });
        else failed.push(`${file.name}: ${r.error || t('Installazione non riuscita.')}`);
    }
    installing = false;
    await loadPlugins();
    failed.forEach(msg => toast(msg, 'err'));
    if (done.length === 1 && !failed.length) {
        toast(done[0].update ? t('Plugin aggiornato.') : t('Plugin installato.'), 'ok');
        if (done[0].id) { location.hash = `#/plugins/${encodeURIComponent(done[0].id)}`; return; }
    } else if (done.length) {
        toast(t('{n} plugin installati o aggiornati.', { n: done.length }), 'ok');
    }
    if (current?.name === 'plugins') renderPluginList();
    else if (done.length) location.hash = '#/plugins';
}
// "Installa plugin": from .zip files or from an address.
async function openInstallDialog() {
    let result = null;
    await dialog({
        title: t('Installa plugin'),
        body: html`<div class="drop" data-inst="files" tabindex="0">${icon('upload')} ${tr('<b>Scegli uno o più file .zip</b> o trascinali qui')}</div>
            <div class="or"><span>${t('oppure da un indirizzo')}</span></div>`,
        fields: [
            { name: 'source', label: t('Repository GitHub o link a un file .zip'), placeholder: 'owner/repo', mono: true,
              hint: t('Da un repository vengono installati i .zip dell\'ultima release, o il ramo principale se non ha release.') },
            { name: 'auto_update', type: 'switch', label: t('Aggiornamento automatico'), value: true,
              hint: t('Ogni 6 ore controlla se c\'è una versione nuova e la installa.') },
        ],
        confirm: t('Installa da indirizzo'),
        onOpen: dlg => {
            const drop = $('[data-inst=files]', dlg);
            const pick = () => { dlg.close(); $('#plugin-file').click(); };
            drop.addEventListener('click', pick);
            drop.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(); } });
            drop.addEventListener('dragover', e => { e.preventDefault(); drop.classList.add('over'); });
            drop.addEventListener('dragleave', () => drop.classList.remove('over'));
            drop.addEventListener('drop', e => {
                e.preventDefault();
                const files = e.dataTransfer.files;
                dlg.close();
                installPlugins(files);
            });
        },
        onSubmit: async v => {
            if (!v.source.trim()) return t('Indica un repository o un link, oppure scegli dei file.');
            const r = await post('/admin/lua-plugins/install-url', { source: v.source.trim(), auto_update: v.auto_update === 'on' });
            if (!r.ok) return r.error;
            result = r.data;
        },
    });
    if (!result) return;
    const done = result.installed || [];
    (result.errors || []).forEach(msg => toast(msg, 'err'));
    await Promise.all([loadPlugins(), loadSources()]);
    if (done.length === 1) {
        toast(done[0].update ? t('Plugin aggiornato.') : t('Plugin installato.'), 'ok');
        location.hash = `#/plugins/${encodeURIComponent(done[0].id)}`;
    } else {
        toast(t('{n} plugin installati o aggiornati.', { n: done.length }), 'ok');
        if (current?.name === 'plugins') renderPluginList(); else location.hash = '#/plugins';
    }
}

$('#plugin-file').addEventListener('change', e => {
    installPlugins(e.target.files);
    e.target.value = '';
});
let dragDepth = 0;
const dropAllowed = () => ['plugins', 'plugin'].includes(current?.name);
window.addEventListener('dragenter', e => {
    if (!dropAllowed() || !e.dataTransfer?.types?.includes('Files') || $('dialog[open]')) return;
    dragDepth++;
    $('#drop-overlay').hidden = false;
});
window.addEventListener('dragleave', () => {
    if (dragDepth && --dragDepth === 0) $('#drop-overlay').hidden = true;
});
window.addEventListener('dragover', e => { if (dropAllowed() && !$('dialog[open]')) e.preventDefault(); });
window.addEventListener('drop', e => {
    if (!dropAllowed() || $('dialog[open]')) return;
    e.preventDefault();
    dragDepth = 0;
    $('#drop-overlay').hidden = true;
    installPlugins(e.dataTransfer.files);
});

// ─── Plugin: detail ──────────────────────────────────────────────────────────
let pv = { id: null, tab: '', log: null };
const tabLabels = { settings: 'Impostazioni', network: 'Rete', tasks: 'Attività', source: 'Aggiornamenti', log: 'Log' };

function pluginTabs(d) {
    return ['settings', 'network', ...((d.tasks || []).length ? ['tasks'] : []), 'source', 'log'];
}

function renderPluginHead() {
    const d = state.plugins?.[pv.id];
    if (!d) return;
    const st = pluginState(d);
    const run = d.run_state;
    const main = run === 'running'
        ? html`<button class="btn btn-secondary" data-pa="restart">${icon('refresh')}${t('Riavvia')}</button>
               <button class="btn btn-secondary" data-pa="stop">${icon('stop')}${t('Ferma')}</button>`
        : html`<button class="btn btn-primary" data-pa="start" ${raw(run === 'waiting' ? `disabled title="${esc(t('Compila prima le impostazioni obbligatorie'))}"` : '')}>${icon('play')}${t('Avvia')}</button>`;
    mount('#plugin-head', html`<div class="plugin-head">
        ${pluginAvatar(pv.id, pluginName(pv.id, d))}
        <div class="ph-info">
            <div class="title"><h1>${pluginName(pv.id, d)}</h1><span class="pill ${st.cls}">${st.text}</span></div>
            <span class="faint mono small">${pv.id}</span>
            ${d.description ? html`<p class="muted">${d.description}</p>` : ''}
        </div>
        <div class="actions">
            ${main}
            <button class="icon-btn" data-pa="menu" aria-label="${t('Altre azioni')}" aria-haspopup="menu">${icon('more')}</button>
            <div class="menu" id="plugin-menu" hidden role="menu">
                <button data-pa="code">${icon('code')}${t('Modifica il codice')}</button>
                <button data-pa="wipe">${icon('eraser')}${t('Cancella i dati raccolti')}</button>
                <hr>
                <button data-pa="uninstall" class="danger">${icon('trash')}${t('Disinstalla')}</button>
            </div>
        </div>
    </div>`);

    const notes = [];
    const rs = d.runtime_status || {};
    const pidUrl = encodeURIComponent(pv.id);
    if (run === 'waiting') {
        notes.push(html`<div class="callout warn">${icon('alert')}<div><b>${t('Mancano delle impostazioni obbligatorie')}</b>
            <span>${t('{fields}. Compilale e salva: poi potrai avviare il plugin.', { fields: missingLabels(d).join(', ') })}</span></div>
            ${pv.tab !== 'settings' ? html`<a class="btn btn-secondary btn-sm" href="#/plugins/${pidUrl}/settings">${t('Compila')}</a>` : ''}</div>`);
    } else if (st.key === 'error' || st.key === 'needs_config') {
        notes.push(html`<div class="callout ${st.key === 'error' ? 'err' : 'warn'}">${icon('alert')}<div><b>${st.key === 'error' ? t('Il plugin segnala un errore') : t('Il plugin chiede una configurazione')}</b>
            ${rs.detail ? html`<span>${rs.detail}</span>` : ''}</div>
            ${pv.tab !== 'log' ? html`<a class="btn btn-secondary btn-sm" href="#/plugins/${pidUrl}/log">${t('Apri il log')}</a>` : ''}</div>`);
    } else if (rs.detail && st.key !== 'stopped') {
        notes.push(html`<div class="callout info">${icon('info')}<div><span>${rs.detail}</span></div></div>`);
    }
    if (d.discarded_states > 0 && run === 'running') {
        notes.push(html`<div class="callout warn">${icon('clock')}<div><b>${d.discarded_states === 1 ? t('1 chiamata interrotta per tempo scaduto dall\'avvio') : t('{n} chiamate interrotte per tempo scaduto dall\'avvio', { n: d.discarded_states })}</b>
            <span>${t('Se il numero continua a crescere, una sorgente o un\'attività del plugin si blocca: controlla il log.')}</span></div></div>`);
    }
    mount('#plugin-callouts', notes);

    const tabs = pluginTabs(d);
    const counts = { tasks: (d.tasks || []).length, settings: (d.fields || []).length };
    $$('#plugin-tabs a').forEach(a => {
        const tab = a.dataset.tab;
        a.hidden = !tabs.includes(tab);
        a.href = `#/plugins/${pidUrl}/${tab}`;
        a.classList.toggle('on', tab === pv.tab);
        mount(a, counts[tab] ? html`${t(tabLabels[tab])}<em>${counts[tab]}</em>` : t(tabLabels[tab]));
    });
}

function renderPluginPanel(force = false) {
    const d = state.plugins?.[pv.id];
    if (!d) return;
    const host = $('#plugin-panel');
    if (pv.tab !== 'log' && pv.log) { pv.log.destroy(); pv.log = null; }

    if (pv.tab === 'settings') {
        const form = $('form[data-form=plugin-settings]', host);
        if (!force && form && !canRefill(form)) return;
        const fields = d.fields || [];
        if (!fields.length) {
            mount(host, html`<div class="card empty">${icon('check')}<span>${t('Questo plugin non ha impostazioni.')}</span></div>`);
            return;
        }
        mount(host, html`<form class="card" data-form="plugin-settings">
            ${fields.map(f => {
                const label = f.label || f.key;
                if (f.type === 'bool') {
                    return html`<label class="row-switch"><span><b>${label}</b></span>
                        <input type="checkbox" class="switch" name="${f.key}" ${raw(truthy(f.current_value) ? 'checked' : '')}></label>`;
                }
                const pw = f.type === 'password';
                return html`<label class="field"><span>${label}${f.required ? html`<span class="req">*</span>` : ''}</span>
                    <input class="input" name="${f.key}" type="${pw ? 'password' : f.type === 'number' ? 'number' : 'text'}"
                        value="${pw ? '' : (f.current_value || '')}" autocomplete="${pw ? 'new-password' : 'off'}"
                        placeholder="${pw && f.is_set ? t('Impostata · lascia vuoto per non cambiarla') : ''}">
                    ${pw && !f.is_set ? html`<small>${t('Non ancora impostata.')}</small>` : ''}
                </label>`;
            })}
            <div class="form-actions"><button type="button" class="btn btn-ghost" data-form-reset>${t('Annulla')}</button><button type="submit" class="btn btn-primary">${t('Salva')}</button></div>
        </form>`);
        markClean($('form', host));
    } else if (pv.tab === 'network') {
        const form = $('form[data-form=plugin-egress]', host);
        if (!force && form && !canRefill(form)) return;
        const cur = d.egress || 'direct';
        mount(host, html`<form class="card" data-form="plugin-egress">
            <div class="card-h"><div><h2>${t('Uscita di rete')}</h2><p class="muted">${tr('Da dove passa il flusso video di questo plugin. Le uscite si aggiungono e si gestiscono nella pagina <a href="#/network">Rete</a>.')}</p></div></div>
            <div class="list">${state.egress.map(p => {
                const off = !p.enabled && p.name !== 'direct';
                return html`<label class="list-row" style="${off ? 'opacity:.5' : 'cursor:pointer'}">
                    <input type="radio" name="egress" value="${p.name}" ${raw(p.name === cur ? 'checked' : '')} ${raw(off ? 'disabled' : '')} style="accent-color:var(--accent);width:17px;height:17px">
                    <span class="list-main"><span class="list-title">${egressLabel(p.name)}</span><span class="list-sub">${egressKind(p)}${off ? ' · ' + t('disattivata') : ''}</span></span>
                </label>`;
            })}</div>
            <div class="form-actions"><button type="button" class="btn btn-ghost" data-form-reset>${t('Annulla')}</button><button type="submit" class="btn btn-primary">${t('Salva')}</button></div>
        </form>`);
        markClean($('form', host));
    } else if (pv.tab === 'tasks') {
        mount(host, html`<div class="card"><div class="card-h"><div><h2>${t('Attività')}</h2><p class="muted">${t('Lavori che il plugin esegue da solo a intervalli regolari. Puoi lanciarli subito, per esempio per aggiornare un catalogo.')}</p></div></div>
            <div class="list">${(d.tasks || []).map(task => html`<div class="list-row task-row">
                <span class="list-main"><span class="list-title mono">${task.function}</span><span class="list-sub">${humanCron(task.cron)}${task.cron ? html` <span class="cron">${task.cron}</span>` : ''}</span></span>
                <button class="btn btn-secondary btn-sm" data-pa="run-task" data-task="${task.function}" ${raw(d.run_state !== 'running' ? `disabled title="${esc(t('Il plugin è fermo'))}"` : '')}>${icon('play')}${t('Esegui ora')}</button>
            </div>`)}</div></div>`);
    } else if (pv.tab === 'source') {
        const form = $('form[data-form=plugin-source]', host);
        if (!force && form && !canRefill(form)) return;
        const src = state.sources[pv.id];
        if (src) {
            mount(host, html`<div class="card">
                <div class="card-h"><div><h2>${t('Origine')}</h2><p class="muted">${t('Da qui il plugin si aggiorna con l\'ultima versione pubblicata.')}</p></div></div>
                <dl class="kv">
                    <dt>${t('Indirizzo')}</dt><dd><a class="mono" href="${sourceLink(src.source)}" target="_blank" rel="noopener">${src.source}</a></dd>
                    ${src.asset ? html`<dt>${t('Archivio')}</dt><dd class="mono">${src.asset}</dd>` : ''}
                    <dt>${t('Versione installata')}</dt><dd class="mono">${shortVer(src.version) || '—'}</dd>
                    <dt>${t('Ultimo controllo')}</dt><dd>${src.checked_at ? ago(src.checked_at) : t('mai')}</dd>
                    <dt>${t('Ultimo aggiornamento')}</dt><dd>${src.updated_at ? ago(src.updated_at) : '—'}</dd>
                </dl>
                ${src.last_error ? html`<div class="callout err">${icon('alert')}<div><b>${t('L\'ultimo controllo non è riuscito')}</b><span>${src.last_error}</span></div></div>` : ''}
                <label class="row-switch"><span><b>${t('Aggiornamento automatico')}</b><small>${t('Ogni 6 ore controlla se c\'è una versione nuova e la installa.')}</small></span>
                    <input type="checkbox" class="switch" data-src="auto" ${raw(src.auto_update ? 'checked' : '')}></label>
                <div class="form-actions start">
                    <button class="btn btn-primary" data-src="check">${icon('refresh')}${t('Controlla ora')}</button>
                    <button class="btn btn-ghost danger-text" data-src="unlink">${t('Scollega')}</button>
                </div>
            </div>`);
        } else {
            mount(host, html`<form class="card" data-form="plugin-source">
                <div class="card-h"><div><h2>${t('Origine')}</h2><p class="muted">${t('Questo plugin è stato installato da un file. Collegalo al suo repository o al link del suo .zip per aggiornarlo da lì.')}</p></div></div>
                <label class="field"><span>${t('Repository GitHub o link a un file .zip')}</span><input class="input mono" name="source" placeholder="owner/repo" autocomplete="off"></label>
                <label class="row-switch"><span><b>${t('Aggiornamento automatico')}</b><small>${t('Ogni 6 ore controlla se c\'è una versione nuova e la installa.')}</small></span>
                    <input type="checkbox" class="switch" name="auto_update" checked></label>
                <div class="form-actions"><button type="submit" class="btn btn-primary">${icon('link')}${t('Collega e aggiorna')}</button></div>
            </form>`);
            markClean($('form', host));
        }
    } else if (pv.tab === 'log') {
        if (pv.log && !force) return;
        pv.log?.destroy();
        const pid = encodeURIComponent(pv.id);
        pv.log = createLogView(host, {
            historyUrl: `/admin/lua-plugins/logs?plugin_id=${pid}`,
            streamUrl: `/admin/lua-plugins/logs/stream?plugin_id=${pid}`,
            max: 600, short: true,
        });
    }
}
function humanCron(c) {
    if (!c) return t('Solo manuale');
    const m = /^@every\s+(\d+)(s|m|h)$/.exec(c.trim());
    if (m) {
        const n = +m[1];
        const one = { s: 'Ogni secondo', m: 'Ogni minuto', h: 'Ogni ora' }[m[2]];
        const many = { s: 'Ogni {n} secondi', m: 'Ogni {n} minuti', h: 'Ogni {n} ore' }[m[2]];
        return n === 1 ? t(one) : t(many, { n });
    }
    const named = { '@hourly': 'Ogni ora', '@daily': 'Ogni giorno', '@midnight': 'Ogni notte', '@weekly': 'Ogni settimana' }[c.trim()];
    return named ? t(named) : t('Pianificata:');
}

async function showPlugin(params, fresh) {
    const changed = params.id !== pv.id;
    pv.id = params.id;
    if (changed) { pv.log?.destroy(); pv.log = null; mount('#plugin-panel', ''); }
    if (fresh || !state.plugins) await Promise.all([loadPlugins(), loadEgress(), loadSources()]);
    if (current?.name !== 'plugin' || pv.id !== params.id) return;
    const d = state.plugins?.[pv.id];
    if (!d) {
        toast(t('Plugin non trovato.'), 'err');
        location.hash = '#/plugins';
        return;
    }
    pv.tab = pluginTabs(d).includes(params.tab) ? params.tab : 'settings';
    renderPluginHead();
    renderPluginPanel(true);
}

pages.plugin = {
    enter(params) { showPlugin(params, true); },
    update(params) {
        if (params.id !== pv.id) return false;
        showPlugin(params, false);
    },
    leave() {
        pv.log?.destroy();
        pv = { id: null, tab: '', log: null };
        closeMenu();
    },
    topics: {
        plugins: async () => {
            if (!state.plugins?.[pv.id]) {
                toast(t('Il plugin non è più installato.'), 'info');
                location.hash = '#/plugins';
                return;
            }
            await loadSources();
            renderPluginHead();
            if (pv.tab !== 'log') renderPluginPanel();
        },
        egress: async () => {
            await loadEgress();
            if (pv.tab === 'network') renderPluginPanel();
        },
    },
};

function closeMenu() {
    const m = $('#plugin-menu');
    if (m) m.hidden = true;
}
document.addEventListener('click', e => {
    if (!e.target.closest('[data-pa=menu]') && !e.target.closest('#plugin-menu')) closeMenu();
});

async function pluginLifecycle(action) {
    const name = pluginName(pv.id, state.plugins?.[pv.id]);
    if (action !== 'start') {
        const ok = await confirmDialog(
            action === 'stop' ? t('Fermare {name}?', { name }) : t('Riavviare {name}?', { name }),
            t('Se qualcuno sta guardando qualcosa da questo plugin, la riproduzione si interrompe.'),
            { confirm: action === 'stop' ? t('Ferma') : t('Riavvia') });
        if (!ok) return;
    }
    const r = await post(`/admin/lua-plugins/state/${encodeURIComponent(pv.id)}`, { action });
    if (!r.ok || r.data?.ok === false) { toast(r.data?.message || r.error || t('Operazione non riuscita.'), 'err'); return; }
    toast(t({ start: 'Plugin avviato.', stop: 'Plugin fermato.', restart: 'Plugin riavviato.' }[action]), 'ok');
    setTimeout(async () => { await loadPlugins(); if (current?.name === 'plugin') renderPluginHead(); }, action === 'restart' ? 900 : 300);
}

$('section[data-page=plugin]').addEventListener('click', async e => {
    const b = e.target.closest('[data-pa]');
    if (!b || b.disabled) return;
    const pid = pv.id;
    const name = pluginName(pid, state.plugins?.[pid]);
    switch (b.dataset.pa) {
        case 'menu': { const m = $('#plugin-menu'); m.hidden = !m.hidden; break; }
        case 'start': case 'stop': case 'restart': pluginLifecycle(b.dataset.pa); break;
        case 'code': closeMenu(); openEditor(pid, name); break;
        case 'wipe':
            closeMenu();
            dangerAction({
                title: t('Cancellare i dati di {name}?', { name }),
                body: t('Cache e file raccolti dal plugin vengono cancellati. Il plugin resta installato e configurato: li ricostruirà alla prossima sincronizzazione.'),
                confirm: t('Cancella i dati'), url: `/admin/lua-plugins/wipe-data/${encodeURIComponent(pid)}`,
                done: d => toast(t('Dati cancellati: {keys} chiavi, {files} file.', { keys: d.redis_keys ?? 0, files: d.cache_files ?? 0 })
                    + (d.redis_error ? ' ' + t('Errore Redis: {err}', { err: d.redis_error }) : ''), d.redis_error ? 'warn' : 'ok'),
            });
            break;
        case 'uninstall': {
            closeMenu();
            const ok = await confirmDialog(t('Disinstallare {name}?', { name }), t('Il plugin e i suoi file vengono rimossi dal server. Per riaverlo dovrai installarlo di nuovo.'), { confirm: t('Disinstalla'), danger: true });
            if (!ok) return;
            pv.log?.destroy();
            pv.log = null;
            const r = await api('/admin/lua-plugins/uninstall', { method: 'DELETE', json: { plugin_id: pid } });
            if (!r.ok) { toast(r.error || t('Disinstallazione non riuscita.'), 'err'); return; }
            toast(t('{name} disinstallato.', { name }), 'ok');
            await loadPlugins();
            location.hash = '#/plugins';
            break;
        }
        case 'run-task': {
            const r = await post(`/admin/lua-plugins/run-task/${encodeURIComponent(pid)}/${encodeURIComponent(b.dataset.task)}`);
            if (r.ok) toast(t('Attività {task} avviata: segui il log per vedere come va.', { task: b.dataset.task }), 'info');
            else toast(r.data?.message || r.error || t('Attività non avviata.'), 'warn');
            break;
        }
    }
});

async function checkSourceNow(btn) {
    if (btn) btn.disabled = true;
    const r = await post(`/admin/lua-plugins/sources/${encodeURIComponent(pv.id)}/check`);
    if (btn) btn.disabled = false;
    await Promise.all([loadPlugins(), loadSources()]);
    if (current?.name === 'plugin') { renderPluginHead(); renderPluginPanel(true); }
    if (!r.ok) { toast(r.error || t('Controllo non riuscito.'), 'err'); return; }
    toast(r.data?.updated ? t('Aggiornato all\'ultima versione.') : t('È già l\'ultima versione.'), r.data?.updated ? 'ok' : 'info');
}
$('section[data-page=plugin]').addEventListener('click', async e => {
    const b = e.target.closest('[data-src]');
    if (!b || b.dataset.src === 'auto') return;
    if (b.dataset.src === 'check') checkSourceNow(b);
    if (b.dataset.src === 'unlink') {
        if (!(await confirmDialog(t('Scollegare il plugin dalla sua origine?'), t('Resta installato com\'è, ma non si aggiornerà più da lì.'), { confirm: t('Scollega') }))) return;
        const r = await api(`/admin/lua-plugins/sources/${encodeURIComponent(pv.id)}`, { method: 'DELETE' });
        if (!r.ok) { toast(r.error || t('Operazione non riuscita.'), 'err'); return; }
        await loadSources();
        renderPluginPanel(true);
    }
});
$('section[data-page=plugin]').addEventListener('change', async e => {
    const sw = e.target.closest('[data-src=auto]');
    if (!sw) return;
    const r = await post(`/admin/lua-plugins/sources/${encodeURIComponent(pv.id)}`, { auto_update: sw.checked });
    if (!r.ok) { sw.checked = !sw.checked; toast(r.error || t('Operazione non riuscita.'), 'err'); return; }
    await loadSources();
    toast(sw.checked ? t('Aggiornamento automatico attivo.') : t('Aggiornamento automatico spento.'), 'ok');
});

$('section[data-page=plugin]').addEventListener('submit', async e => {
    const form = e.target;
    e.preventDefault();
    const btn = $('[type=submit]', form);
    if (form.dataset.form === 'plugin-source') {
        const v = formValues(form);
        if (!v.source.trim()) { toast(t('Indica un repository o un link.'), 'err'); return; }
        btn.disabled = true;
        const r = await post(`/admin/lua-plugins/sources/${encodeURIComponent(pv.id)}`, { source: v.source.trim(), auto_update: v.auto_update });
        btn.disabled = false;
        if (!r.ok) { toast(r.error || t('Operazione non riuscita.'), 'err'); return; }
        await loadSources();
        markClean(form);
        checkSourceNow(null);
        return;
    }
    if (form.dataset.form === 'plugin-settings') {
        const before = JSON.parse(form._initial || '{}');
        const now = formValues(form);
        const payload = {};
        for (const f of state.plugins[pv.id].fields || []) {
            const k = f.key;
            if (!(k in now)) continue;
            if (f.type === 'bool') { if (now[k] !== before[k]) payload[k] = now[k] ? 'true' : 'false'; }
            else if (f.type === 'password') { if (now[k].trim()) payload[k] = now[k].trim(); }
            else if (now[k].trim() !== String(before[k] ?? '').trim()) payload[k] = now[k].trim();
        }
        if (!Object.keys(payload).length) { toast(t('Nessuna modifica da salvare.'), 'info'); return; }
        btn.disabled = true;
        const r = await post(`/admin/lua-plugins/settings/${encodeURIComponent(pv.id)}`, payload);
        btn.disabled = false;
        if (!r.ok) { toast(r.error || t('Salvataggio non riuscito.'), 'err'); return; }
        toast(t('Impostazioni salvate.'), 'ok');
        await loadPlugins();
        renderPluginHead();
        renderPluginPanel(true);
    } else if (form.dataset.form === 'plugin-egress') {
        const egress = formValues(form).egress;
        btn.disabled = true;
        const r = await post(`/admin/lua-plugins/egress/${encodeURIComponent(pv.id)}`, { egress });
        btn.disabled = false;
        if (!r.ok) { toast(r.error || t('Salvataggio non riuscito.'), 'err'); return; }
        toast(t('Uscita impostata: {name}.', { name: egressLabel(r.data?.egress || egress) }), 'ok');
        await loadPlugins();
        renderPluginHead();
        renderPluginPanel(true);
    }
});

// ─── Code editor ─────────────────────────────────────────────────────────────
const ed = { pid: null, path: null, dirty: false };
async function openEditor(pid, name) {
    Object.assign(ed, { pid, path: null, dirty: false });
    $('#editor-name').textContent = name;
    $('#editor-state').textContent = '';
    $('#editor-text').value = '';
    $('#editor').showModal();
    const r = await api(`/admin/lua-plugins/files/${encodeURIComponent(pid)}`);
    const files = r.ok && Array.isArray(r.data) && r.data.length ? r.data.map(f => f.path) : ['init.lua'];
    mount('#editor-files', files.map(p => html`<li data-path="${p}" title="${p}">${p}</li>`));
    await editorLoad(files.includes('init.lua') ? 'init.lua' : files[0]);
}
async function editorLoad(path) {
    if (ed.dirty && path !== ed.path && !editorDiscard()) return;
    const r = await api(`/admin/lua-plugins/file/${encodeURIComponent(ed.pid)}?path=${encodeURIComponent(path)}`);
    $('#editor-text').value = r.ok ? (r.data?.content ?? '') : `-- ${t('Impossibile aprire {path}: {err}', { path, err: r.error })}`;
    Object.assign(ed, { path, dirty: false });
    $$('#editor-files li').forEach(li => li.classList.toggle('on', li.dataset.path === path));
    $('#editor-state').textContent = path;
}
// The editor is itself a modal: ask in place rather than stacking dialogs.
const editorDiscard = () => window.confirm(t('Ci sono modifiche non salvate. Scartarle?'));
async function editorSave() {
    if (!ed.pid || !ed.path) return;
    const st = $('#editor-state');
    st.textContent = `${ed.path} · ${t('salvataggio…')}`;
    const r = await api(`/admin/lua-plugins/file/${encodeURIComponent(ed.pid)}?path=${encodeURIComponent(ed.path)}`,
        { method: 'PUT', json: { content: $('#editor-text').value } });
    if (!r.ok) { st.textContent = `${ed.path} · ${t('non salvato')}`; toast(r.error || t('Salvataggio non riuscito.'), 'err'); return; }
    ed.dirty = false;
    st.textContent = `${ed.path} · ${t('salvato, il plugin si sta ricaricando')}`;
    toast(t('File salvato.'), 'ok');
}
function editorClose() {
    if (ed.dirty && !editorDiscard()) return;
    ed.dirty = false;
    $('#editor').close();
}
$('#editor-files').addEventListener('click', e => { const li = e.target.closest('li'); if (li) editorLoad(li.dataset.path); });
$('#editor').addEventListener('click', e => {
    const b = e.target.closest('[data-editor]');
    if (b?.dataset.editor === 'save') editorSave();
    if (b?.dataset.editor === 'close') editorClose();
});
$('#editor').addEventListener('cancel', e => { e.preventDefault(); editorClose(); });
$('#editor-text').addEventListener('input', () => {
    if (!ed.dirty) { ed.dirty = true; $('#editor-state').textContent = `${ed.path} · ${t('modificato')}`; }
});
$('#editor-text').addEventListener('keydown', e => {
    if (e.key === 'Tab') {
        e.preventDefault();
        const ta = e.target;
        ta.setRangeText('    ', ta.selectionStart, ta.selectionEnd, 'end');
        ta.dispatchEvent(new Event('input'));
    }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') { e.preventDefault(); editorSave(); }
});

// ─── Downloads ───────────────────────────────────────────────────────────────
const dlStatus = {
    queued: ['In coda', ''], scheduled: ['Programmato', 'info'], running: ['In corso', 'accent pulse'],
    paused: ['In pausa', 'warn'], completed: ['Pronto', 'ok'], failed: ['Fallito', 'err'], canceled: ['Annullato', ''],
};
function renderDownloads() {
    const d = state.downloads;
    if (!d) { mount('#dl-usage', html`<span class="muted">${t('Impossibile leggere i download.')}</span>`); return; }
    if (!d.enabled) {
        mount('#dl-usage', html`<div class="callout info">${icon('info')}<div><b>${t('I download offline sono spenti')}</b><span>${t('Attivali dalle impostazioni qui sotto.')}</span></div></div>`);
    } else {
        const pct = d.quota_bytes ? Math.min(100, 100 * d.used_bytes / d.quota_bytes) : 0;
        mount('#dl-usage', html`<div class="ov-body">
            <span>${tr('<b>{used}</b> usati di {quota} · ancora {free} disponibili', { used: esc(fmtBytes(d.used_bytes)), quota: esc(fmtBytes(d.quota_bytes)), free: esc(fmtBytes(d.available_bytes)) })}</span>
            <div class="progress ${pct > 90 ? 'warn' : ''}"><i style="width:${pct.toFixed(1)}%"></i></div>
            ${d.ffmpeg === false ? html`<div class="callout err">${icon('alert')}<div><b>${t('ffmpeg non trovato')}</b><span>${t('Senza ffmpeg i download non possono partire.')}</span></div></div>` : ''}
        </div>`);
    }
    const rows = d.downloads || [];
    if (!rows.length) {
        mount('#dl-list', html`<div class="empty">${icon('download')}<span>${t('Nessun download. I profili li chiedono dall\'app Pileus.')}</span></div>`);
        return;
    }
    mount('#dl-list', rows.map(x => {
        const name = x.series_title
            ? `${x.series_title}${x.season_number ? ` · S${x.season_number}E${x.episode_number}` : ''} — ${x.title}`
            : (x.title || x.download_id);
        const [label, cls] = dlStatus[x.status] || [x.status, ''];
        const extra = [];
        if (x.status === 'scheduled' && x.scheduled_for) extra.push(t('alle {when}', { when: fmtDateTime(x.scheduled_for) }));
        if (x.shared) extra.push(t('condiviso'));
        if (x.upgrade_pending) extra.push(t('cerco una qualità migliore'));
        if (x.fetched) extra.push(t('già sul dispositivo'));
        const size = x.status === 'completed' ? fmtBytes(x.file_bytes) : `~${fmtBytes(x.estimated_bytes)}`;
        const prog = Math.round((x.progress || 0) * 100);
        return html`<div class="list-row">
            <span class="list-main">
                <span class="list-title">${name}</span>
                <span class="list-sub"><span>${x.plugin_id}</span>${x.quality_label ? html`<span>${x.quality_label}</span>` : ''}<span>${size}</span>${extra.map(s => html`<span>${s}</span>`)}</span>
                ${x.status === 'running' ? html`<div class="progress"><i style="width:${prog}%"></i></div>` : ''}
                ${x.error ? html`<span class="small danger-text">${x.error}</span>` : ''}
            </span>
            <span class="pill ${cls}">${t(label)}${x.status === 'running' ? ` ${prog}%` : ''}</span>
            <button class="icon-btn" data-dl-del="${x.download_id}" title="${t('Elimina')}" aria-label="${t('Elimina')}">${icon('trash')}</button>
        </div>`;
    }));
}
function fillDownloadSettings() {
    const form = $('form[data-form=downloads]');
    const i = state.info;
    if (!i || !canRefill(form)) return;
    fillForm(form, {
        download_enabled: i.download_enabled !== false,
        download_quota_gb: i.download_quota_gb ?? 20,
        download_min_free_gb: i.download_min_free_gb ?? 3,
        download_retention_days: i.download_retention_days ?? 7,
        download_max_mbps: i.download_max_mbps ?? 0,
        download_pace: i.download_pace ?? 2,
        download_pause_while_streaming: i.download_pause_while_streaming !== false,
        download_delete_after_fetch: !!i.download_delete_after_fetch,
    });
}
pages.downloads = {
    async enter() {
        await Promise.all([loadDownloads(), loadInfo()]);
        renderDownloads();
        fillDownloadSettings();
    },
    topics: { downloads: async () => { await loadDownloads(); renderDownloads(); } },
};
$('#dl-list').addEventListener('click', async e => {
    const b = e.target.closest('[data-dl-del]');
    if (!b) return;
    const ok = await confirmDialog(t('Eliminare questo download?'), t('Il file sparisce dal server. I dispositivi che l\'hanno già scaricato lo tengono.'), { confirm: t('Elimina'), danger: true });
    if (!ok) return;
    const r = await api(`/admin/downloads/${encodeURIComponent(b.dataset.dlDel)}`, { method: 'DELETE' });
    if (!r.ok) { toast(r.error || t('Eliminazione non riuscita.'), 'err'); return; }
    toast(t('Download eliminato.'), 'ok');
    await loadDownloads();
    renderDownloads();
});
$('form[data-form=downloads]').addEventListener('submit', async e => {
    e.preventDefault();
    const v = formValues(e.target);
    const num = (x, def) => { const n = parseFloat(x); return String(isNaN(n) || n < 0 ? def : n); };
    const r = await saveSettings({
        download_enabled: v.download_enabled ? '1' : '0',
        download_quota_gb: num(v.download_quota_gb, 20),
        download_min_free_gb: num(v.download_min_free_gb, 3),
        download_retention_days: num(v.download_retention_days, 7),
        download_max_mbps: num(v.download_max_mbps, 0),
        download_pace: num(v.download_pace, 2),
        download_pause_while_streaming: v.download_pause_while_streaming ? '1' : '0',
        download_delete_after_fetch: v.download_delete_after_fetch ? '1' : '0',
    });
    if (!r.ok) { toast(r.error || t('Salvataggio non riuscito.'), 'err'); return; }
    toast(t('Impostazioni dei download salvate.'), 'ok');
    markClean(e.target);
    await Promise.all([loadInfo(), loadDownloads()]);
    renderDownloads();
    fillDownloadSettings();
});

// ─── Profiles ────────────────────────────────────────────────────────────────
function renderProfiles() {
    const list = state.profiles;
    if (!list) { mount('#profile-list', html`<div class="card empty">${t('Impossibile leggere i profili.')}</div>`); return; }
    if (!list.length) {
        mount('#profile-list', html`<div class="card empty" style="grid-column:1/-1">${icon('users')}<b>${t('Nessun profilo')}</b>
            <span>${t('Crea un profilo per ogni persona che usa Pileus.')}</span>
            <button class="btn btn-primary" data-action="profile-create">${icon('plus')}${t('Nuovo profilo')}</button></div>`);
        return;
    }
    mount('#profile-list', list.map(p => html`<div class="card profile-card">
        <div class="top">
            <span class="avatar-wrap">${avatar(p.name, { src: profilePicture(p) })}${(p.avatar_url || '').startsWith('mycelium:/')
                ? html`<button class="avatar-x" data-pf="unphoto" data-id="${p.profile_id}" title="${t('Togli foto')}" aria-label="${t('Togli foto')}">${icon('x')}</button>` : ''}</span>
            <span class="name">${p.name}</span>
            ${p.pin_protected ? html`<span class="pill accent plain">${icon('lock')}PIN</span>` : ''}
        </div>
        ${p.pin_protected ? html`<div class="ov-body small">
            <span>${(p.trusted_devices || []).length ? t('Dispositivi che non chiedono il PIN:') : t('Ogni dispositivo chiede il PIN.')}</span>
            <div class="trusted">${(p.trusted_devices || []).map(d => html`<span class="chip">${d.label || String(d.device_id).slice(0, 8)}
                <button data-pf="untrust" data-id="${p.profile_id}" data-device="${d.device_id}" title="${t('Chiedi di nuovo il PIN su questo dispositivo')}" aria-label="${t('Revoca')}">✕</button></span>`)}</div>
        </div>` : html`<span class="muted small">${t('Senza PIN: chiunque abbia un dispositivo abbinato può usarlo.')}</span>`}
        <div class="bottom">
            <button class="btn btn-secondary btn-sm" data-pf="rename" data-id="${p.profile_id}">${icon('edit')}${t('Rinomina')}</button>
            <button class="btn btn-secondary btn-sm" data-pf="photo" data-id="${p.profile_id}">${icon('image')}${t('Foto')}</button>
            <button class="btn btn-secondary btn-sm" data-pf="pin" data-id="${p.profile_id}">${icon('lock')}${p.pin_protected ? t('Cambia PIN') : t('Imposta PIN')}</button>
            ${p.pin_protected ? html`<button class="btn btn-ghost btn-sm" data-pf="unpin" data-id="${p.profile_id}">${t('Togli PIN')}</button>` : ''}
            <button class="btn btn-ghost btn-sm danger-text" data-pf="delete" data-id="${p.profile_id}" style="margin-left:auto" title="${t('Elimina')}" aria-label="${t('Elimina')}">${icon('trash')}</button>
        </div>
    </div>`));
}
pages.profiles = {
    async enter() { await loadProfiles(); renderProfiles(); },
    topics: { profiles: async () => { await loadProfiles(); renderProfiles(); } },
};
async function refreshProfiles() { await loadProfiles(); if (current?.name === 'profiles') renderProfiles(); }
async function createProfile() {
    await dialog({
        title: t('Nuovo profilo'), confirm: t('Crea'),
        body: t('Sarà disponibile subito su tutti i dispositivi abbinati.'),
        fields: [{ name: 'name', label: t('Nome'), required: true, maxlength: 40, placeholder: t('es. Giulia') }],
        onSubmit: async v => {
            if (!v.name.trim()) return t('Serve un nome.');
            const r = await post('/admin/pileus/profiles', { name: v.name.trim(), avatar_url: '' });
            if (!r.ok) return r.error;
            toast(t('Profilo {name} creato.', { name: v.name.trim() }), 'ok');
        },
    });
    refreshProfiles();
}
$('section[data-page=profiles]').addEventListener('click', async e => {
    const b = e.target.closest('[data-pf]');
    if (!b) return;
    const p = (state.profiles || []).find(x => x.profile_id === b.dataset.id);
    if (!p) return;
    const id = encodeURIComponent(p.profile_id);
    switch (b.dataset.pf) {
        case 'photo': {
            const input = $('#profile-photo');
            input.dataset.id = p.profile_id;
            input.click();
            return;
        }
        case 'unphoto': {
            const r = await api(`/admin/pileus/profiles/${id}/avatar`, { method: 'DELETE' });
            toast(r.ok ? t('Foto rimossa.') : (r.error || t('Operazione non riuscita.')), r.ok ? 'ok' : 'err');
            break;
        }
        case 'rename':
            await dialog({
                title: t('Rinomina profilo'), confirm: t('Salva'),
                fields: [{ name: 'name', label: t('Nome'), value: p.name, required: true, maxlength: 40 }],
                onSubmit: async v => {
                    if (!v.name.trim()) return t('Serve un nome.');
                    const r = await post(`/admin/pileus/profiles/${id}`, { name: v.name.trim(), avatar_url: p.avatar_url || '' });
                    if (!r.ok) return r.error;
                    toast(t('Profilo rinominato.'), 'ok');
                },
            });
            break;
        case 'pin':
            await dialog({
                title: p.pin_protected ? t('Cambia il PIN di {name}', { name: p.name }) : t('Imposta un PIN per {name}', { name: p.name }),
                body: t('Tutti i dispositivi dovranno inserire il nuovo PIN per aprire questo profilo.'),
                confirm: t('Salva PIN'),
                fields: [{ name: 'pin', label: 'PIN', type: 'password', inputmode: 'numeric', pattern: '[0-9]{4,8}', required: true, maxlength: 8, autocomplete: 'new-password', hint: t('Da 4 a 8 cifre.') }],
                invalid: () => t('Il PIN deve essere di 4-8 cifre.'),
                onSubmit: async v => {
                    const r = await post(`/admin/pileus/profiles/${id}/pin`, { pin: v.pin.trim() });
                    if (!r.ok) return r.error;
                    toast(t('PIN impostato.'), 'ok');
                },
            });
            break;
        case 'unpin': {
            if (!(await confirmDialog(t('Togliere il PIN a {name}?', { name: p.name }), t('Il profilo si aprirà senza codice da qualunque dispositivo abbinato.'), { confirm: t('Togli PIN') }))) return;
            const r = await post(`/admin/pileus/profiles/${id}/pin`, { pin: '' });
            toast(r.ok ? t('PIN rimosso.') : (r.error || t('Operazione non riuscita.')), r.ok ? 'ok' : 'err');
            break;
        }
        case 'untrust': {
            if (!(await confirmDialog(t('Chiedere di nuovo il PIN?'), t('Quel dispositivo dovrà reinserire il PIN la prossima volta che apre il profilo.'), { confirm: t('Sì, chiedilo') }))) return;
            const r = await api(`/admin/pileus/profiles/${id}/trust/${encodeURIComponent(b.dataset.device)}`, { method: 'DELETE' });
            toast(r.ok ? t('Fatto.') : (r.error || t('Operazione non riuscita.')), r.ok ? 'ok' : 'err');
            break;
        }
        case 'delete': {
            if (!(await confirmDialog(t('Eliminare il profilo {name}?', { name: p.name }), t('La sua cronologia di visione non sarà più raggiungibile da nessun profilo.'), { confirm: t('Elimina'), danger: true }))) return;
            const r = await api(`/admin/pileus/profiles/${id}`, { method: 'DELETE' });
            toast(r.ok ? t('Profilo eliminato.') : (r.error || t('Eliminazione non riuscita.')), r.ok ? 'ok' : 'err');
            break;
        }
    }
    refreshProfiles();
});

$('#profile-photo').addEventListener('change', async e => {
    const file = e.target.files[0];
    const id = e.target.dataset.id;
    e.target.value = '';
    if (!file || !id) return;
    if (file.size > 10 << 20) { toast(t('Immagine oltre 10 MB.'), 'err'); return; }
    const fd = new FormData();
    fd.append('image', file);
    toast(t('Carico la foto…'), 'info');
    const r = await api(`/admin/pileus/profiles/${encodeURIComponent(id)}/avatar`, { method: 'POST', body: fd });
    toast(r.ok ? t('Foto aggiornata. I dispositivi la vedono al prossimo aggiornamento dei profili.') : (r.error || t('Operazione non riuscita.')), r.ok ? 'ok' : 'err');
    refreshProfiles();
});

// ─── Devices ─────────────────────────────────────────────────────────────────
function renderDevices() {
    const list = state.devices;
    if (!list) { mount('#device-list', html`<div class="empty">${t('Impossibile leggere i dispositivi.')}</div>`); return; }
    if (!list.length) {
        mount('#device-list', html`<div class="empty">${icon('device')}<b>${t('Nessun dispositivo abbinato')}</b>
            <span>${t('Apri Pileus sulla TV o sul telefono e inserisci il codice che ottieni con «Abbina un dispositivo».')}</span></div>`);
        return;
    }
    const sorted = [...list].sort((a, b) => (a.revoked - b.revoked) || ((b.last_seen_at || 0) - (a.last_seen_at || 0)));
    mount('#device-list', sorted.map(d => html`<div class="list-row" style="${d.revoked ? 'opacity:.6' : ''}">
        <span class="avatar" style="background:var(--surface-3);color:var(--muted)">${icon('device')}</span>
        <span class="list-main">
            <span class="list-title">${d.label || d.device_id}</span>
            <span class="list-sub"><span>${t('Attivo {when}', { when: ago(d.last_seen_at) })}</span><span>${t('Abbinato il {date}', { date: fmtDate(d.created_at) })}</span>${d.label ? html`<span class="mono">${String(d.device_id).slice(0, 12)}</span>` : ''}</span>
        </span>
        ${d.revoked ? html`<span class="pill err">${t('Revocato')}</span>` : ''}
        <span class="list-actions">
            <button class="btn btn-ghost btn-sm" data-dev="rename" data-id="${d.device_id}">${t('Rinomina')}</button>
            ${d.revoked
                ? html`<button class="btn btn-secondary btn-sm" data-dev="unrevoke" data-id="${d.device_id}">${t('Ripristina')}</button>`
                : html`<button class="btn btn-ghost btn-sm danger-text" data-dev="revoke" data-id="${d.device_id}">${t('Revoca')}</button>`}
            <button class="icon-btn" data-dev="delete" data-id="${d.device_id}" title="${t('Elimina')}" aria-label="${t('Elimina')}">${icon('trash')}</button>
        </span>
    </div>`));
}
pages.devices = {
    async enter() { await loadDevices(); renderDevices(); },
    topics: { devices: async () => { await loadDevices(); renderDevices(); } },
};
$('#device-list').addEventListener('click', async e => {
    const b = e.target.closest('[data-dev]');
    if (!b) return;
    const d = (state.devices || []).find(x => x.device_id === b.dataset.id);
    if (!d) return;
    const id = encodeURIComponent(d.device_id);
    const name = d.label || d.device_id;
    let r = null;
    switch (b.dataset.dev) {
        case 'rename':
            await dialog({
                title: t('Rinomina dispositivo'), confirm: t('Salva'),
                fields: [{ name: 'label', label: t('Nome'), value: d.label || '', placeholder: t('es. TV del salotto'), maxlength: 60 }],
                onSubmit: async v => {
                    const res = await post(`/admin/pileus/devices/${id}/rename`, { label: v.label.trim() });
                    if (!res.ok) return res.error;
                    toast(t('Dispositivo rinominato.'), 'ok');
                },
            });
            break;
        case 'revoke':
            if (!(await confirmDialog(t('Revocare {name}?', { name }), t('Perde subito l\'accesso al server e dovrà abbinarsi di nuovo con un codice.'), { confirm: t('Revoca'), danger: true }))) return;
            r = await post(`/admin/pileus/devices/${id}/revoke`);
            toast(r.ok ? t('Dispositivo revocato.') : r.error, r.ok ? 'ok' : 'err');
            break;
        case 'unrevoke':
            r = await post(`/admin/pileus/devices/${id}/unrevoke`);
            toast(r.ok ? t('Dispositivo ripristinato.') : r.error, r.ok ? 'ok' : 'err');
            break;
        case 'delete':
            if (!(await confirmDialog(t('Eliminare {name}?', { name }), t('Sparisce dall\'elenco. Se si ripresenta con un codice valido, ricompare. I profili non vengono toccati.'), { confirm: t('Elimina'), danger: true }))) return;
            r = await api(`/admin/pileus/devices/${id}`, { method: 'DELETE' });
            toast(r.ok ? t('Dispositivo eliminato.') : r.error, r.ok ? 'ok' : 'err');
            break;
    }
    await loadDevices();
    if (current?.name === 'devices') renderDevices();
});

// Pairing: a short-lived code the Pileus app types in. Polled every 2 s to
// notice both a device using it and its expiry.
async function pairDevice() {
    let timer = null, expires = 0, pairedID = '';
    const paint = (dlg, view) => mount($('.body', dlg), view);
    // Returns an error to show, or nothing once saved.
    const saveName = async dlg => {
        const input = $('[data-pair=name]', dlg);
        const label = input?.value.trim();
        if (!label) { input?.focus(); return t('Scrivi un nome, oppure scegli «Più tardi».'); }
        const r = await post(`/admin/pileus/devices/${encodeURIComponent(pairedID)}/rename`, { label });
        if (!r.ok) return r.error || t('Operazione non riuscita.');
        toast(t('Dispositivo salvato come {name}.', { name: label }), 'ok');
        loadDevices().then(() => { if (current?.name === 'devices') renderDevices(); });
    };
    const codeView = code => html`
        <span>${t('Su Pileus scegli di collegarti a un server e inserisci questo codice:')}</span>
        <div class="pair-code">${code}</div>
        <div class="pair-meta" data-pair="left">&nbsp;</div>`;
    const generate = async dlg => {
        clearInterval(timer);
        paint(dlg, html`<div class="pair-meta">${t('Genero il codice…')}</div>`);
        const r = await post('/admin/pileus/pairing/token');
        if (!r.ok) { paint(dlg, html`<div class="callout err">${icon('alert')}<div>${r.error}</div></div>`); return; }
        expires = r.data.expires_at * 1000;
        paint(dlg, codeView(r.data.code));
        tick(dlg);
        timer = setInterval(() => poll(dlg), 2000);
    };
    const tick = dlg => {
        const left = Math.max(0, Math.round((expires - Date.now()) / 1000));
        const el = $('[data-pair=left]', dlg);
        if (el) el.textContent = left > 0 ? t('Valido ancora {n} secondi', { n: left }) : t('Scaduto');
    };
    const poll = async dlg => {
        tick(dlg);
        const r = await api('/admin/pileus/pairing/token');
        if (!r.ok || !dlg.open) return;
        if (r.data.used) {
            clearInterval(timer);
            pairedID = r.data.device_id || '';
            paint(dlg, html`<div class="pair-done">${icon('check')}${t('Dispositivo abbinato')}</div>
                ${pairedID ? html`<label class="field"><span>${t('Come vuoi chiamarlo?')}</span>
                    <input class="input" data-pair="name" maxlength="60" placeholder="${t('es. TV del salotto')}" autocomplete="off">
                    <small>${t('Così lo riconosci nell\'elenco invece di vedere un codice.')}</small></label>` : ''}`);
            if (pairedID) {
                // The footer becomes "Più tardi" / "Salva nome".
                const submit = $('[type=submit]', dlg);
                submit.textContent = t('Salva nome');
                const later = document.createElement('button');
                later.type = 'button';
                later.className = 'btn btn-ghost';
                later.dataset.pair = 'later';
                later.textContent = t('Più tardi');
                submit.before(later);
                $('[data-pair=name]', dlg).focus();
            }
            loadDevices().then(() => { if (current?.name === 'devices') renderDevices(); });
        } else if (!r.data.active) {
            clearInterval(timer);
            paint(dlg, html`<div class="pair-meta">${t('Il codice è scaduto.')}</div>
                <button type="button" class="btn btn-secondary" data-pair="again">${icon('refresh')}${t('Nuovo codice')}</button>`);
        }
    };
    await dialog({
        title: t('Abbina un dispositivo'),
        body: html`<div class="pair-meta">…</div>`,
        confirm: t('Chiudi'), cancel: false,
        onOpen: dlg => {
            dlg.addEventListener('click', e => {
                if (e.target.closest('[data-pair=again]')) generate(dlg);
                if (e.target.closest('[data-pair=later]')) dlg.close();
            });
            generate(dlg);
        },
        // Once paired, "Salva nome" (and Enter in the field) saves the name.
        onSubmit: async (_, dlg) => (pairedID && $('[data-pair=name]', dlg) ? saveName(dlg) : undefined),
        onClose: () => clearInterval(timer),
    });
}

// ─── App Pileus ──────────────────────────────────────────────────────────────
function renderApps() {
    const i = state.info || {};
    const link = location.origin + '/app/';
    const a = $('#apps-link');
    if (a) { a.href = link; a.textContent = link; }
    const form = $('form[data-form=webapp]');
    if (canRefill(form)) fillForm(form, { pileus_web_repo: i.pileus_web_repo || '' });
    const ver = $('#webapp-version');
    ver.textContent = i.pileus_web_version
        ? t('Installata {v}', { v: i.pileus_web_version + (i.pileus_web_build ? ` (${i.pileus_web_build})` : '') })
        : t('Non installata');
    ver.className = `pill ${i.pileus_web_version ? 'ok' : ''}`;
    $('#webapp-update-btn').disabled = !i.pileus_web_repo;
    $('#webapp-auto').checked = i.pileus_web_auto_update !== false;
    const st = $('#webapp-status');
    if (!st.dataset.busy) st.textContent = i.pileus_web_repo ? '' : t('Indica il repository per poter aggiornare.');
}
pages.apps = {
    async enter() { renderApps(); await loadInfo(); renderApps(); },
};
$('form[data-form=webapp]').addEventListener('submit', async e => {
    e.preventDefault();
    const r = await saveSettings({ pileus_web_repo: formValues(e.target).pileus_web_repo.trim() });
    if (!r.ok) { toast(r.error || t('Salvataggio non riuscito.'), 'err'); return; }
    toast(t('Repository salvato.'), 'ok');
    markClean(e.target);
    await loadInfo();
    renderApps();
    // A new repository: install its latest release right away.
    if (state.info?.pileus_web_repo) updateWebApp();
});
$('#webapp-auto').addEventListener('change', async e => {
    const on = e.target.checked;
    const r = await saveSettings({ pileus_web_auto_update: on ? '1' : '0' });
    if (!r.ok) { e.target.checked = !on; toast(r.error || t('Salvataggio non riuscito.'), 'err'); return; }
    toast(on ? t('Aggiornamento automatico attivo.') : t('Aggiornamento automatico spento.'), 'ok');
    await loadInfo();
});
async function updateWebApp(force = false) {
    const btn = $('#webapp-update-btn'), st = $('#webapp-status');
    btn.disabled = true;
    st.dataset.busy = '1';
    st.textContent = t('Aggiornamento in corso…');
    const r = await post('/admin/pileus-web/update', { force });
    delete st.dataset.busy;
    btn.disabled = false;
    const d = r.data || {};
    if (!r.ok) { st.textContent = r.error; toast(r.error || t('Aggiornamento non riuscito.'), 'err'); return; }
    if (d.ok === false && d.warning) {
        st.textContent = d.warning;
        if (await confirmDialog(t('Aggiornare comunque?'), d.warning, { confirm: t('Aggiorna comunque') })) return updateWebApp(true);
        return;
    }
    if (!d.ok) { st.textContent = d.detail || t('Aggiornamento non riuscito.'); toast(st.textContent, 'err'); return; }
    st.textContent = t('Aggiornata a {v} ({asset}).', { v: d.version, asset: d.asset })
        + (d.checksum_verified ? ' ' + t('Checksum verificato.') : (d.warning ? ' ' + d.warning : ''));
    toast(t('App web aggiornata a {v}.', { v: d.version }), d.checksum_verified ? 'ok' : 'warn');
    await loadInfo();
    renderApps();
}

// ─── Network ─────────────────────────────────────────────────────────────────
function renderEgress() {
    const users = {};
    Object.entries(state.plugins || {}).forEach(([pid, d]) => {
        const e = d.egress || 'direct';
        (users[e] = users[e] || []).push(pluginName(pid, d));
    });
    mount('#egress-list', state.egress.map(p => {
        const direct = p.name === 'direct';
        const wg = p.kind === 'wireproxy';
        const status = direct ? ['Sempre attiva', 'plain']
            : !p.enabled ? ['Disattivata', '']
            : wg && !p.running ? ['Avvio…', 'warn pulse']
            : ['Attiva', 'ok'];
        const used = users[p.name] || [];
        return html`<div class="list-row">
            <span class="list-main">
                <span class="list-title">${egressLabel(p.name)}</span>
                <span class="list-sub"><span class="${p.proxy_url ? 'mono' : ''}">${egressKind(p)}</span>
                    <span>${used.length ? t('Usata da {list}', { list: used.join(', ') }) : t('Nessun plugin la usa')}</span></span>
            </span>
            <span class="pill ${status[1]}">${t(status[0])}</span>
            ${direct ? '' : html`<span class="list-actions">
                <button class="btn btn-ghost btn-sm" data-eg="test" data-name="${p.name}">${t('Prova')}</button>
                <input type="checkbox" class="switch" data-eg="toggle" data-name="${p.name}" ${raw(p.enabled ? 'checked' : '')} title="${p.enabled ? t('Disattiva') : t('Attiva')}" aria-label="${t('Attiva')}">
                <button class="icon-btn" data-eg="delete" data-name="${p.name}" title="${t('Elimina')}" aria-label="${t('Elimina')}">${icon('trash')}</button>
            </span>`}
        </div>`;
    }));
}
function renderBrowserService() {
    const i = state.info;
    if (!i) return;
    const form = $('form[data-form=browser]');
    if (canRefill(form)) fillForm(form, { browser_service_url: i.browser_service_url || '', browser_service_api_key: '' });
    $('#bs-env').textContent = i.browser_service_url_env
        ? `${i.browser_service_url ? t('Ignorato, prevale questo campo') : t('In uso dall\'ambiente')}: MYCELIUM_BROWSER_URL=${i.browser_service_url_env}` : '';
    $('#bs-key-state').textContent = i.browser_service_key_set ? t('Chiave salvata qui.')
        : i.browser_service_key_env_set ? t('Chiave presa da MYCELIUM_BROWSER_API_KEY.') : t('Nessuna chiave.');
    $('#bs-clear-key').hidden = !i.browser_service_key_set;
    const st = $('#bs-status');
    if (!st.dataset.tested) {
        st.textContent = i.browser_service_active ? t('Configurato') : t('Non configurato');
        st.className = `pill ${i.browser_service_active ? 'info' : ''}`;
    }
    const v6 = $('#egress-ipv6');
    if (typeof i.egress_ipv6 === 'boolean') v6.checked = i.egress_ipv6;
}
async function testBrowserService() {
    const st = $('#bs-status');
    st.dataset.tested = '1';
    st.textContent = t('Verifico…');
    st.className = 'pill';
    const r = await api('/admin/browser-service/health');
    const d = r.data;
    if (!r.ok || !d) { st.textContent = t('Verifica non riuscita'); st.className = 'pill err'; return; }
    if (!d.configured) { st.textContent = t('Non configurato'); st.className = 'pill'; return; }
    if (d.error) { st.textContent = t('Non raggiungibile'); st.className = 'pill err'; toast(t('Servizio browser: {err}', { err: d.error }), 'err'); return; }
    st.textContent = d.ready ? t('Raggiungibile') + (d.engine ? ' · ' + d.engine : '') : t('Raggiungibile, motore non pronto');
    st.className = `pill ${d.ready ? 'ok' : 'warn'}`;
}
pages.network = {
    async enter() {
        renderEgress();
        renderBrowserService();
        await Promise.all([loadEgress(), loadInfo(), state.plugins ? null : loadPlugins()]);
        renderEgress();
        renderBrowserService();
    },
    topics: {
        egress: async () => { await loadEgress(); renderEgress(); },
        plugins: renderEgress,
    },
};
$('#egress-list').addEventListener('click', async e => {
    const b = e.target.closest('[data-eg]');
    if (!b || b.dataset.eg === 'toggle') return;
    const name = b.dataset.name;
    if (b.dataset.eg === 'test') {
        b.disabled = true;
        toast(t('Provo {name}…', { name }), 'info');
        const r = await post(`/admin/egress/${encodeURIComponent(name)}/test`);
        b.disabled = false;
        if (!r.ok) { toast(r.error || t('Prova non riuscita.'), 'err'); return; }
        const d = r.data || {};
        if (d.leaking) toast(t('{name} non instrada: l\'IP di uscita è lo stesso della connessione diretta ({ip}).', { name, ip: d.exit_ip }), 'err');
        else toast(t('{name} funziona: esce da {ip}', { name, ip: d.exit_ip || '?' })
            + (d.exit_loc ? ` (${d.exit_loc})` : '') + (d.warp && d.warp !== 'off' ? ` · warp ${d.warp}` : '') + '.', 'ok');
    } else if (b.dataset.eg === 'delete') {
        if (!(await confirmDialog(t('Eliminare l\'uscita {name}?', { name }), t('I plugin che la usano torneranno all\'uscita diretta.'), { confirm: t('Elimina'), danger: true }))) return;
        const r = await api(`/admin/egress/${encodeURIComponent(name)}`, { method: 'DELETE' });
        toast(r.ok ? t('Uscita {name} eliminata.', { name }) : r.error, r.ok ? 'ok' : 'err');
        await loadEgress();
        renderEgress();
    }
});
$('#egress-list').addEventListener('change', async e => {
    const sw = e.target.closest('[data-eg=toggle]');
    if (!sw) return;
    const r = await post(`/admin/egress/${encodeURIComponent(sw.dataset.name)}/enabled`, { enabled: sw.checked });
    if (!r.ok) { sw.checked = !sw.checked; toast(r.error || t('Operazione non riuscita.'), 'err'); return; }
    await loadEgress();
    renderEgress();
});
async function addProxy() {
    await dialog({
        title: t('Aggiungi un proxy'), confirm: t('Aggiungi'),
        body: t('Un proxy SOCKS5 o HTTP da usare come uscita di rete per i plugin.'),
        fields: [
            { name: 'name', label: t('Nome'), required: true, placeholder: t('es. casa-vpn'), maxlength: 40 },
            { name: 'proxy_url', label: t('Indirizzo'), required: true, placeholder: 'socks5://host:port', mono: true },
        ],
        onSubmit: async v => {
            const r = await post('/admin/egress', { name: v.name.trim(), proxy_url: v.proxy_url.trim(), kind: 'proxy' });
            if (!r.ok) return r.error;
            toast(t('Uscita {name} aggiunta. Provala per verificarla.', { name: v.name.trim() }), 'ok');
        },
    });
    await loadEgress();
    renderEgress();
}
async function addWireGuard() {
    let file = null;
    await dialog({
        title: t('Aggiungi una configurazione WireGuard'), confirm: t('Carica e avvia'),
        body: html`<span>${tr('Un file <code>.conf</code> di Mullvad, Proton o di un tuo peer. mycelium avvia un processo dedicato e lo usa come uscita.')}</span>
            <div class="drop" data-wg="drop" tabindex="0">${tr('<b>Scegli il file</b> o trascinalo qui')}</div>
            <input type="file" accept=".conf,text/plain" data-wg="file" hidden>`,
        fields: [{ name: 'name', label: t('Nome'), value: 'wg', required: true, maxlength: 40 }],
        onOpen: dlg => {
            const drop = $('[data-wg=drop]', dlg), input = $('[data-wg=file]', dlg);
            const pick = f => {
                if (!f) return;
                file = f;
                mount(drop, html`${icon('file')} <b>${f.name}</b>`);
            };
            drop.addEventListener('click', () => input.click());
            drop.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); input.click(); } });
            input.addEventListener('change', () => pick(input.files[0]));
            drop.addEventListener('dragover', e => { e.preventDefault(); drop.classList.add('over'); });
            drop.addEventListener('dragleave', () => drop.classList.remove('over'));
            drop.addEventListener('drop', e => { e.preventDefault(); drop.classList.remove('over'); pick(e.dataTransfer.files[0]); });
        },
        onSubmit: async v => {
            if (!file) return t('Scegli il file .conf.');
            if (file.size > 128 * 1024) return t('Il file è troppo grande per essere una configurazione WireGuard.');
            const fd = new FormData();
            fd.append('name', v.name.trim());
            fd.append('conf', file, file.name || 'wg0.conf');
            const r = await api('/admin/egress/wireproxy', { method: 'POST', body: fd });
            if (!r.ok) return r.error;
            toast(t('Uscita WireGuard {name} attiva. Provala per verificarla.', { name: v.name.trim() }), 'ok');
        },
    });
    await loadEgress();
    renderEgress();
}
$('form[data-form=browser]').addEventListener('submit', async e => {
    e.preventDefault();
    const v = formValues(e.target);
    const body = { browser_service_url: v.browser_service_url.trim() };
    if (v.browser_service_api_key.trim()) body.browser_service_api_key = v.browser_service_api_key.trim();
    const r = await saveSettings(body);
    if (!r.ok) { toast(r.error || t('Salvataggio non riuscito.'), 'err'); return; }
    toast(t('Servizio browser aggiornato.'), 'ok');
    markClean(e.target);
    await loadInfo();
    renderBrowserService();
    testBrowserService();
});
$('#egress-ipv6').addEventListener('change', async e => {
    const on = e.target.checked;
    const r = await saveSettings({ egress_ipv6: on ? '1' : '0' });
    if (!r.ok) { e.target.checked = !on; toast(r.error || t('Salvataggio non riuscito.'), 'err'); return; }
    toast(on ? t('In uscita viene provato prima IPv6.') : t('In uscita viene usato prima IPv4.'), 'ok');
});

// ─── System ──────────────────────────────────────────────────────────────────
function renderSystem() {
    const i = state.info;
    if (i) {
        const ver = String(i.version || '').replace(/^v/, '');
        mount('#sys-version', i.update_available && i.latest_version
            ? html`<div class="action-row" style="border:0;padding:0">
                <span><b>${t('mycelium {v} · è disponibile la {latest}', { v: ver, latest: String(i.latest_version).replace(/^v/, '') })}</b>
                <small>${i.in_docker ? t('Si aggiorna scaricando la nuova immagine Docker.') : t('Aggiorna l\'installazione del server.')}</small></span>
                <button class="btn btn-primary" data-action="core-update">${icon('download')}${t('Aggiorna')}</button></div>`
            : html`<div class="ov-line"><span class="pill ok">${t('Aggiornato')}</span><span>mycelium ${ver}${i.latest_version ? '' : html` <span class="faint">· ${t('non è stato possibile controllare gli aggiornamenti')}</span>`}</span></div>`);
        const form = $('form[data-form=warm]');
        if (canRefill(form)) fillForm(form, { warm_resolve_enabled: i.warm_resolve_enabled !== false, warm_resolve_per_hour: i.warm_resolve_per_hour ?? 20 });
    }
    const w = state.warm;
    if (w) {
        const ok = w.warm_completed_ok || 0;
        const secs = ms => (ms / 1000).toFixed(1) + ' s';
        const dur = x => x && x.count ? `${secs(x.mean_ms)} (p90 ${secs(x.p90_ms)}, ${x.count})` : '—';
        const reasons = { disabled: 'spenta', busy: 'server occupato', budget: 'limite orario', browser_busy: 'browser occupato', recent: 'già fatta da poco' };
        const skipped = Object.entries(w.warm_skipped || {}).map(([k, v]) => `${reasons[k] ? t(reasons[k]) : k} ${v}`).join(', ') || t('nessuna');
        const fresh = w.resolve_duration_fresh || {};
        const rows = [
            ['Avviate', w.warm_started || 0],
            ['Unite a una già in corso', w.warm_joined || 0],
            ['Completate', t('{ok} riuscite, {err} errori', { ok, err: w.warm_completed_err || 0 })],
            ['Durata media', dur(w.warm_duration)],
            ['Play serviti in anticipo', `${w.warm_hit || 0}${ok ? ` (${Math.round(100 * (w.warm_hit || 0) / ok)}%)` : ''}, ${t('{n} mentre era in corso', { n: w.warm_hit_inflight || 0 })}`],
            ['Saltate', skipped],
            ['Play con estrazione propria', `${dur(fresh)}${fresh.count ? ', ' + t('{n} dalla cache', { n: fresh.cache_hits || 0 }) : ''}`],
            ['Play uniti a una in corso', dur(w.resolve_duration_joined)],
        ];
        mount('#warm-stats', rows.map(([k, v]) => html`<dt>${t(k)}</dt><dd>${v}</dd>`));
    }
}
pages.system = {
    async enter() {
        renderSystem();
        await Promise.all([loadInfo(), loadWarm()]);
        renderSystem();
    },
};
$('form[data-form=warm]').addEventListener('submit', async e => {
    e.preventDefault();
    const v = formValues(e.target);
    const n = parseFloat(v.warm_resolve_per_hour);
    const r = await saveSettings({
        warm_resolve_enabled: v.warm_resolve_enabled ? '1' : '0',
        warm_resolve_per_hour: String(isNaN(n) || n < 0 ? 20 : Math.floor(n)),
    });
    if (!r.ok) { toast(r.error || t('Salvataggio non riuscito.'), 'err'); return; }
    toast(t('Estrazione anticipata salvata.'), 'ok');
    markClean(e.target);
    await loadInfo();
});
$('form[data-form=password]').addEventListener('submit', async e => {
    e.preventDefault();
    const form = e.target;
    const v = formValues(form);
    if (v.new_password.length < 8) { toast(t('La nuova password deve avere almeno 8 caratteri.'), 'err'); return; }
    const r = await post('/admin/password/change', { current_password: v.current_password, new_password: v.new_password });
    if (!r.ok) { toast(r.error || t('Cambio password non riuscito.'), 'err'); return; }
    form.reset();
    toast(t('Password aggiornata.'), 'ok');
});
async function coreUpdate(btn) {
    btn.disabled = true;
    const r = await post('/admin/core/update');
    btn.disabled = false;
    const d = r.data || {};
    if (d.status === 'up_to_date') { toast(t('Il server è già aggiornato.'), 'info'); await loadInfo(); renderSystem(); return; }
    if (d.status === 'updating') { toast(t('Aggiornamento avviato: il servizio si riavvierà.'), 'ok'); waitForRestart(); return; }
    if (d.status === 'manual_required') {
        await dialog({
            title: t('Aggiornare a {v}', { v: d.version || t('la nuova versione') }),
            body: html`<span>${d.detail || ''}</span>${d.url ? html`<a href="${d.url}" target="_blank" rel="noopener">${t('Note di rilascio')}</a>` : ''}`,
            confirm: t('Ho capito'), cancel: false,
        });
        return;
    }
    toast(d.detail || r.error || t('Aggiornamento non riuscito.'), 'err');
}
// After a restart, reload as soon as the server answers again.
function waitForRestart() {
    const started = Date.now();
    const check = async () => {
        try {
            const r = await fetch('/health', { cache: 'no-store' });
            if (r.ok && Date.now() - started > 2500) { location.reload(); return; }
        } catch { /* still down */ }
        if (Date.now() - started < 90_000) setTimeout(check, 1500);
        else toast(t('Il server non risponde ancora: ricarica la pagina tra poco.'), 'warn');
    };
    setTimeout(check, 2000);
}
async function memoryProfile(btn) {
    const box = $('#mem-profile');
    btn.disabled = true;
    mount(box, html`<p class="muted small">${t('Misuro…')}</p>`);
    const r = await api('/admin/lua-plugins/memory');
    btn.disabled = false;
    if (!r.ok) { mount(box, html`<p class="danger-text small">${r.error}</p>`); return; }
    const d = r.data;
    const plugins = d.plugins || [];
    const max = Math.max(1, ...plugins.map(p => (p.bytes || 0) + (p.index_bytes || 0)));
    mount(box, html`
        <div class="mem-summary">
            <span>${t('Processo')} <b>${fmtMem(d.rss_bytes)}</b></span>
            <span>${t('Heap Go')} <b>${fmtMem(d.go_heap_bytes)}</b></span>
            <span>${t('Plugin Lua')} <b>${fmtMem(d.lua_total_bytes)}</b></span>
            <span>${t('Indici condivisi')} <b>${fmtMem(d.index_bytes || 0)}</b></span>
            <span>${t('Goroutine')} <b>${d.goroutines}</b></span>
            <span>${t('Misurato in')} <b>${d.took_ms} ms</b></span>
        </div>
        ${plugins.length ? plugins.map(p => {
            const total = (p.bytes || 0) + (p.index_bytes || 0);
            const states = p.states || [];
            const notes = [p.busy ? t('{n} VM occupate, non misurate', { n: p.busy }) : '', states.some(s => s.truncated) ? t('misura troncata') : ''].filter(Boolean).join(' · ');
            return html`<details class="mem-row">
                <summary><b>${p.name || p.plugin_id}</b><span class="faint small">${states.length}/${p.pool_size} VM</span><span class="mono" style="margin-left:auto">${fmtMem(total)}</span></summary>
                <div class="progress bar"><i style="width:${Math.round(100 * total / max)}%"></i></div>
                <div class="sub">
                    <div><span>${t('per VM')}: ${states.map(s => fmtMem(s.bytes)).join(' + ') || '—'}${notes ? ' · ' + notes : ''}</span><span>${p.took_ms} ms</span></div>
                    ${(p.indexes || []).map(ix => html`<div><span>${t('indice {name} (condiviso)', { name: ix.name })}</span><span>${t('{r} record · {k} chiavi', { r: ix.records, k: ix.keys })}</span><span>${fmtMem(ix.bytes)}</span></div>`)}
                    ${(p.top || []).map(it => html`<div><span>${it.path}</span><span>${t('{n} voci', { n: it.entries })}</span><span>${fmtMem(it.bytes)}</span></div>`)}
                </div>
            </details>`;
        }) : html`<p class="muted small">${t('Nessun plugin caricato.')}</p>`}`);
}

// ─── Logs ────────────────────────────────────────────────────────────────────
let sysLog = null;
pages.logs = {
    enter() { sysLog = createLogView($('#syslog'), { streamUrl: '/admin/logs/stream', max: 1500 }); },
    leave() { sysLog?.destroy(); sysLog = null; },
};

// ─── Guided tour ─────────────────────────────────────────────────────────────
// A walk through the real pages: each step opens its page, dims everything
// but the element it talks about and explains it next to it. Shown once per
// browser (and always right after the setup, ?tour=1); "Guida" in the menu
// reopens it. The first step also picks the language.
const TOUR_DONE_KEY = 'mycelium.tourDone';
const tourSteps = [
    { title: 'Benvenuto in Mycelium', lang: true,
      body: 'Mycelium è il server dietro Pileus: raccoglie i contenuti dai plugin e li porta su TV, telefoni e browser di casa. Ti mostro le pagine principali, una alla volta.' },
    { page: 'overview', target: 'section[data-page=overview] .grid-3', title: 'Panoramica',
      body: 'Qui vedi come sta il server: processore, memoria e da quanto è acceso. Sopra compaiono solo gli avvisi che chiedono la tua attenzione.' },
    { page: 'overview', target: 'section[data-page=overview] .grid-2', title: 'Riepiloghi',
      body: 'Plugin, dispositivi, download ed estrazione anticipata in breve. Tocca una scheda per aprire la sua pagina.' },
    { page: 'plugins', target: 'section[data-page=plugins] .page-head [data-action=plugin-install]', title: 'Installa i plugin',
      body: 'I plugin sono le sorgenti dei contenuti. Installali da qui, anche più file .zip insieme, oppure trascinandoli nella pagina.' },
    { page: 'plugins', target: '#plugin-list', title: 'I tuoi plugin',
      body: 'Ogni scheda è un plugin e il colore dello stato dice se funziona, se manca una configurazione o se c\'è un errore. Aprila per impostazioni, rete, attività e log.' },
    { page: 'devices', target: 'section[data-page=devices] .page-head [data-action=pair]', title: 'Abbina i dispositivi',
      body: 'Per collegare una TV o un telefono genera un codice qui e inseriscilo in Pileus. L\'elenco sotto mostra i dispositivi abbinati.' },
    { page: 'profiles', target: 'section[data-page=profiles] .page-head [data-action=profile-create]', title: 'Profili',
      body: 'Un profilo per ogni persona di casa, con la sua cronologia e un PIN facoltativo. Sono condivisi da tutti i dispositivi.' },
    { page: 'network', target: '#egress-list', card: true, title: 'Uscite di rete',
      body: 'Da dove escono le connessioni dei plugin: direttamente dal server, da un proxy o da una VPN WireGuard. Ogni plugin sceglie la sua.' },
    { page: 'system', target: '#sys-version', card: true, title: 'Sistema',
      body: 'Aggiornamenti, password, prestazioni e manutenzione. In fondo alla pagina c\'è la zona con le azioni irreversibili.' },
    { target: '.nav-foot', nav: true, title: 'Lingua e guida',
      body: 'Qui in fondo al menu cambi la lingua, riapri questa guida ed esci.' },
    { title: 'Tutto pronto', final: true,
      body: 'Il primo passo è installare un plugin, poi abbinare un dispositivo.' },
];
const tourState = { i: 0, active: false, el: null, seq: 0 };

function tourLayer() {
    let layer = $('#tour-layer');
    if (!layer) {
        layer = document.createElement('div');
        layer.id = 'tour-layer';
        layer.className = 'tour-layer';
        layer.innerHTML = '<div class="tour-block"></div><div class="tour-spot"></div><div class="tour-pop" role="dialog" aria-modal="true"></div>';
        document.body.appendChild(layer);
        layer.addEventListener('click', onTourClick);
    }
    return layer;
}
const sleep = ms => new Promise(r => setTimeout(r, ms));
async function waitForEl(sel, ms = 3000) {
    const end = Date.now() + ms;
    for (;;) {
        const el = $(sel);
        if (el && el.getClientRects().length) return el;
        if (Date.now() > end) return null;
        await sleep(80);
    }
}
const narrow = () => matchMedia('(max-width: 900px)').matches;

async function showTourStep() {
    // A newer call (a fast "Next") makes this one stale: it must not paint.
    const seq = ++tourState.seq;
    const s = tourSteps[tourState.i];
    const layer = tourLayer();
    layer.hidden = false;
    // Open the page the step is about.
    if (s.page && current?.name !== s.page) location.hash = s.page === 'overview' ? '#/' : `#/${s.page}`;
    const navOpen = !!s.nav && narrow();
    if (document.body.classList.contains('nav-open') !== navOpen) {
        document.body.classList.toggle('nav-open', navOpen);
        await sleep(280); // the drawer slides in or out first
    }
    let el = null;
    if (s.target) {
        el = await waitForEl(s.target);
        if (el && s.card) el = el.closest('.card') || el;
        if (el) {
            el.scrollIntoView({ block: 'center', behavior: 'instant' });
            await sleep(60);
        }
    }
    if (!tourState.active || seq !== tourState.seq) return;
    tourState.el = el;
    const last = tourState.i === tourSteps.length - 1;
    mount($('.tour-pop', layer), html`
        <div class="tour-dots">${tourSteps.map((_, i) => html`<i class="${i === tourState.i ? 'on' : ''}"></i>`)}</div>
        ${!el ? html`<div class="tour-art">${tourState.i === 0 ? html`<img src="/static/logo.svg" alt="" width="68" height="68">` : html`<span class="tour-icon">${icon('check')}</span>`}</div>` : ''}
        <h3>${t(s.title)}</h3>
        <p>${t(s.body)}</p>
        ${s.lang ? html`<div class="tour-lang"><span>${t('Lingua della dashboard')}</span>
            <div class="segmented"><button data-lang="en">English</button><button data-lang="it">Italiano</button></div></div>` : ''}
        ${s.final ? html`<div class="tour-cta">
            <button class="btn btn-secondary" data-tour="go" data-go="plugins">${icon('upload')}${t('Installa un plugin')}</button>
            <button class="btn btn-secondary" data-tour="go" data-go="devices">${icon('wifi')}${t('Abbina un dispositivo')}</button></div>
            <p class="small faint">${t('Puoi riaprire questa guida quando vuoi dal menu, alla voce Guida.')}</p>` : ''}
        <div class="tour-nav">
            ${last ? '' : html`<button class="btn btn-ghost btn-sm" data-tour="close">${t('Salta')}</button>`}
            <span class="tour-count">${tourState.i + 1} / ${tourSteps.length}</span>
            ${tourState.i > 0 ? html`<button class="btn btn-secondary btn-sm" data-tour="prev">${t('Indietro')}</button>` : ''}
            <button class="btn btn-primary btn-sm" data-tour="${last ? 'close' : 'next'}">${last ? t('Inizia') : t('Avanti')}</button>
        </div>`);
    paintLangSwitch();
    placeTour();
    $('.tour-pop [data-tour=next], .tour-pop [data-tour=close]:last-child', layer)?.focus({ preventScroll: true });
}

// placeTour puts the spotlight on the step's element and the explanation
// next to it (below, above, or centred when there is no element).
function placeTour() {
    const layer = $('#tour-layer');
    if (!layer || layer.hidden) return;
    const spot = $('.tour-spot', layer), pop = $('.tour-pop', layer), block = $('.tour-block', layer);
    const el = tourState.el;
    const vw = innerWidth, vh = innerHeight, gap = 14, pad = 8;
    if (!el) {
        spot.hidden = true;
        block.classList.add('dim');
        pop.classList.add('centered');
        pop.style.left = pop.style.top = '';
        return;
    }
    block.classList.remove('dim');
    pop.classList.remove('centered');
    const r = el.getBoundingClientRect();
    spot.hidden = false;
    Object.assign(spot.style, {
        left: `${r.left - pad}px`, top: `${r.top - pad}px`,
        width: `${r.width + pad * 2}px`, height: `${r.height + pad * 2}px`,
    });
    const pw = pop.offsetWidth, ph = pop.offsetHeight;
    let top = r.bottom + pad + gap;
    if (top + ph > vh - 12) top = r.top - pad - gap - ph;
    if (top < 12) top = Math.min(Math.max(12, r.bottom - ph), vh - ph - 12);
    let left = Math.min(Math.max(12, r.left + r.width / 2 - pw / 2), vw - pw - 12);
    pop.style.left = `${left}px`;
    pop.style.top = `${top}px`;
}
addEventListener('resize', () => tourState.active && placeTour());
addEventListener('scroll', () => tourState.active && placeTour(), true);

function openTour() {
    $$('dialog[open]').forEach(d => d.close());
    tourState.active = true;
    tourState.i = 0;
    showTourStep();
}
function closeTour(go) {
    tourState.active = false;
    tourState.el = null;
    try { localStorage.setItem(TOUR_DONE_KEY, '1'); } catch { /* shown again next time */ }
    const layer = $('#tour-layer');
    if (layer) layer.hidden = true;
    document.body.classList.remove('nav-open');
    location.hash = go ? `#/${go}` : '#/';
}
function onTourClick(e) {
    const b = e.target.closest('[data-tour]');
    if (!b) return;
    switch (b.dataset.tour) {
        case 'next': tourState.i = Math.min(tourState.i + 1, tourSteps.length - 1); showTourStep(); break;
        case 'prev': tourState.i = Math.max(tourState.i - 1, 0); showTourStep(); break;
        case 'close': closeTour(); break;
        case 'go':
            closeTour(b.dataset.go);
            if (b.dataset.go === 'devices') setTimeout(pairDevice, 300);
            break;
    }
}
document.addEventListener('keydown', e => {
    if (!tourState.active) return;
    if (e.key === 'Escape') closeTour();
    else if (e.key === 'ArrowRight' && tourState.i < tourSteps.length - 1) { tourState.i++; showTourStep(); }
    else if (e.key === 'ArrowLeft' && tourState.i > 0) { tourState.i--; showTourStep(); }
});
function shouldShowTour() {
    if (new URLSearchParams(location.search).has('tour')) return true;
    try { return localStorage.getItem(TOUR_DONE_KEY) !== '1'; } catch { return false; }
}

// ─── Global actions ──────────────────────────────────────────────────────────
document.addEventListener('click', async e => {
    const b = e.target.closest('[data-action]');
    if (!b || b.disabled) return;
    switch (b.dataset.action) {
        case 'logout':
            await post('/admin/logout');
            location.href = '/admin/login';
            break;
        case 'tour': document.body.classList.remove('nav-open'); openTour(); break;
        case 'plugin-install': openInstallDialog(); break;
        case 'profile-create': createProfile(); break;
        case 'pair': pairDevice(); break;
        case 'webapp-update': updateWebApp(); break;
        case 'egress-add-proxy': addProxy(); break;
        case 'egress-add-wg': addWireGuard(); break;
        case 'bs-test': testBrowserService(); break;
        case 'bs-clear-key': {
            const r = await saveSettings({ browser_service_api_key: '' });
            toast(r.ok ? t('Chiave rimossa.') : (r.error || t('Operazione non riuscita.')), r.ok ? 'ok' : 'err');
            await loadInfo();
            renderBrowserService();
            break;
        }
        case 'core-update': coreUpdate(b); break;
        case 'cache-clear': {
            const r = await post('/admin/cache/clear');
            toast(r.ok ? t('Cache video svuotata.') : (r.error || t('Operazione non riuscita.')), r.ok ? 'ok' : 'err');
            break;
        }
        case 'restart': {
            if (!(await confirmDialog(t('Riavviare il servizio?'), t('Streaming e connessioni attive si interrompono per qualche secondo, poi tutto riparte da solo.'), { confirm: t('Riavvia') }))) return;
            const r = await post('/admin/system/restart');
            if (!r.ok) { toast(r.error || t('Riavvio non riuscito.'), 'err'); return; }
            toast(t('Riavvio in corso: la pagina si ricarica da sola.'), 'info');
            waitForRestart();
            break;
        }
        case 'mem-profile': memoryProfile(b); break;
        case 'wipe-profiles':
            dangerAction({
                title: t('Cancellare i dati di tutti i profili?'),
                body: t('Profili, cronologia di visione e dati privati dei plugin vengono eliminati. I dispositivi restano abbinati.'),
                confirm: t('Cancella i profili'), url: '/admin/profiles/wipe',
                done: d => toast(t('Cancellati {p} profili e {h} voci di cronologia.', { p: d.profiles ?? 0, h: d.history_rows ?? 0 }), 'ok'),
            });
            break;
        case 'wipe-plugins':
            dangerAction({
                title: t('Cancellare i dati di tutti i plugin?'),
                body: t('Redis, cache immagini e cache su disco vengono svuotati. I plugin restano installati e configurati e ricostruiscono tutto alla prossima sincronizzazione.'),
                confirm: t('Cancella i dati'), url: '/admin/plugins/wipe-data',
                done: d => toast(t('Cancellati {img} immagini e {files} file di cache.', { img: d.webp_files ?? 0, files: d.plugin_cache_files ?? 0 })
                    + (d.redis_error ? ' ' + t('Errore Redis: {err}', { err: d.redis_error }) : ''), d.redis_error ? 'warn' : 'ok'),
            });
            break;
        case 'factory-reset':
            dangerAction({
                title: t('Reset di fabbrica?'),
                body: html`<span>${tr('Cancella <b>tutto</b>: profili, dispositivi, plugin, dati e la password amministratore. Il server torna come appena installato.')}</span><span>${t('Non si può annullare.')}</span>`,
                confirm: t('Cancella tutto'), url: '/admin/factory-reset',
                done: () => { location.href = '/setup'; },
            });
            break;
    }
});

// Mobile navigation drawer.
document.addEventListener('click', e => {
    if (e.target.closest('[data-nav-open]')) document.body.classList.add('nav-open');
    else if (e.target.closest('[data-nav-close]')) document.body.classList.remove('nav-open');
});
document.addEventListener('keydown', e => { if (e.key === 'Escape') document.body.classList.remove('nav-open'); });

// ─── Start ───────────────────────────────────────────────────────────────────
(async () => {
    document.documentElement.lang = getLang();
    translateStatic();
    paintLangSwitch();
    route();
    connectLive();
    if (shouldShowTour()) {
        openTour();
        // ?tour=1 (from the setup) is a one-off: drop it from the address.
        if (location.search) history.replaceState(null, '', location.pathname + location.hash);
    }
    await Promise.all([loadInfo(), loadPlugins()]);
    setInterval(loadInfo, 3_600_000);
})();

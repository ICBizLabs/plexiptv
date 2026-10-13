'use strict';

// ---------- helpers ----------
const $ = (s) => document.querySelector(s);
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'value') el.value = v;
    else if (k === 'checked') el.checked = !!v;
    else if (k === 'indeterminate') el.indeterminate = !!v;
    else if (k === 'selected') el.selected = !!v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : String(kid));
  }
  return el;
}
// fill replaces an element's children, flattening arrays and skipping null/false.
const fill = (el, ...kids) => el.replaceChildren(...kids.flat(Infinity).filter((k) => k != null && k !== false));
const opt = (value, label, sel) => h('option', { value, selected: String(sel) === String(value) }, label);

async function api(method, path, body) {
  const r = await fetch(path, {
    method, credentials: 'same-origin',
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  let data = null;
  try { data = await r.json(); } catch { /* empty body */ }
  if (r.status === 401 && !path.startsWith('/api/login')) { showLogin(); throw new Error('Login required'); }
  if (!r.ok) throw new Error((data && data.error) || r.statusText);
  return data;
}
function toast(msg, isErr) {
  const el = h('div', { class: isErr ? 'err' : '' }, msg);
  $('#toast').append(el);
  setTimeout(() => el.remove(), isErr ? 7000 : 3500);
}
const fail = (e) => toast(e.message || String(e), true);
function debounce(fn, ms) { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; }
function ago(unix) {
  if (!unix) return 'never';
  const s = Math.round(Date.now() / 1000 - unix);
  if (s < 60) return s + 's ago';
  if (s < 3600) return Math.round(s / 60) + ' min ago';
  if (s < 86400) return Math.round(s / 3600) + ' h ago';
  return Math.round(s / 86400) + ' d ago';
}
function bytes(n) {
  const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i ? 1 : 0) + ' ' + u[i];
}
const PLEX_LIMIT = 480;
// tunerWarning explains a tuner whose lineup is held back by its channel limit
// (or, with no limit, one that is too big for Plex).
function tunerWarning(d) {
  if (d.channels > d.sent) {
    return `${d.channels.toLocaleString()} channels selected, but only the first ${d.sent.toLocaleString()} (lowest numbers) are sent to Plex because of this tuner's limit. Narrow your groups/rules or add another tuner.`;
  }
  if (d.sent > PLEX_LIMIT) return `Over Plex's ~${PLEX_LIMIT} channel limit per tuner. Plex setup may hang on "Finding the best guide". Set a channel limit or add another tuner.`;
  return null;
}

// ---------- modal ----------
function modal(title, body, actions) {
  const dlg = $('#modal');
  const form = $('#modal-form');
  fill(form, h('h2', {}, title), body, h('div', { class: 'dialog-actions' }, actions));
  if (!dlg.open) dlg.showModal();
  return dlg;
}
const closeModal = () => $('#modal').close();
function field(label, input, hint) {
  return h('label', { class: 'field' }, h('span', {}, label), input, hint ? h('small', { class: 'muted' }, hint) : null);
}

// ---------- shared data ----------
const store = { sources: [], devices: [], host: location.hostname, webPort: location.port || 80 };
async function loadShared() {
  const [sources, devices] = await Promise.all([api('GET', '/api/sources'), api('GET', '/api/devices')]);
  store.sources = sources; store.devices = devices;
}
const sourceName = (id) => (store.sources.find((s) => s.id === id) || {}).name || '#' + id;
const deviceName = (id) => (store.devices.find((d) => d.id === id) || {}).name || 'default';

// ---------- auth ----------
async function boot() {
  const st = await api('GET', '/api/state');
  if (!st.authed) return showLogin(st.setup);
  $('#login').classList.add('hidden');
  $('#shell').classList.remove('hidden');
  await loadShared().catch(fail);
  route();
}
function showLogin(setup) {
  $('#shell').classList.add('hidden');
  $('#login').classList.remove('hidden');
  $('#login').dataset.setup = setup ? '1' : '';
  $('#login-msg').textContent = setup ? 'First run: choose a password for the web panel.' : '';
  $('#login-btn').textContent = setup ? 'Set password' : 'Log in';
  $('#login-pw').autocomplete = setup ? 'new-password' : 'current-password';
  $('#login-pw').focus();
}
$('#login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const setup = $('#login').dataset.setup === '1';
  try {
    await api('POST', setup ? '/api/setup' : '/api/login', { password: $('#login-pw').value });
    $('#login-pw').value = ''; $('#login-err').textContent = '';
    boot();
  } catch (err) { $('#login-err').textContent = err.message; }
});

// ---------- routing ----------
let timer = null;
const views = {};
function route() {
  clearInterval(timer); timer = null;
  const [tab, qs] = (location.hash.slice(1) || 'dashboard').split('?');
  const params = new URLSearchParams(qs || '');
  document.querySelectorAll('#tabs a').forEach((a) => a.classList.toggle('active', a.dataset.tab === tab));
  const view = $('#view');
  fill(view, h('p', { class: 'muted' }, 'Loading…'));
  (views[tab] || views.dashboard)(view, params).catch(fail);
}
document.querySelectorAll('#tabs a').forEach((a) => a.addEventListener('click', () => { location.hash = a.dataset.tab; }));
window.addEventListener('hashchange', route);

// ---------- dashboard ----------
views.dashboard = async (view) => {
  const [st, sources] = await Promise.all([api('GET', '/api/status'), api('GET', '/api/sources')]);
  store.sources = sources;
  store.host = st.host || location.hostname;
  const base = `http://${store.host}:${st.web_port}`;
  const devCards = st.devices.map((d) => h('div', { class: 'card' },
    h('h2', {}, d.name),
    h('div', { class: 'stat' }, d.sent.toLocaleString(), h('span', { class: 'muted', style: 'font-size:13px' }, ' channels sent to Plex')),
    tunerWarning(d) ? h('p', { class: 'warn' }, tunerWarning(d)) : null,
    h('p', {}, 'Add to Plex: ', h('code', {}, `${store.host}:${d.port}`)),
    h('p', { class: 'muted' }, `Device ID ${d.device_id} · ${d.tuners} tuners · numbers from ${d.start_number}`),
  ));
  const srcRows = sources.map((s) => h('tr', {},
    h('td', {}, s.name), h('td', {}, s.kind),
    h('td', {}, s.kind === 'epg' ? '—' : s.channel_count.toLocaleString()),
    h('td', {}, s.refreshing ? h('span', { class: 'warn' }, 'refreshing…') : ago(s.last_refresh)),
    h('td', { class: s.last_error ? 'error' : 'ok' }, s.last_error || 'ok'),
  ));
  fill(view, 
    st.rules_error ? h('div', { class: 'card error' }, 'Rule problem: ' + st.rules_error) : null,
    !sources.length ? h('div', { class: 'card' }, h('h2', {}, 'Get started'),
      h('ol', {},
        h('li', {}, 'Add your provider under ', h('a', { href: '#sources' }, 'Sources'), ' (Xtream login or M3U URL).'),
        h('li', {}, 'Pick what you want under ', h('a', { href: '#groups' }, 'Groups'), ' (one click per group) and fine-tune with ', h('a', { href: '#rules' }, 'Rules'), '.'),
        h('li', {}, 'In Plex: Settings → Live TV & DVR → Set up Plex DVR. If the tuner isn\'t found, enter the address shown below.'),
        h('li', {}, 'When Plex asks for a guide, choose "Have an XMLTV guide?" and use the XMLTV URL below.'))) : null,
    h('div', { class: 'grid' }, devCards),
    h('div', { class: 'card' },
      h('h2', {}, 'Guide and playlist'),
      h('p', {}, 'XMLTV for Plex: ', h('code', {}, base + '/xmltv.xml')),
      h('p', {}, 'M3U of your lineup (for other players): ', h('code', {}, base + '/lineup.m3u')),
      h('p', { class: 'muted' }, `Active streams: ${st.streams} · ffmpeg ${st.ffmpeg ? 'found' : 'not found (HLS sources are passed through as-is)'}`)),
    sources.length ? h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {}, ['Source', 'Type', 'Channels', 'Refreshed', 'Status'].map((t) => h('th', {}, t)))),
      h('tbody', {}, srcRows))) : null,
  );
  if (sources.some((s) => s.refreshing)) timer = setInterval(() => route(), 3000);
};

// ---------- channels ----------
const ch = { search: '', source: '', group: '', status: 'all', device: '', sort: 'source', offset: 0, limit: 100, selected: new Set(), allMatching: false, total: 0, rows: [] };

function chQuery() {
  const p = new URLSearchParams();
  for (const k of ['search', 'source', 'group', 'status', 'device', 'sort']) if (ch[k]) p.set(k, ch[k]);
  return p;
}

views.channels = async (view, params) => {
  if (params.has('group')) { ch.group = params.get('group'); ch.source = params.get('source') || ''; ch.offset = 0; ch.search = ''; ch.status = 'all'; }
  await loadShared();
  const groups = await api('GET', '/api/groups');
  const groupSel = h('select', { onchange: (e) => { ch.group = e.target.value; reload(); } },
    opt('', 'All groups', ch.group),
    groups.filter((g) => !ch.source || String(g.source_id) === String(ch.source))
      .map((g) => opt(g.name, `${g.name} (${g.enabled}/${g.total})`, ch.group)));
  const search = h('input', { type: 'search', placeholder: 'Search name, group or tvg-id…', value: ch.search,
    oninput: debounce((e) => { ch.search = e.target.value; reload(); }, 200) });
  const toolbar = h('div', { class: 'toolbar' },
    search,
    h('select', { onchange: (e) => { ch.source = e.target.value; ch.group = ''; route(); } },
      opt('', 'All sources', ch.source), store.sources.filter((s) => s.kind !== 'epg').map((s) => opt(s.id, s.name, ch.source))),
    groupSel,
    h('select', { onchange: (e) => { ch.status = e.target.value; reload(); } },
      opt('all', 'Any status', ch.status), opt('enabled', 'In lineup', ch.status), opt('disabled', 'Not in lineup', ch.status),
      opt('manual', 'Manually set', ch.status), opt('noepg', 'In lineup, no EPG id', ch.status),
      opt('capped', 'Over tuner limit (not sent)', ch.status)),
    store.devices.length > 1 ? h('select', { onchange: (e) => { ch.device = e.target.value; reload(); } },
      opt('', 'All tuners', ch.device), store.devices.map((d) => opt(d.id, d.name, ch.device))) : null,
    h('select', { onchange: (e) => { ch.sort = e.target.value; reload(); } },
      opt('source', 'Provider order', ch.sort), opt('number', 'Channel number', ch.sort), opt('name', 'Name', ch.sort)),
  );
  const bulk = h('div', { class: 'bulk hidden' });
  const tableWrap = h('div', { class: 'table-wrap' });
  const pager = h('div', { class: 'pager' });
  fill(view, toolbar, bulk, tableWrap, pager);

  async function reload(keepOffset) {
    if (!keepOffset) { ch.offset = 0; ch.selected.clear(); ch.allMatching = false; }
    const p = chQuery(); p.set('offset', ch.offset); p.set('limit', ch.limit);
    const t0 = performance.now();
    const res = await api('GET', '/api/channels?' + p);
    ch.rows = res.rows; ch.total = res.total;
    renderTable(); renderBulk();
    const last = Math.min(ch.offset + ch.limit, ch.total);
    fill(pager, 
      h('span', { class: 'muted' }, `${ch.total ? ch.offset + 1 : 0}–${last} of ${ch.total.toLocaleString()} (${Math.round(performance.now() - t0)} ms)`),
      h('button', { disabled: ch.offset === 0, onclick: (e) => { e.preventDefault(); ch.offset = Math.max(0, ch.offset - ch.limit); reload(true); } }, '‹ Prev'),
      h('button', { disabled: last >= ch.total, onclick: (e) => { e.preventDefault(); ch.offset += ch.limit; reload(true); } }, 'Next ›'));
  }

  async function patch(ids, p) {
    try {
      const body = { ids, patch: p };
      let url = '/api/channels';
      if (ch.allMatching) { body.all_matching = true; body.ids = []; url += '?' + chQuery(); }
      const r = await api('PATCH', url, body);
      toast(`Updated ${r.updated.toLocaleString()} channel(s)`);
      await reload(true);
    } catch (e) { fail(e); }
  }

  function renderBulk() {
    const n = ch.allMatching ? ch.total : ch.selected.size;
    bulk.classList.toggle('hidden', n === 0);
    if (!n) return;
    const ids = [...ch.selected];
    fill(bulk, h('div', { class: 'row' },
      h('strong', {}, `${n.toLocaleString()} selected`),
      !ch.allMatching && ch.selected.size === ch.rows.length && ch.total > ch.rows.length
        ? h('a', { onclick: () => { ch.allMatching = true; renderBulk(); } }, `Select all ${ch.total.toLocaleString()} matching`) : null,
      h('span', { class: 'spacer' }),
      h('button', { onclick: () => patch(ids, { enabled: 'on' }) }, 'Add to lineup'),
      h('button', { onclick: () => patch(ids, { enabled: 'off' }) }, 'Remove'),
      h('button', { title: 'Clear manual on/off so rules decide', onclick: () => patch(ids, { enabled: 'rules' }) }, 'Follow rules'),
      store.devices.length > 1 ? h('select', { onchange: (e) => { if (e.target.value) patch(ids, { device_id: +e.target.value }); } },
        opt('', 'Move to tuner…'), store.devices.map((d) => opt(d.id, d.name))) : null,
      !ch.allMatching ? h('button', { onclick: () => renumberDialog(ids) }, 'Renumber…') : null,
      h('button', { onclick: () => { ch.selected.clear(); ch.allMatching = false; renderTable(); renderBulk(); } }, 'Clear'),
    ));
  }

  function renumberDialog(ids) {
    const ordered = ch.rows.filter((r) => ch.selected.has(r.id)).map((r) => r.id);
    const start = h('input', { type: 'number', min: 1, value: ch.rows.find((r) => r.id === ordered[0])?.number || 1000 });
    modal('Renumber channels', h('div', {},
      h('p', { class: 'muted' }, `${ordered.length} channel(s) get consecutive numbers in the order shown. Channels already using those numbers are given new ones.`),
      field('Start at', start)), [
      h('button', { value: 'cancel' }, 'Cancel'),
      h('button', { class: 'primary', onclick: async (e) => {
        e.preventDefault();
        try { await api('POST', '/api/channels/renumber', { ids: ordered, start: +start.value }); closeModal(); toast('Renumbered'); reload(true); } catch (err) { fail(err); }
      } }, 'Renumber')]);
  }

  function editable(text, cls, onSave, type) {
    const span = h('span', { class: 'edit ' + (cls || ''), title: 'Click to edit' }, text === '' ? '—' : text);
    span.addEventListener('click', () => {
      const inp = h('input', { type: type || 'text', value: text, style: 'width:100%' });
      let done = false;
      const finish = async (save) => {
        if (done) return; done = true;
        if (save && inp.value !== String(text)) await onSave(inp.value);
        else span.replaceWith(editable(text, cls, onSave, type));
      };
      inp.addEventListener('keydown', (e) => { if (e.key === 'Enter') finish(true); if (e.key === 'Escape') finish(false); });
      inp.addEventListener('blur', () => finish(true));
      span.replaceWith(inp); inp.focus(); inp.select();
    });
    return span;
  }

  function renderTable() {
    const allOnPage = ch.rows.length > 0 && ch.rows.every((r) => ch.selected.has(r.id));
    const head = h('tr', {},
      h('th', {}, h('input', { type: 'checkbox', checked: allOnPage || ch.allMatching, onchange: (e) => {
        ch.allMatching = false;
        ch.rows.forEach((r) => e.target.checked ? ch.selected.add(r.id) : ch.selected.delete(r.id));
        renderTable(); renderBulk();
      } })),
      ['', '#', 'Name', 'Group', 'EPG id', store.devices.length > 1 ? 'Tuner' : null, 'Status'].filter((x) => x !== null).map((t) => h('th', {}, t)));
    const body = ch.rows.map((r) => {
      const sel = ch.allMatching || ch.selected.has(r.id);
      return h('tr', { class: (sel ? 'selected ' : '') + (r.enabled ? '' : 'dim') },
        h('td', {}, h('input', { type: 'checkbox', checked: sel, disabled: ch.allMatching, onchange: (e) => {
          e.target.checked ? ch.selected.add(r.id) : ch.selected.delete(r.id); renderTable(); renderBulk();
        } })),
        h('td', { class: 'logo' }, r.logo ? h('img', { src: r.logo, loading: 'lazy', alt: '', referrerpolicy: 'no-referrer', onerror: (e) => e.target.remove() }) : null),
        h('td', { class: 'num mono' }, r.number ? editable(String(r.number), '', (v) => patch([r.id], { number: parseInt(v, 10) || 0 }), 'number') : h('span', { class: 'muted' }, '—')),
        h('td', {}, editable(r.name, '', (v) => patch([r.id], { name: v === r.orig_name ? '' : v })),
          r.name !== r.orig_name ? h('div', { class: 'muted', style: 'font-size:12px' }, 'was: ' + r.orig_name) : null),
        h('td', {}, h('a', { onclick: () => { ch.group = r.group; ch.source = String(r.source_id); route(); } }, r.group || '—'),
          store.sources.length > 1 ? h('div', { class: 'muted', style: 'font-size:12px' }, r.source_name) : null),
        h('td', { class: 'mono' }, h('a', { onclick: () => epgDialog(r, () => reload(true)) }, r.epg_id || 'set…')),
        store.devices.length > 1 ? h('td', {}, r.enabled ? deviceName(r.device_id) : '') : null,
        h('td', {}, h('span', {
          class: 'pill ' + (r.capped ? 'capped' : r.enabled ? (r.manual ? 'on manual' : 'on') : (r.manual ? 'manual' : '')),
          style: 'cursor:pointer', title: 'Click to toggle (sets a manual override)',
          onclick: () => patch([r.id], { enabled: r.enabled ? 'off' : 'on' }),
        }, r.capped ? 'over tuner limit' : r.enabled ? 'in lineup' : 'off', r.manual ? ' · manual' : (r.rule_id ? ' · rule' : ''))),
      );
    });
    fill(tableWrap, h('table', {}, h('thead', {}, head), h('tbody', {}, body)));
    if (!ch.rows.length) tableWrap.append(h('p', { class: 'muted', style: 'padding:12px' }, store.sources.length ? 'No channels match.' : 'No sources yet — add one under Sources.'));
  }

  await reload(true);
};

function epgDialog(row, after) {
  const q = h('input', { type: 'search', value: row.orig_name, placeholder: 'Search guide channels…' });
  const manual = h('input', { type: 'text', value: row.epg_id, placeholder: 'XMLTV channel id' });
  const list = h('div', { style: 'max-height:300px;overflow:auto' });
  const save = async (id) => {
    try { await api('PATCH', '/api/channels', { ids: [row.id], patch: { epg_id: id } }); closeModal(); toast('EPG id saved'); after(); } catch (e) { fail(e); }
  };
  const search = debounce(async () => {
    const res = await api('GET', '/api/epg/search?q=' + encodeURIComponent(q.value)).catch(fail);
    if (!res) return;
    fill(list, res.length ? h('table', {}, h('tbody', {}, res.map((c) => h('tr', {},
      h('td', { class: 'logo' }, c.icon ? h('img', { src: c.icon, loading: 'lazy', alt: '', referrerpolicy: 'no-referrer', onerror: (e) => e.target.remove() }) : null),
      h('td', {}, c.name, h('div', { class: 'muted mono' }, c.id)),
      h('td', {}, h('button', { class: 'small', onclick: (e) => { e.preventDefault(); save(c.id); } }, 'Use')))))) : h('p', { class: 'muted' }, 'No guide channels found. Is an EPG loaded for your sources?'));
  }, 200);
  q.addEventListener('input', search);
  modal('Guide (EPG) for ' + row.name, h('div', {},
    h('p', { class: 'muted' }, `Provider tvg-id: ${row.tvg_id || 'none'}`),
    field('Search', q), list, field('Or enter an id', manual)), [
    h('button', { value: 'cancel' }, 'Cancel'),
    h('button', { onclick: (e) => { e.preventDefault(); save(''); } }, 'Reset to provider'),
    h('button', { class: 'primary', onclick: (e) => { e.preventDefault(); save(manual.value.trim()); } }, 'Save')]);
  search();
}

// ---------- groups ----------
const gr = { filter: '', source: '', show: 'all' };
views.groups = async (view) => {
  await loadShared();
  let groups = await api('GET', '/api/groups');
  const wrap = h('div', { class: 'table-wrap' });
  const summary = h('span', { class: 'muted' });
  const render = () => {
    const f = gr.filter.toLowerCase();
    const shown = groups.filter((g) => (!gr.source || String(g.source_id) === gr.source) && (!f || g.name.toLowerCase().includes(f)) &&
      (gr.show === 'all' || (gr.show === 'on' ? g.enabled > 0 : g.enabled === 0)));
    const enabledTotal = groups.reduce((a, g) => a + g.enabled, 0);
    summary.textContent = `${shown.length.toLocaleString()} groups shown · ${enabledTotal.toLocaleString()} channels in lineup`;
    fill(wrap, h('table', {},
      h('thead', {}, h('tr', {}, ['', 'Group', store.sources.length > 1 ? 'Source' : null, 'Channels', 'In lineup', ''].filter((x) => x !== null).map((t) => h('th', {}, t)))),
      h('tbody', {}, shown.map((g) => h('tr', { class: g.enabled ? '' : 'dim' },
        h('td', {}, h('input', { type: 'checkbox', checked: g.enabled === g.total, indeterminate: g.enabled > 0 && g.enabled < g.total,
          onchange: async () => {
            try {
              await api('POST', '/api/groups/toggle', { source_id: g.source_id, group: g.name, include: g.enabled === 0 });
              groups = await api('GET', '/api/groups'); render();
            } catch (e) { fail(e); }
          } })),
        h('td', {}, g.name || '(no group)'),
        store.sources.length > 1 ? h('td', { class: 'muted' }, g.source_name) : null,
        h('td', {}, g.total.toLocaleString()),
        h('td', { class: g.enabled > PLEX_LIMIT ? 'warn' : '' }, g.enabled.toLocaleString()),
        h('td', {}, h('a', { href: `#channels?group=${encodeURIComponent(g.name)}&source=${g.source_id}` }, 'channels ›')))))));
  };
  fill(view, 
    h('div', { class: 'card muted' }, 'Ticking a group adds an include rule for it; unticking adds an exclude rule (or removes the include). Fine-tune individual channels on the Channels page, or write pattern rules on the Rules page.'),
    h('div', { class: 'toolbar' },
      h('input', { type: 'search', placeholder: 'Filter groups…', value: gr.filter, oninput: debounce((e) => { gr.filter = e.target.value; render(); }, 120) }),
      h('select', { onchange: (e) => { gr.source = e.target.value; render(); } }, opt('', 'All sources', gr.source),
        store.sources.filter((s) => s.kind !== 'epg').map((s) => opt(String(s.id), s.name, gr.source))),
      h('select', { onchange: (e) => { gr.show = e.target.value; render(); } }, opt('all', 'All groups', gr.show), opt('on', 'With channels in lineup', gr.show), opt('off', 'Not in lineup', gr.show)),
      summary),
    wrap);
  render();
};

// ---------- rules ----------
views.rules = async (view) => {
  await loadShared();
  const data = await api('GET', '/api/rules');
  const editing = { id: 0, action: 'include', field: 'group', op: 'contains', value: '', source_id: 0, device_id: 0, enabled: true };
  const preview = h('div', { class: 'muted' }, 'Type a value to preview matches.');
  const f = {
    action: h('select', {}, opt('include', 'Include'), opt('exclude', 'Exclude')),
    field: h('select', {}, opt('group', 'Group'), opt('name', 'Name'), opt('tvg_id', 'tvg-id'), opt('any', 'Any field')),
    op: h('select', {}, opt('contains', 'contains'), opt('equals', 'equals'), opt('prefix', 'starts with'), opt('suffix', 'ends with'), opt('regex', 'matches regex')),
    value: h('input', { type: 'text', placeholder: 'e.g. US | Sports', style: 'flex:1 1 220px' }),
    source: h('select', {}, opt(0, 'Any source'), store.sources.filter((s) => s.kind !== 'epg').map((s) => opt(s.id, s.name))),
    device: store.devices.length > 1 ? h('select', {}, opt(0, 'Default tuner'), store.devices.map((d) => opt(d.id, d.name))) : null,
  };
  const saveBtn = h('button', { class: 'primary' }, 'Add rule');
  const cancelBtn = h('button', { class: 'hidden' }, 'Cancel edit');
  const read = () => ({ ...editing, action: f.action.value, field: f.field.value, op: f.op.value, value: f.value.value,
    source_id: +f.source.value, device_id: f.device ? +f.device.value : 0 });
  const doPreview = debounce(async () => {
    const r = read();
    if (!r.value) { fill(preview, 'Type a value to preview matches.'); return; }
    try {
      const p = await api('POST', '/api/rules/preview', r);
      fill(preview, h('strong', {}, `${p.matches.toLocaleString()} channels match`),
        p.sample?.length ? h('ul', { class: 'samples' }, p.sample.map((s) => h('li', {}, s))) : null);
    } catch (e) { fill(preview, h('span', { class: 'error' }, e.message)); }
  }, 200);
  Object.values(f).forEach((el) => el && el.addEventListener(el.tagName === 'INPUT' ? 'input' : 'change', doPreview));
  const load = (r) => {
    Object.assign(editing, r);
    f.action.value = r.action; f.field.value = r.field; f.op.value = r.op; f.value.value = r.value; f.source.value = r.source_id;
    if (f.device) f.device.value = r.device_id;
    saveBtn.textContent = r.id ? 'Save rule' : 'Add rule';
    cancelBtn.classList.toggle('hidden', !r.id);
    doPreview();
  };
  saveBtn.addEventListener('click', async () => {
    const r = read();
    if (!r.value) return toast('Enter a value', true);
    try {
      await api(r.id ? 'PUT' : 'POST', r.id ? `/api/rules/${r.id}` : '/api/rules', r);
      toast(r.id ? 'Rule saved' : 'Rule added'); route();
    } catch (e) { fail(e); }
  });
  cancelBtn.addEventListener('click', () => load({ id: 0, action: 'include', field: 'group', op: 'contains', value: '', source_id: 0, device_id: 0, enabled: true }));

  const rules = data.rules;
  const move = async (i, d) => {
    const ids = rules.map((r) => r.id);
    [ids[i], ids[i + d]] = [ids[i + d], ids[i]];
    try { await api('POST', '/api/rules/order', { ids }); route(); } catch (e) { fail(e); }
  };
  const opLabel = { contains: 'contains', equals: 'equals', prefix: 'starts with', suffix: 'ends with', regex: 'matches' };
  const fieldLabel = { group: 'group', name: 'name', tvg_id: 'tvg-id', any: 'any field' };
  const rows = rules.map((r, i) => h('tr', { class: r.enabled ? '' : 'dim' },
    h('td', { class: 'mono muted' }, i + 1),
    h('td', {}, h('button', { class: 'small', disabled: i === 0, onclick: () => move(i, -1) }, '↑'), ' ',
      h('button', { class: 'small', disabled: i === rules.length - 1, onclick: () => move(i, 1) }, '↓')),
    h('td', {}, h('input', { type: 'checkbox', checked: r.enabled, title: 'Enabled', onchange: async (e) => {
      try { await api('PUT', `/api/rules/${r.id}`, { ...r, enabled: e.target.checked }); route(); } catch (err) { fail(err); }
    } })),
    h('td', {}, h('span', { class: 'pill ' + (r.action === 'include' ? 'on' : '') }, r.action), ' ',
      fieldLabel[r.field], ' ', h('span', { class: 'muted' }, opLabel[r.op]), ' ', h('code', {}, r.value),
      r.source_id ? h('span', { class: 'muted' }, ` · only ${sourceName(r.source_id)}`) : null,
      r.device_id ? h('span', { class: 'muted' }, ` → ${deviceName(r.device_id)}`) : null),
    h('td', {}, (data.counts[r.id] ?? 0).toLocaleString()),
    h('td', {}, h('button', { class: 'small', onclick: () => { load(r); window.scrollTo(0, 0); } }, 'Edit'), ' ',
      h('button', { class: 'small danger', onclick: async () => {
        if (!confirm('Delete this rule?')) return;
        try { await api('DELETE', `/api/rules/${r.id}`); route(); } catch (e) { fail(e); }
      } }, 'Delete'))));
  fill(view, 
    data.error ? h('div', { class: 'card error' }, data.error) : null,
    h('div', { class: 'card' },
      h('h2', {}, 'Rule'),
      h('div', { class: 'row' }, f.action, f.field, f.op, f.value, f.source, f.device, saveBtn, cancelBtn),
      h('div', { style: 'margin-top:10px' }, preview)),
    h('div', { class: 'card muted' }, 'Rules run top to bottom and the last matching rule wins, so put broad includes first and exceptions after them (for example: include group starts with "US |", then exclude name matches "\\b(4K|UHD)\\b"). A manual on/off on the Channels page always beats rules. Matching ignores case.'),
    h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {}, ['', 'Order', 'On', 'Rule', 'Matches', ''].map((t) => h('th', {}, t)))),
      h('tbody', {}, rows))),
    !rules.length ? h('p', { class: 'muted' }, 'No rules yet. Tick groups on the Groups page or add a rule above.') : null);
};

// ---------- sources ----------
views.sources = async (view) => {
  const sources = await api('GET', '/api/sources');
  store.sources = sources;
  const cards = sources.map((s) => h('div', { class: 'card' },
    h('div', { class: 'row' }, h('h2', { style: 'margin:0' }, s.name), h('span', { class: 'pill' }, s.kind), !s.enabled ? h('span', { class: 'pill' }, 'disabled') : null,
      h('span', { class: 'spacer' }),
      h('button', { disabled: s.refreshing, onclick: async () => {
        try { await api('POST', `/api/sources/${s.id}/refresh`); toast('Refresh started'); setTimeout(route, 500); } catch (e) { fail(e); }
      } }, s.refreshing ? 'Refreshing…' : 'Refresh'),
      h('button', { onclick: () => sourceDialog(s) }, 'Edit'),
      h('button', { class: 'danger', onclick: async () => {
        if (!confirm(`Delete ${s.name}? Its channels, numbers and overrides are removed.`)) return;
        try { await api('DELETE', `/api/sources/${s.id}`); route(); } catch (e) { fail(e); }
      } }, 'Delete')),
    h('p', { class: 'muted mono' }, s.url, s.username ? ` · ${s.username}` : ''),
    s.kind !== 'epg' ? h('p', {}, `${s.channel_count.toLocaleString()} live channels`,
      s.skipped_count ? h('span', { class: 'muted' }, ` (${s.skipped_count.toLocaleString()} VOD/series skipped)`) : null,
      ` · streams ${s.active_streams}/${(s.max_conns || s.provider_conns) || '∞'}`,
      ` · mode ${s.stream_mode}`) : null,
    h('p', {}, `Guide: ${s.epg_count.toLocaleString()} channels`, s.epg_error ? h('span', { class: 'error' }, ` · ${s.epg_error}`) : null),
    h('p', { class: s.last_error ? 'error' : 'muted' }, `Refreshed ${ago(s.last_refresh)}`, s.refresh_hours ? ` · every ${s.refresh_hours} h` : ' · manual only',
      s.last_error ? ` · ${s.last_error}` : '')));
  fill(view, 
    h('div', { class: 'toolbar' },
      h('button', { class: 'primary', onclick: () => sourceDialog({ kind: 'xtream' }) }, '+ Xtream account'),
      h('button', { onclick: () => sourceDialog({ kind: 'm3u' }) }, '+ M3U playlist'),
      h('button', { onclick: () => sourceDialog({ kind: 'epg' }) }, '+ XMLTV guide')),
    cards.length ? cards : h('p', { class: 'muted' }, 'No sources yet.'));
  if (sources.some((s) => s.refreshing)) timer = setInterval(async () => {
    const now = await api('GET', '/api/sources').catch(() => null);
    if (now && !now.some((s) => s.refreshing)) route();
  }, 2000);
};

function sourceDialog(s) {
  const isNew = !s.id;
  const kind = s.kind;
  const i = {
    name: h('input', { value: s.name || '', placeholder: kind === 'xtream' ? 'My provider' : 'Name' }),
    url: h('input', { value: s.url || '', placeholder: kind === 'xtream' ? 'http://provider.example:8080' : (kind === 'm3u' ? 'http://…/playlist.m3u or C:\\path\\list.m3u' : 'http://…/guide.xml.gz') }),
    username: h('input', { value: s.username || '', autocomplete: 'off' }),
    password: h('input', { type: 'password', value: '', autocomplete: 'new-password', placeholder: s.has_password ? '(unchanged)' : '' }),
    epg_url: h('input', { value: s.epg_url || '', placeholder: kind === 'xtream' ? 'blank = provider xmltv.php' : 'blank = url-tvg from the playlist' }),
    max_conns: h('input', { type: 'number', min: 0, value: s.max_conns || 0 }),
    stream_mode: h('select', {}, opt('auto', 'Auto (proxy; ffmpeg for HLS)', s.stream_mode || 'auto'), opt('proxy', 'Proxy (pass-through)', s.stream_mode),
      opt('ffmpeg', 'ffmpeg remux', s.stream_mode), opt('redirect', 'Redirect (no proxy)', s.stream_mode)),
    user_agent: h('input', { value: s.user_agent || '', placeholder: 'blank = Go default' }),
    refresh_hours: h('input', { type: 'number', min: 0, value: s.refresh_hours ?? 24 }),
    enabled: h('input', { type: 'checkbox', checked: isNew || s.enabled }),
  };
  const label = { xtream: 'Xtream Codes account', m3u: 'M3U playlist', epg: 'XMLTV guide' }[kind];
  const body = h('div', {},
    field('Name', i.name),
    field(kind === 'xtream' ? 'Server URL' : 'URL or file path', i.url),
    kind === 'xtream' ? [field('Username', i.username), field('Password', i.password)] : null,
    kind !== 'epg' ? [
      field('Guide (XMLTV) URL', i.epg_url),
      field('Max concurrent streams', i.max_conns, kind === 'xtream' ? '0 = use the limit your provider reports' : '0 = unlimited'),
      field('Stream mode', i.stream_mode),
    ] : null,
    field('User-Agent', i.user_agent),
    field('Auto refresh (hours)', i.refresh_hours, '0 = only when you click Refresh'),
    h('label', { class: 'check' }, i.enabled, 'Enabled'));
  modal((isNew ? 'Add ' : 'Edit ') + label, body, [
    h('button', { value: 'cancel' }, 'Cancel'),
    h('button', { class: 'primary', onclick: async (e) => {
      e.preventDefault();
      const out = { kind, name: i.name.value, url: i.url.value, username: i.username.value, password: i.password.value,
        epg_url: i.epg_url.value, max_conns: +i.max_conns.value, stream_mode: i.stream_mode.value, user_agent: i.user_agent.value,
        refresh_hours: +i.refresh_hours.value, enabled: i.enabled.checked };
      try {
        await api(isNew ? 'POST' : 'PUT', isNew ? '/api/sources' : `/api/sources/${s.id}`, out);
        closeModal(); toast(isNew ? 'Source added — importing in the background' : 'Saved'); route();
      } catch (err) { fail(err); }
    } }, isNew ? 'Add and import' : 'Save')]);
}

// ---------- devices ----------
views.devices = async (view) => {
  const [devices, st] = await Promise.all([api('GET', '/api/devices'), api('GET', '/api/status')]);
  store.devices = devices; store.host = st.host || location.hostname;
  fill(view, 
    h('div', { class: 'card muted' }, `Plex handles about ${PLEX_LIMIT} channels per tuner device. With a bigger lineup, add more tuners and send groups to them with a rule's tuner option or "Move to tuner" on the Channels page. Each tuner gets its own port and is added to Plex as a separate DVR.`),
    h('div', { class: 'toolbar' }, h('button', { class: 'primary', onclick: () => deviceDialog({}, devices) }, '+ Add tuner')),
    h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {}, ['Name', 'Plex address', 'Device ID', 'Tuners', 'Numbers from', 'Channel limit', 'Sent / selected', ''].map((t) => h('th', {}, t)))),
      h('tbody', {}, devices.map((d) => h('tr', {},
        h('td', {}, d.name, d.web_port ? h('span', { class: 'muted' }, ' (default)') : null),
        h('td', { class: 'mono' }, `${store.host}:${d.port}`),
        h('td', { class: 'mono' }, d.device_id),
        h('td', {}, d.tuners),
        h('td', {}, d.start_number),
        h('td', {}, d.max_channels ? d.max_channels.toLocaleString() : 'none'),
        h('td', { class: tunerWarning(d) ? 'warn' : '', title: tunerWarning(d) || '' },
          `${d.sent.toLocaleString()} / ${d.channels.toLocaleString()}`, tunerWarning(d) ? ' ⚠' : ''),
        h('td', {}, h('button', { class: 'small', onclick: () => deviceDialog(d, devices) }, 'Edit'), ' ',
          !d.web_port ? h('button', { class: 'small danger', onclick: async () => {
            if (!confirm(`Delete ${d.name}? Its channels move to the default tuner.`)) return;
            try { await api('DELETE', `/api/devices/${d.id}`); route(); } catch (e) { fail(e); }
          } }, 'Delete') : null)))))));
};

function deviceDialog(d, devices) {
  const isNew = !d.id;
  const nextPort = Math.max(...devices.map((x) => x.port)) + 1;
  const i = {
    name: h('input', { value: d.name || `plexiptv ${devices.length + 1}` }),
    port: h('input', { type: 'number', value: d.port || nextPort, disabled: d.web_port }),
    tuners: h('input', { type: 'number', min: 1, max: 32, value: d.tuners || 2 }),
    start: h('input', { type: 'number', min: 1, value: d.start_number || (Math.max(...devices.map((x) => x.start_number)) + 1000) }),
    max: h('input', { type: 'number', min: 0, value: isNew ? PLEX_LIMIT : d.max_channels }),
  };
  modal(isNew ? 'Add tuner' : 'Edit tuner', h('div', {},
    field('Name (shown in Plex)', i.name),
    field('Port', i.port, d.web_port ? 'The default tuner shares the web panel port.' : 'Allow this port through the firewall.'),
    field('Tuner count', i.tuners, 'How many streams Plex may open at once. Match your provider connection limit.'),
    field('Auto-number channels from', i.start),
    field('Channel limit', i.max, `Only this many channels (lowest numbers first) are sent to Plex. Plex stalls on lineups much over ~${PLEX_LIMIT}. 0 = no limit.`)), [
    h('button', { value: 'cancel' }, 'Cancel'),
    h('button', { class: 'primary', onclick: async (e) => {
      e.preventDefault();
      const out = { name: i.name.value, port: +i.port.value, tuners: +i.tuners.value, start_number: +i.start.value, max_channels: +i.max.value };
      try { await api(isNew ? 'POST' : 'PUT', isNew ? '/api/devices' : `/api/devices/${d.id}`, out); closeModal(); route(); } catch (err) { fail(err); }
    } }, 'Save')]);
}

// ---------- streams ----------
views.streams = async (view) => {
  const render = async () => {
    const list = await api('GET', '/api/streams');
    fill(view, list.length ? h('div', { class: 'table-wrap' }, h('table', {},
      h('thead', {}, h('tr', {}, ['Channel', 'Source', 'Mode', 'Clients', 'Running', 'Data', ''].map((t) => h('th', {}, t)))),
      h('tbody', {}, list.map((s) => h('tr', {},
        h('td', {}, s.name), h('td', {}, sourceName(s.source_id)), h('td', {}, s.mode || 'connecting'),
        h('td', {}, (s.clients || []).map((c) => c.remote).join(', ') || '—'),
        h('td', {}, Math.round((Date.now() - new Date(s.started)) / 60000) + ' min'),
        h('td', {}, bytes(s.bytes)),
        h('td', {}, h('button', { class: 'small danger', onclick: async () => { await api('DELETE', `/api/streams/${s.channel_id}`).catch(fail); render(); } }, 'Stop')))))))
      : h('p', { class: 'muted' }, 'Nothing is streaming right now.'));
  };
  await render();
  timer = setInterval(() => render().catch(() => {}), 3000);
};

// ---------- logs ----------
views.logs = async (view) => {
  const pre = h('pre', { class: 'logs' });
  fill(view, pre);
  const render = async () => {
    const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
    pre.textContent = (await api('GET', '/api/logs')).join('\n');
    if (atBottom) pre.scrollTop = pre.scrollHeight;
  };
  await render();
  pre.scrollTop = pre.scrollHeight;
  timer = setInterval(() => render().catch(() => {}), 3000);
};

// ---------- settings ----------
views.settings = async (view) => {
  const s = await api('GET', '/api/settings');
  const dummy = h('input', { type: 'checkbox', checked: s.dummy_epg, onchange: async (e) => {
    try { await api('PUT', '/api/settings', { dummy_epg: e.target.checked }); toast('Saved'); } catch (err) { fail(err); }
  } });
  const pw = h('input', { type: 'password', autocomplete: 'new-password', placeholder: 'New password' });
  fill(view, 
    h('div', { class: 'card' }, h('h2', {}, 'Guide'),
      h('label', { class: 'check' }, dummy, 'Generate placeholder guide entries for channels without EPG data'),
      h('p', { class: 'muted' }, 'Plex hides channels that have no guide data, so keep this on unless every channel has a real EPG.'),
      h('button', { onclick: async () => {
        try { const r = await api('POST', '/api/epg/automatch'); toast(`Matched ${r.matched} channel(s) to guide entries by name`); } catch (e) { fail(e); }
      } }, 'Auto-match missing EPG ids by name')),
    h('div', { class: 'card' }, h('h2', {}, 'Password'),
      h('div', { class: 'row' }, pw, h('button', { onclick: async () => {
        try { await api('PUT', '/api/settings', { new_password: pw.value }); pw.value = ''; toast('Password changed'); } catch (e) { fail(e); }
      } }, 'Change')),
      h('p', {}, h('button', { onclick: async () => { await api('POST', '/api/logout'); showLogin(false); } }, 'Log out'))),
    h('div', { class: 'card' }, h('h2', {}, 'System'),
      h('p', {}, 'Data folder: ', h('code', {}, s.data_dir)),
      h('p', {}, 'ffmpeg: ', s.ffmpeg ? h('span', { class: 'ok' }, 'found') : h('span', { class: 'warn' }, 'not found — set ffmpeg_path in config.yaml or add ffmpeg to PATH to remux HLS streams'))));
};

boot().catch(fail);

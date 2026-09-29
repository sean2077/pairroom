'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { test } = require('node:test');
const source = fs.readFileSync(path.join(__dirname, '../internal/webui/assets/updates.js'), 'utf8');
const window = {};
vm.runInNewContext(source, { window, document: { readyState: 'loading', addEventListener() {} }, URL, AbortController, setTimeout, clearTimeout, Date });
const create = window.PairRoomUpdates.create;
const AUTO = 'pairroom.updates.auto', IGNORE = 'pairroom.updates.ignored';
const time = Date.parse('2026-09-29T00:00:00Z');
const release = (latest = '5.10.0') => ({ status: 'available', current_version: '5.9.0', latest_version: latest,
  release_url: `https://github.com/sean2077/pairroom/releases/tag/v${latest}`, checked_at: new Date(time).toISOString(),
  next_check_at: new Date(time + 6 * 3600000).toISOString() });
function storage(values = {}) {
  const data = new Map(Object.entries(values));
  return { getItem: key => data.get(key) ?? null, setItem: (key, value) => data.set(key, value), removeItem: key => data.delete(key) };
}
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }

test('no contact before authentication or automatic-check consent; manual checks do not opt in', async () => {
  let calls = 0;
  const prefs = storage();
  const controller = create({ storage: prefs, now: () => time, request: async () => { calls++; return release(); } });
  await controller.check(true);
  controller.setActive(true);
  await controller.check();
  assert.equal(calls, 0);
  assert.equal(controller.state().auto, false);
  await controller.check(true);
  assert.equal(calls, 1);
  assert.equal(controller.state().result.latest_version, '5.10.0');
  assert.equal(controller.state().notify, false);
  assert.equal(prefs.getItem(AUTO), null);
});

test('automatic cadence, manual refresh and version-specific persistent dismissal', async () => {
  let now = time, latest = '5.10.0', calls = 0;
  const prefs = storage({ [AUTO]: 'true' });
  const controller = create({ storage: prefs, now: () => now, request: async () => { calls++; return release(latest); } });
  controller.setActive(true); await controller.check();
  assert.equal(calls, 1); assert.equal(controller.state().notify, true);
  controller.ignore();
  assert.equal(prefs.getItem(IGNORE), '5.10.0'); assert.equal(controller.state().notify, false);
  now += 3600000; await controller.check(); assert.equal(calls, 1);
  await controller.check(true); assert.equal(calls, 2); assert.equal(controller.state().notify, false);
  latest = '5.11.0'; now += 6 * 3600000;
  await controller.check(); assert.equal(calls, 3); assert.equal(controller.state().notify, true);
  controller.setAuto(false);
  assert.equal(controller.state().notify, false); assert.equal(prefs.getItem(AUTO), 'false');
});

test('one request lane; disabling automatic checks aborts and ignores late replies', async () => {
  const first = deferred(); let signal, calls = 0;
  const controller = create({ storage: storage({ [AUTO]: 'true' }), now: () => time,
    request: async options => { calls++; signal = options.signal; return first.promise; } });
  controller.setActive(true);
  const a = controller.check(), b = controller.check(true);
  assert.equal(a, b); assert.equal(calls, 1);
  controller.setAuto(false);
  assert.equal(signal.aborted, true); assert.equal(controller.state().checking, false);
  first.resolve(release()); await a;
  assert.equal(controller.state().result, null);
  await controller.check(true); assert.equal(calls, 2);
  assert.equal(controller.state().result.latest_version, '5.10.0');
});

test('logout clears state and cannot accept replies from a previous authenticated session', async () => {
  const first = deferred(); let signal;
  const controller = create({ storage: storage({ [AUTO]: 'true' }), request: async options => { signal = options.signal; return first.promise; } });
  controller.setActive(true); const pending = controller.check();
  controller.setActive(false);
  assert.equal(signal.aborted, true);
  first.resolve(release()); await pending;
  assert.equal(controller.state().result, null); assert.equal(controller.state().notify, false);
});

test('cross-tab preferences apply immediately, and clearing consent disables automatic checks', async () => {
  const prefs = storage(); let calls = 0;
  const controller = create({ storage: prefs, now: () => time, request: async () => { calls++; return release(); } });
  controller.setActive(true); prefs.setItem(AUTO, 'true'); controller.syncPreferences(); await controller.check();
  assert.equal(calls, 1); assert.equal(controller.state().notify, true);
  prefs.setItem(IGNORE, '5.10.0'); controller.syncPreferences(); assert.equal(controller.state().notify, false);
  prefs.removeItem(AUTO); controller.syncPreferences(); assert.equal(controller.state().auto, false);
});

test('blocked preference storage stays usable with memory-only opt-in and dismissal', async () => {
  const controller = create({ storage: { getItem() { throw Error('blocked'); }, setItem() { throw Error('blocked'); } },
    now: () => time, request: async () => release() });
  controller.setActive(true); assert.equal(controller.state().auto, false);
  controller.setAuto(true); await controller.check(); assert.equal(controller.state().notify, true);
  controller.ignore(); controller.syncPreferences(); assert.equal(controller.state().notify, false);
});

test('outages retain a known release, use failure backoff and permit explicit retry', async () => {
  let now = time, calls = 0;
  const controller = create({ storage: storage({ [AUTO]: 'true' }), now: () => now, request: async () => {
    calls++; if (calls > 1) throw Error('offline'); return release();
  } });
  controller.setActive(true); await controller.check();
  await controller.check(true);
  assert.equal(controller.state().failed, true); assert.equal(controller.state().notify, true);
  assert.equal(controller.state().result.latest_version, '5.10.0');
  now += 14 * 60000; await controller.check(); assert.equal(calls, 2);
  now += 60000; await controller.check(); assert.equal(calls, 3);
});

test('GitHub backoff is honored by automatic checks without trusting unbounded timestamps', async () => {
  let now = time, calls = 0;
  const controller = create({ storage: storage({ [AUTO]: 'true' }), now: () => now, request: async () => {
    calls++; return { status: 'unavailable', current_version: '5.9.0', next_check_at: new Date(time + 3600000).toISOString() };
  } });
  controller.setActive(true); await controller.check();
  assert.equal(controller.state().failed, true);
  now += 30 * 60000; await controller.check(); assert.equal(calls, 1);
  now += 30 * 60000; await controller.check(); assert.equal(calls, 2);
});

test('invalid and attacker-selected release links never become actionable results', async () => {
  const mutations = [null, { status: 'invented' }, { current_version: null }, { latest_version: '5.10.0-rc1' },
    { release_url: 'javascript:alert(1)' }, { release_url: 'https://github.com.evil.example/sean2077/pairroom/releases/tag/v5.10.0' },
    { release_url: 'https://github.com/sean2077/pairroom/releases/tag/v5.10.0?token=secret' },
    { release_url: 'https://github.com/sean2077/pairroom/releases/tag/v5.11.0' }, { checked_at: 'not a date' }];
  for (const mutation of mutations) {
    const controller = create({ request: async () => mutation === null ? null : { ...release(), ...mutation } });
    controller.setActive(true); await controller.check(true);
    assert.equal(controller.state().result, null); assert.equal(controller.state().failed, true);
  }
});

test('expired authentication is not retried and cannot retain a release banner', async () => {
  const controller = create({ storage: storage({ [AUTO]: 'true' }), request: async () => { throw Object.assign(Error('expired'), { status: 401 }); } });
  controller.setActive(true); await controller.check();
  assert.equal(controller.state().active, false); assert.equal(controller.state().notify, false);
});

test('native Desktop already owns updates and must not mount another checker', () => {
  let touched = false;
  vm.runInNewContext(source, { window: { PairRoomDesktop: {} }, document: { readyState: 'complete', get body() { touched = true; throw Error('should not mount'); } }, URL, AbortController, setTimeout, clearTimeout, Date });
  assert.equal(touched, false);
});

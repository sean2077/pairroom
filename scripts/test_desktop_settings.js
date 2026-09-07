'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const crypto = require('node:crypto');

function bridge({native = true, platform = 'windows', top = true, search = '?desktop=1'} = {}) {
  const sent = [], timers = new Map(); let sequence = 0;
  const window = {location: {search}, crypto};
  window.top = top ? window : {};
  const transport = {postMessage(message) { sent.push(JSON.parse(message)); }};
  if (native && platform === 'windows') window.chrome = {webview: transport};
  if (native && platform !== 'windows') window.webkit = {messageHandlers: {external: transport}};
  vm.runInNewContext(fs.readFileSync('internal/webui/assets/desktop.js', 'utf8'), {
    window, URLSearchParams,
    setTimeout(callback) { timers.set(++sequence, callback); return sequence; },
    clearTimeout(id) { timers.delete(id); },
  });
  return {api: window.PairRoomDesktop, sent, timers, transport};
}

async function main() {
  for (const options of [{native: false}, {top: false}, {search: ''}]) {
    assert.equal(bridge(options).api, undefined, 'browser/query/iframe alone must not expose desktop controls');
  }
  for (const platform of ['windows', 'darwin', 'linux']) {
    const b = bridge({platform});
    assert.equal(b.sent.length, 0, 'script load must not mutate native state');
    const read = b.api.readStartup();
    assert.equal(b.sent[0].action, 'get');
    assert.equal(b.sent[0].enabled, undefined);
    b.api.receive({id: 'obsolete-window', enabled: true});
    assert.equal(b.timers.size, 1, 'obsolete response must not settle the current request');
    b.api.receive({id: b.sent[0].id, enabled: false});
    assert.equal(await read, false);
    for (const enabled of [true, false]) {
      const write = b.api.setStartup(enabled);
      const request = b.sent.at(-1);
      assert.equal(request.action, 'set'); assert.equal(request.enabled, enabled);
      await assert.rejects(b.api.setStartup(!enabled), /already pending/);
      b.api.receive({id: request.id, enabled});
      assert.equal(await write, enabled);
    }
    assert.equal(b.timers.size, 0);
    await assert.rejects(b.api.setStartup('true'), /boolean/);
  }
  {
    const b = bridge(), pending = b.api.setStartup(true);
    b.api.receive({id: b.sent[0].id, error: 'access denied', enabled: false});
    await assert.rejects(pending, (error) => error.enabled === false && error.message === 'access denied');
    const read = b.api.readStartup();
    b.api.receive({id: b.sent.at(-1).id});
    await assert.rejects(read, /unavailable/);
  }
  {
    const b = bridge(), read = b.api.readStartup();
    b.timers.values().next().value();
    await assert.rejects(read, /did not respond/);
    const next = b.api.readStartup();
    b.api.receive({id: b.sent.at(-1).id, enabled: true});
    assert.equal(await next, true, 'timeout must release request ownership');
  }
  {
    const b = bridge();
    b.transport.postMessage = () => { throw new Error('transport failed'); };
    await assert.rejects(b.api.readStartup(), /transport failed/);
    assert.equal(b.timers.size, 0);
  }
  const html = fs.readFileSync('internal/service/assets/index.html', 'utf8');
  assert.ok(html.indexOf('/_pairroom/desktop.js') < html.indexOf('/management.js'), 'install native bridge before rendering Settings');
  console.log('desktop settings native transport, isolation, timeout, and error contracts: ok');
}
main().catch((error) => { console.error(error); process.exitCode = 1; });

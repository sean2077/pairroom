'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const root = path.resolve(__dirname, '..');
const read = (file) => fs.readFileSync(path.join(root, file), 'utf8');
const ctx = {window: {}};
vm.runInNewContext(read('internal/webui/assets/catalogs.js'), ctx);
const en = ctx.window.PairRoomLocales.en.translation;
const zh = ctx.window.PairRoomLocales['zh-CN'].translation;
const keys = Object.keys(en).filter((key) => key.startsWith('room.native.setup.')).sort();
assert.ok(keys.length >= 12);
assert.deepEqual(keys, Object.keys(zh).filter((key) => key.startsWith('room.native.setup.')).sort());
for (const key of keys) { assert.ok(en[key]); assert.ok(zh[key]); }
for (const file of ['internal/service/assets/index.html', 'internal/service/assets/native-host.html']) {
  const html = read(file);
  assert.ok(html.includes('data-native-setup'), file);
  assert.ok(html.includes('/_pairroom/native-setup.js'), file);
  assert.ok(html.includes('/_pairroom/native-setup.css'), file);
}
const skill = read('skills/pairroom-relay/SKILL.md');
assert.equal(skill, read('internal/relayclient/skill/pairroom-relay/SKILL.md'));
assert.ok(!/CLAUDE_CODE_SESSION_ID|CODEX_SESSION_ID|bind_nonce|--continue|--session-id|exchange --help/.test(skill));
assert.ok(!/Claude Code|Codex/.test(skill));
assert.ok(skill.split('\n').length <= 24, 'onboarding skill should stay concise');
console.log('native setup translations, entry points and skill payload: OK');

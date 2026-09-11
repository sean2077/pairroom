'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const root = path.resolve(__dirname, '..');
const read = file => fs.readFileSync(path.join(root, file), 'utf8');
const entries = {
  management: 'internal/service/assets/index.html',
  room: 'internal/server/assets/index.html',
  native: 'internal/service/assets/native-host.html',
};
for (const [kind, file] of Object.entries(entries)) {
  const html = read(file);
  assert.match(html, new RegExp(`<body class="workbench workbench-${kind}">`));
  const links = [...html.matchAll(/<link\b[^>]*rel="stylesheet"[^>]*>/g)].map(m => m[0]);
  assert.match(links.at(-1), /href="\/_pairroom\/workbench.css"/);
  assert.equal(links.filter(link => link.includes('/workbench.css')).length, 1);
  const scripts = [...html.matchAll(/<script\b[^>]*src="[^"]+"[^>]*>/g)].map(m => m[0]);
  assert.match(scripts[0], /src="\/_pairroom\/theme.js"/);
  assert.doesNotMatch(scripts[0], /\bdefer\b|\basync\b/);
  assert(html.indexOf(scripts[0]) < html.indexOf(links[0]), `${kind}: palette must resolve before CSS`);
  assert.equal(scripts.filter(script => script.includes('/theme.js')).length, 1);
  assert.match(scripts.at(-1), /src="\/_pairroom\/workbench.js"/);
}
const css = read('internal/webui/assets/workbench.css');
assert.doesNotMatch(css, /@import|@font-face|https?:\/\//, 'skin must work offline without remote dependencies');
for (const rule of ['prefers-reduced-motion', 'forced-colors', 'focus-visible']) assert(css.includes(rule));

function luminance(hex) {
  const values = hex.match(/[a-f\d]{2}/gi).map(v => parseInt(v, 16) / 255)
    .map(v => v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4);
  return values[0] * .2126 + values[1] * .7152 + values[2] * .0722;
}
function contrast(a, b) {
  const values = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (values[0] + .05) / (values[1] + .05);
}
const palettes = [...css.matchAll(/:root(?:,\s*:root\[data-theme\]|\[data-theme="dark"\])\s*\{([^}]+)\}/g)].filter(([, block]) => block.includes('--wb-canvas:'));
assert.equal(palettes.length, 2);
for (const [, block] of palettes) {
  const tokens = Object.fromEntries([...block.matchAll(/--wb-([\w-]+):\s*(#[a-f\d]{6});/gi)].map(m => [m[1], m[2]]));
  for (const surface of ['canvas', 'chrome', 'hover']) {
    assert(contrast(tokens.ink, tokens[surface]) >= 4.5, `${surface}: body text contrast`);
    assert(contrast(tokens.muted, tokens[surface]) >= 4.5, `${surface}: metadata contrast`);
  }
  for (const surface of ['primary', 'primary-hover']) {
    assert(contrast(tokens['on-primary'], tokens[surface]) >= 4.5, `${surface}: CTA contrast`);
  }
}

// Head execution must not require body nodes, i18n, or an available storage API.
for (const [saved, osDark, expected] of [['light', true, 'light'], ['dark', false, 'dark'], ['system', true, 'dark'], ['invalid', false, 'light'], [null, true, 'dark'], ['blocked', true, 'dark']]) {
  const events = [], listeners = {};
  const document = {documentElement: {dataset: {}}, readyState: 'loading',
    querySelectorAll: () => [], addEventListener: (type, fn) => { listeners[type] = fn; },
    dispatchEvent: event => events.push(event)};
  const window = {matchMedia: () => ({matches: osDark, addEventListener() {}}), addEventListener() {}};
  vm.runInNewContext(read('internal/webui/assets/theme.js'), {window, document,
    localStorage: {getItem() { if (saved === 'blocked') throw new Error('storage unavailable'); return saved; }, setItem() {}},
    CustomEvent: class { constructor(type, options) { this.type = type; this.detail = options.detail; } }});
  assert.equal(document.documentElement.dataset.theme, expected);
  assert.equal(events.length, 0, 'head execution must not broadcast before the DOM');
  listeners.DOMContentLoaded();
  assert.equal(events[0].type, 'pairroom:theme');
}
console.log('Workbench: shared entrypoints, offline assets, contrast, and pre-paint theme contracts passed.');

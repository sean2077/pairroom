'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.resolve(__dirname, '..');
const listeners = new Map();
const mediaListeners = [];
const storage = new Map([['pairroom.theme', 'system']]);
const media = {
  matches: false,
  addEventListener(type, handler) { if (type === 'change') mediaListeners.push(handler); },
};
const control = {
  dataset: { themeValue: 'system' },
  attrs: {},
  classList: { toggle() {} },
  setAttribute(name, value) { this.attrs[name] = value; },
};
const frameMessages = [];
const document = {
  readyState: 'complete',
  documentElement: { dataset: {} },
  querySelectorAll(selector) {
    if (selector === '[data-theme-value]') return [control];
    if (selector === 'iframe') return [{ contentWindow: { postMessage(value) { frameMessages.push(value); } } }];
    return [];
  },
  addEventListener(type, handler) { listeners.set(`document:${type}`, handler); },
  dispatchEvent() {},
};
const window = {
  location: { origin: 'http://127.0.0.1:7332' },
  matchMedia() { return media; },
  addEventListener(type, handler) { listeners.set(`window:${type}`, handler); },
  PairRoomI18n: { t(key) { return key; } },
};
const context = {
  window, document,
  localStorage: {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, value); },
  },
  CustomEvent: class { constructor(type, options) { this.type = type; this.detail = options?.detail; } },
};
vm.runInNewContext(fs.readFileSync(path.join(root, 'internal/webui/assets/theme.js'), 'utf8'), context);
if (window.PairRoomTheme.mode !== 'system' || document.documentElement.dataset.theme !== 'light') throw new Error('system theme did not resolve from media query');
window.PairRoomTheme.setTheme('dark');
if (storage.get('pairroom.theme') !== 'dark' || document.documentElement.dataset.theme !== 'dark') throw new Error('dark theme did not persist and apply');
listeners.get('window:storage')({ key: 'pairroom.theme', newValue: 'light' });
if (window.PairRoomTheme.mode !== 'light' || document.documentElement.dataset.theme !== 'light') throw new Error('cross-tab storage event did not apply');
listeners.get('window:message')({ origin: window.location.origin, data: { type: 'pairroom-theme', mode: 'system' } });
media.matches = true;
mediaListeners[0]();
if (document.documentElement.dataset.theme !== 'dark') throw new Error('system theme did not react to OS theme change');
if (!frameMessages.some((message) => message.type === 'pairroom-theme')) throw new Error('Management did not broadcast theme to embedded Rooms');

const managementHTML = fs.readFileSync(path.join(root, 'internal/service/assets/index.html'), 'utf8');
if ((managementHTML.match(/data-theme-cycle/g) || []).length < 2) throw new Error('Management topbar and Room tabstrip must both expose theme controls');
const managementJS = fs.readFileSync(path.join(root, 'internal/service/assets/management.js'), 'utf8');
if (/\.style\.|setAttribute\(['"]style|\bstyle\s*:/.test(managementJS)) throw new Error('Management must not create inline styles that violate its Content Security Policy');
const roomHTML = fs.readFileSync(path.join(root, 'internal/server/assets/index.html'), 'utf8');
if (!roomHTML.includes('id="theme-button"')) throw new Error('standalone Room has no theme control');
const roomCSS = fs.readFileSync(path.join(root, 'internal/server/assets/ux.css'), 'utf8');
if (!roomCSS.includes('html[data-embed="1"] body.ux-enhanced #theme-button')) throw new Error('embedded Room theme control is not hidden');

console.log('theme contract ok');

'use strict';

// Render production Markdown with a small DOM so pathological nesting is a
// deterministic regression, independent of a browser's native stack size.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
class Node {
  constructor(tag = '#text', value = '') { this.tagName = tag; this.value = value; this.children = []; this.dataset = {}; }
  appendChild(node) { this.children.push(node); return node; }
  append(...nodes) { for (const node of nodes) this.appendChild(typeof node === 'string' ? new Node('#text', node) : node); }
  set textContent(value) { this.value = value; this.children = []; }
  get textContent() { return this.value + this.children.map(node => node.textContent).join(''); }
  setAttribute(name, value) { this[name] = value; }
  addEventListener() {}
}
const window = {};
const document = { createElement: tag => new Node(tag), createTextNode: text => new Node('#text', text) };
vm.runInNewContext(fs.readFileSync('internal/server/assets/richtext.js', 'utf8'), {window, document});
const render = source => { const root = new Node('main'); window.PairRoomRichText.render(root, source); return root; };
const flatten = node => [node, ...node.children.flatMap(flatten)];
const root = render('# Heading\n\n**bold _nested_** and `literal <script>`\n\n[unsafe](javascript:alert) [safe](https://example.com)\n\n```js\n<script>\n```');
assert.ok(root.textContent.includes('Heading'));
assert.ok(flatten(root).some(node => node.tagName === 'strong'));
assert.ok(flatten(root).some(node => node.tagName === 'em'));
assert.equal(flatten(root).filter(node => node.tagName === 'a').length, 1, 'dangerous URL schemes remain text');
assert.equal(flatten(root).some(node => node.tagName === 'script'), false, 'raw HTML is never executed');
const deep = '> '.repeat(12000) + 'Keep the complete visible message';
let result;
assert.doesNotThrow(() => { result = render(deep); }, 'deep Markdown must not break the Room renderer');
assert.ok(result.textContent.endsWith('Keep the complete visible message'));
assert.ok(flatten(result).length < 100, 'pathological nesting falls back to text, not thousands of DOM nodes');
console.log('richtext safety and bounded nesting: ok');

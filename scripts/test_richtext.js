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
vm.runInNewContext(fs.readFileSync('internal/webui/assets/richtext.js', 'utf8'), {window, document});
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

// A published body may use the full 256 KiB transport budget. Inline scanning is
// linear in the source length; rescanning the tail once per earlier token made
// this dense case take seconds of blocked transcript main thread.
const unit = '**a** `c` @codex https://e.x/p ~~s~~ *e* ';
const long = unit.repeat(Math.ceil((256 * 1024) / unit.length)).slice(0, 256 * 1024);
const started = Date.now();
render(long);
const elapsed = Date.now() - started;
assert.ok(elapsed < 2000, `256 KiB body must render in bounded time (took ${elapsed} ms)`);

// Unmatched link/image brackets restarted a label scan at every '[' (seconds
// for 128 Ki brackets). The scanner must stay linear and keep link rules.
for (const [label, body] of [['[', '['.repeat(128 * 1024)], ['![', '!['.repeat(64 * 1024)],
  ['[a](', '[a]('.repeat(32 * 1024)], ['[a](b "', '[a](b "'.repeat(16 * 1024)]]) {
  const begin = Date.now();
  const output = render(body);
  const took = Date.now() - begin;
  assert.ok(took < 300, `${JSON.stringify(label)} x many must render in linear time (took ${took} ms)`);
  assert.equal(output.textContent, body, 'unmatched brackets remain visible text');
}
const links = flatten(render('[a](https://e.x) ![i](x.png "t") [x](javascript:1) [](https://e.x) [t](#h \'T\') [s](https://e.x "q")'));
assert.deepEqual(links.filter(node => node.tagName === 'a').map(node => [node.href, node.textContent, node.title]),
  [['https://e.x', 'a', ''], ['https://e.x', 'https://e.x', ''], ['#h', 't', 'T'], ['https://e.x', 's', 'q']], 'safe-link rules and titles are unchanged');

// Tables must preserve the same command/path text as ordinary inline code.
// Only a backslash escaping a table pipe belongs to the table syntax.
const table = flatten(render([
  '| Example | Literal |',
  '| --- | --- |',
  '| Windows | `C:\\repo\\file.go` |',
  '| Regex | `\\d+\\.\\d+` |',
  '| UNC | `\\\\server\\share` |',
  '| Pipe | `left\\|right` |',
  '| Final | C:\\repo\\',
  '| Escaped final pipe | left\\|',
  '| Even slashes | `two\\\\` |',
  '| Odd slashes | `two\\\\\\|pipe` |',
].join('\n')));
assert.deepEqual(table.filter(node => node.tagName === 'td').map(node => node.textContent), [
  'Windows', String.raw`C:\repo\file.go`,
  'Regex', String.raw`\d+\.\d+`,
  'UNC', String.raw`\\server\share`,
  'Pipe', 'left|right',
  'Final', 'C:\\repo\\',
  'Escaped final pipe', 'left|',
  'Even slashes', String.raw`two\\`,
  'Odd slashes', String.raw`two\\|pipe`,
], 'table parsing must not silently corrupt Windows paths, regexes or trailing escapes');
const even = flatten(render('Name | Value\n--- | ---\nend\\\\|cell'));
assert.deepEqual(even.filter(node => node.tagName === 'td').map(node => node.textContent),
  ['end\\\\', 'cell'], 'an even backslash run must not escape a cell separator');

// Autolinks must stop at a later '<', not scan the entire remaining tail
// once per opener. Use the full message budget and bound the VM execution so
// a regression fails promptly instead of hanging the test process/browser.
for (const unit of ['<https://', '<mailto:']) {
  const body = unit.repeat(Math.floor((256 * 1024) / unit.length));
  let output;
  assert.doesNotThrow(() => {
    output = vm.runInNewContext('render(body)', {render, body}, {timeout: 2000});
  }, `${unit} repeated to 256 KiB must not block the transcript main thread`);
  assert.equal(output.textContent, body, 'unclosed autolinks remain complete visible text');
}
const autolinks = flatten(render('<https://e.x/path?q=1> <http://e.x> <mailto:user@example.com>'));
assert.deepEqual(autolinks.filter(node => node.tagName === 'a').map(node => [node.href, node.textContent]), [
  ['https://e.x/path?q=1', 'https://e.x/path?q=1'],
  ['http://e.x', 'http://e.x'],
  ['mailto:user@example.com', 'user@example.com'],
], 'HTTP, HTTPS and mailto autolinks keep their existing presentation');
const nestedLinks = flatten(render('<mailto:broken<mailto:valid@example.com>'));
assert.deepEqual(nestedLinks.filter(node => node.tagName === 'a').map(node => node.href),
  ['mailto:valid@example.com'], 'an earlier unmatched opener cannot swallow a later valid autolink');

console.log(`richtext safety, table fidelity, bounded nesting and inline scans (${elapsed} ms dense body): ok`);

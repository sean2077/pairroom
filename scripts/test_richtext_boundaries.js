'use strict';

// Exercise the production renderer, not isolated regular expressions. A VM
// deadline stops a regression instead of hanging the JavaScript/CI worker.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
class Node {
  constructor(tag = '#text', value = '') { this.tagName = tag; this.value = value; this.children = []; this.dataset = {}; }
  appendChild(node) { this.children.push(node); return node; }
  append(...nodes) { for (const node of nodes) this.appendChild(node); }
  set textContent(value) { this.value = value; this.children = []; }
  get textContent() { return this.value + this.children.map(node => node.textContent).join(''); }
  setAttribute(name, value) { this[name] = value; }
  addEventListener() {}
}
const context = vm.createContext({
  window: {},
  document: {createElement: tag => new Node(tag), createTextNode: text => new Node('#text', text)},
});
vm.runInContext(fs.readFileSync('internal/webui/assets/richtext.js', 'utf8'), context);
const script = new vm.Script('window.PairRoomRichText.render(parent, source)');
function render(source) {
  context.source = source;
  context.parent = new Node('main');
  script.runInContext(context, {timeout: 5000});
  return context.parent;
}
function all(root, tag) {
  const found = [], pending = [root];
  while (pending.length) {
    const node = pending.pop();
    if (node.tagName === tag) found.push(node);
    pending.push(...node.children);
  }
  return found;
}

// Every example fits the Native 256 KiB body limit. All content must survive;
// clipping/truncation is not an acceptable way to bound parsing work.
const cases = [
  ['heading padding', '# x' + ' '.repeat(128 * 1024) + 'y', 'x' + ' '.repeat(128 * 1024) + 'y'],
  ['invalid fence padding', '~~~' + ' '.repeat(128 * 1024) + '`'],
  ['invalid fence markers', '~'.repeat(128 * 1024) + '`', null],
  ['unclosed automatic links', '<https://'.repeat(28 * 1024)],
  ['unclosed mail links', '<mailto:'.repeat(28 * 1024)],
];
// An optional label lets the unchanged baseline demonstrate each failure
// separately; the normal Make discovery runs every case without arguments.
const selected = process.argv[2];
if (selected) assert.ok(cases.some(([label]) => label === selected), 'unknown regression case');
for (const [label, source, expected = source] of cases) {
  if (selected && label !== selected) continue;
  const started = Date.now();
  let output;
  assert.doesNotThrow(() => { output = render(source); }, `${label}: rendering exceeded the safety deadline`);
  if (expected !== null) {
    assert.equal(output.textContent === expected, true, `${label}: visible text changed`);
  } else {
    // The existing inline strike syntax consumes some tildes; an invalid
    // block fence must still render the whole run and its final backtick.
    assert.ok(output.textContent.endsWith('`'));
    assert.ok(output.textContent.length > source.length / 5);
    assert.equal(all(output, 'code').length, 0);
  }
  console.log(`${label}: ${Date.now() - started} ms`);
}

for (const [source, tag, text] of [
  ['# Heading ###  ', 'h1', 'Heading'],
  ['  ## Heading with  spaces##', 'h2', 'Heading with  spaces'],
  ['#### ###', 'h4', '#'],
  ['# first\u2028second', 'p', '# first\u2028second'],
  ['# ', 'p', '# '],
  ['#  ', 'h1', ' '],
  ['#\u2028\t\u2029', 'h1', '\t'],
  ['##### not a heading', 'p', '##### not a heading'],
]) {
  const output = render(source);
  assert.equal(all(output, tag).length, 1, source);
  assert.equal(output.textContent, text, source);
}
for (const source of ['```js\nliteral <script>\n```', '~~~~js\nliteral <script>\n~~~~']) {
  const output = render(source);
  const blocks = all(output, 'code');
  assert.equal(blocks.length, 1);
  assert.equal(blocks[0].textContent, 'literal <script>');
  assert.equal(blocks[0].dataset.language, 'js');
  assert.equal(all(output, 'script').length, 0);
}
const links = all(render('<https://example.invalid/path> <mailto:a@example.invalid>'), 'a');
assert.deepEqual(links.map(node => node.href).sort(), ['https://example.invalid/path', 'mailto:a@example.invalid']);
assert.equal(all(render('<javascript:alert(1)>'), 'a').length, 0);
console.log('richtext block and autolink boundaries: ok');

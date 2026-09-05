'use strict';

const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const root = path.resolve(__dirname, '..');
const catalogPath = path.join(root, 'internal/webui/assets/catalogs.js');
const context = { window: {} };
vm.runInNewContext(fs.readFileSync(catalogPath, 'utf8'), context, { filename: catalogPath });
const locales = context.window.PairRoomLocales;
if (!locales?.en?.translation || !locales?.['zh-CN']?.translation) throw new Error('shared en/zh-CN catalogs are missing');

const en = locales.en.translation;
const zh = locales['zh-CN'].translation;
const enKeys = Object.keys(en).sort();
const zhKeys = Object.keys(zh).sort();
if (JSON.stringify(enKeys) !== JSON.stringify(zhKeys)) throw new Error('en and zh-CN catalog keys differ');

function placeholders(value) {
  return [...String(value).matchAll(/\{\{\s*([A-Za-z0-9_.-]+)(?:\s*,[^}]*)?\}\}/g)].map((match) => match[1]).sort();
}

for (const key of enKeys) {
  if (!/^(?:ui|common|theme|desktop|errors|room|agent)\.[A-Za-z0-9][A-Za-z0-9._-]*$/.test(key)) throw new Error(`non-semantic catalog key: ${key}`);
  if (/[\u3400-\u9fff]/u.test(en[key])) throw new Error(`English catalog entry contains Chinese: ${key}`);
  if (/<\/?[A-Za-z][^>]*>/.test(en[key]) || /<\/?[A-Za-z][^>]*>/.test(zh[key])) throw new Error(`catalog entry contains HTML: ${key}`);
  if (JSON.stringify(placeholders(en[key])) !== JSON.stringify(placeholders(zh[key]))) {
    throw new Error(`placeholder mismatch for ${key}: ${en[key]} <> ${zh[key]}`);
  }
}

const uiRoots = [
  'internal/service/assets',
  'internal/server/assets',
  'desktop/frontend',
];
const referenced = new Set();
const untranslated = [];
const literalAllowlist = new Set([
  'PairRoom', 'PairRoom Service', 'Claude', 'Codex', 'Claude × Codex', 'JSON', 'EN', '1:1',
  'P', 'R', 'Y', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultracode',
  'untrusted', 'on-request', 'never', 'yolo', 'read-only', 'workspace-write', 'danger-full-access',
]);
function humanCopy(value) {
  const text = String(value || '').trim();
  if (!/[A-Za-z]{2}/.test(text) || literalAllowlist.has(text)) return false;
  if (/^(?:https?:\/\/|\/|[A-Za-z]:[\\/]|#token=|[A-Za-z0-9_.-]+\/[A-Za-z0-9_.\/-]+$)/.test(text)) return false;
  return true;
}
for (const uiRoot of uiRoots) {
  for (const name of fs.readdirSync(path.join(root, uiRoot))) {
    if (!/\.(?:html|js|css)$/.test(name)) continue;
    const rel = path.join(uiRoot, name);
    const text = fs.readFileSync(path.join(root, rel), 'utf8');
    if (/[\u3400-\u9fff]/u.test(text)) throw new Error(`${rel} contains Chinese outside the zh-CN catalog`);
    for (const match of text.matchAll(/data-i18n(?:-(?:title|aria-label|placeholder))?="([^"]+)"/g)) referenced.add(match[1]);
    for (const match of text.matchAll(/\bt\(\s*['"]([^'"]+)['"]/g)) referenced.add(match[1]);
    if (name.endsWith('.html')) {
      const withoutCode = text.replace(/<(?:script|style)\b[\s\S]*?<\/(?:script|style)>/gi, '');
      for (const match of withoutCode.matchAll(/<([a-z][a-z0-9-]*)([^>]*)>([^<>]*)<\/\1>/gi)) {
        const [, tag, attrs, value] = match;
        if (!['code', 'pre'].includes(tag.toLowerCase()) && humanCopy(value) && !/\bdata-i18n=/.test(attrs)) untranslated.push(`${rel}: untranslated <${tag}> text ${JSON.stringify(value.trim())}`);
      }
      for (const match of withoutCode.matchAll(/<([a-z][a-z0-9-]*)\b([^>]*)>/gi)) {
        const [, tag, attrs] = match;
        for (const attr of ['title', 'aria-label', 'placeholder']) {
          const value = attrs.match(new RegExp(`\\b${attr}="([^"]*)"`, 'i'))?.[1];
          if (humanCopy(value) && !new RegExp(`\\bdata-i18n-${attr}=`).test(attrs)) untranslated.push(`${rel}: untranslated ${tag}.${attr} ${JSON.stringify(value)}`);
        }
      }
    } else if (name.endsWith('.js')) {
      const directPatterns = [
        /(?:textContent\s*(?:=|:)|\.title\s*=|placeholder\s*:|eyebrow\s*:)\s*(['"])([^'"\n]+)\1/g,
        /(?:panel|statCard|settingRow|keyValue|toast|setConnection)\(\s*(['"])([^'"\n]+)\1/g,
      ];
      for (const pattern of directPatterns) {
        for (const match of text.matchAll(pattern)) if (humanCopy(match[2])) untranslated.push(`${rel}: untranslated JavaScript literal ${JSON.stringify(match[2])}`);
      }
    }
  }
}
for (const key of referenced) if (!en[key]) throw new Error(`UI references missing i18n key: ${key}`);
if (untranslated.length) throw new Error(`untranslated UI copy:\n${untranslated.join('\n')}`);

const managementUX = fs.readFileSync(path.join(root, 'internal/service/assets/management-ux.js'), 'utf8');
if (!/addEventListener\(['"]pairroom:lang['"],\s*localizeEnhancements\)/.test(managementUX)) throw new Error('Management dynamic chrome does not react to language changes');

const vendor = fs.readFileSync(path.join(root, 'internal/webui/assets/i18next.min.js'));
const checksum = crypto.createHash('sha256').update(vendor).digest('hex');
if (checksum !== '760fee0db13b012bafaef92f6838eef1b5eec3cc6b6ca95549afd0b45231a182') throw new Error(`unexpected i18next 26.4.2 asset checksum: ${checksum}`);

for (const html of ['internal/service/assets/index.html', 'internal/server/assets/index.html', 'desktop/frontend/index.html']) {
  const text = fs.readFileSync(path.join(root, html), 'utf8');
  for (const asset of ['/_pairroom/i18next.min.js', '/_pairroom/catalogs.js', '/_pairroom/i18n.js', '/_pairroom/theme.js']) {
    if (!text.includes(asset)) throw new Error(`${html} does not load ${asset}`);
  }
}

console.log(`i18n contract ok (${enKeys.length} semantic keys)`);

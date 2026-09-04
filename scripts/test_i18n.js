'use strict';

const fs = require('fs');
const path = require('path');

const root = path.resolve(__dirname, '..');
const files = [
  'internal/server/assets/i18n.js',
  'internal/service/assets/i18n.js',
  'internal/server/assets/app.js',
  'internal/service/assets/management.js',
  'internal/server/assets/index.html',
  'internal/service/assets/index.html',
];

for (const rel of files) {
  const text = fs.readFileSync(path.join(root, rel), 'utf8');
  if (rel.endsWith('i18n.js')) {
    for (const marker of ['pairroom.lang', 'ZH_TO_EN', 'function t(', 'setLang', 'language-button']) {
      if (!text.includes(marker)) {
        throw new Error(`${rel} missing ${marker}`);
      }
    }
  }
  if (rel.endsWith('app.js') || rel.endsWith('management.js')) {
    if (!text.includes('const t = (zh, en)') || !text.includes("pairroom:lang")) {
      throw new Error(`${rel} is not wired to PairRoomI18n`);
    }
  }
  if (rel.endsWith('index.html')) {
    if (!text.includes('id="language-button"') || !text.includes('i18n.js')) {
      throw new Error(`${rel} missing language control`);
    }
  }
}

const i18n = fs.readFileSync(path.join(root, 'internal/server/assets/i18n.js'), 'utf8');
if (!i18n.includes("'退出 Room': 'Leave Room'") || !i18n.includes("'启动': 'Start'")) {
  throw new Error('i18n dictionary missing required Room translations');
}

console.log('i18n contract ok');

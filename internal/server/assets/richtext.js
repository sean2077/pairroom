(() => {
  'use strict';

  const t = (key, options) => window.PairRoomI18n ? window.PairRoomI18n.t(key, options) : key;

  function render(parent, source, options = {}) {
    const root = document.createElement('div');
    root.className = 'rich-content';
    parseBlocks(root, normalize(source), options);
    parent.appendChild(root);
    return root;
  }

  function normalize(value) {
    return String(value || '').replace(/\r\n?/g, '\n');
  }

  function parseBlocks(parent, source, options) {
    const lines = source.split('\n');
    let index = 0;
    while (index < lines.length) {
      const line = lines[index];
      if (!line.trim()) {
        index += 1;
        continue;
      }

      const fence = line.match(/^\s{0,3}(```+|~~~+)\s*([^`]*)$/);
      if (fence) {
        const marker = fence[1][0];
        const minLength = fence[1].length;
        const language = fence[2].trim().split(/\s+/)[0] || '';
        const body = [];
        index += 1;
        const closing = new RegExp(`^\\s{0,3}${escapeRegExp(marker)}{${minLength},}\\s*$`);
        while (index < lines.length && !closing.test(lines[index])) {
          body.push(lines[index]);
          index += 1;
        }
        if (index < lines.length) index += 1;
        parent.appendChild(codeBlock(body.join('\n'), language, options));
        continue;
      }

      const heading = line.match(/^\s{0,3}(#{1,4})\s+(.+?)\s*#*\s*$/);
      if (heading) {
        const node = document.createElement(`h${heading[1].length}`);
        parseInline(node, heading[2], options);
        parent.appendChild(node);
        index += 1;
        continue;
      }

      if (/^\s{0,3}(?:-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
        parent.appendChild(document.createElement('hr'));
        index += 1;
        continue;
      }

      if (/^\s{0,3}>\s?/.test(line)) {
        const body = [];
        while (index < lines.length && /^\s{0,3}>\s?/.test(lines[index])) {
          body.push(lines[index].replace(/^\s{0,3}>\s?/, ''));
          index += 1;
        }
        const quote = document.createElement('blockquote');
        parseBlocks(quote, body.join('\n'), options);
        parent.appendChild(quote);
        continue;
      }

      const list = listMatch(line);
      if (list) {
        const ordered = list.ordered;
        const node = document.createElement(ordered ? 'ol' : 'ul');
        if (ordered && list.number > 1) node.start = list.number;
        while (index < lines.length) {
          const item = listMatch(lines[index]);
          if (!item || item.ordered !== ordered) break;
          const li = document.createElement('li');
          const task = item.body.match(/^\[([ xX])\]\s+(.+)$/);
          if (task) {
            li.className = 'task-list-item';
            const checkbox = document.createElement('input');
            checkbox.type = 'checkbox';
            checkbox.checked = task[1].toLowerCase() === 'x';
            checkbox.disabled = true;
            checkbox.setAttribute('aria-label', checkbox.checked ? t("ui.completed") : t("ui.notCompleted"));
            li.appendChild(checkbox);
            const label = document.createElement('span');
            parseInline(label, task[2], options);
            li.appendChild(label);
          } else {
            parseInline(li, item.body, options);
          }
          node.appendChild(li);
          index += 1;
        }
        parent.appendChild(node);
        continue;
      }

      if (looksLikeTable(lines, index)) {
        const tableLines = [lines[index]];
        index += 2;
        while (index < lines.length && lines[index].includes('|') && lines[index].trim()) {
          tableLines.push(lines[index]);
          index += 1;
        }
        parent.appendChild(tableBlock(tableLines, options));
        continue;
      }

      const blockImage = parseImageOnly(line.trim());
      if (blockImage && typeof options.createImage === 'function') {
        parent.appendChild(options.createImage(blockImage.href, blockImage.alt, blockImage.title));
        index += 1;
        continue;
      }

      const paragraph = [];
      while (index < lines.length && lines[index].trim() && !startsBlock(lines, index)) {
        paragraph.push(lines[index]);
        index += 1;
      }
      if (paragraph.length === 0) {
        paragraph.push(lines[index]);
        index += 1;
      }
      const p = document.createElement('p');
      parseInline(p, paragraph.join('\n'), options);
      parent.appendChild(p);
    }
  }

  function listMatch(line) {
    const match = line.match(/^\s{0,3}([-+*]|(\d+)[.)])\s+(.+)$/);
    if (!match) return null;
    return { ordered: Boolean(match[2]), number: Number(match[2] || 1), body: match[3] };
  }

  function startsBlock(lines, index) {
    const line = lines[index] || '';
    return /^\s{0,3}(?:```+|~~~+)/.test(line)
      || /^\s{0,3}#{1,4}\s+/.test(line)
      || /^\s{0,3}>\s?/.test(line)
      || Boolean(listMatch(line))
      || /^\s{0,3}(?:-{3,}|\*{3,}|_{3,})\s*$/.test(line)
      || looksLikeTable(lines, index)
      || Boolean(parseImageOnly(line.trim()));
  }

  function looksLikeTable(lines, index) {
    if (index + 1 >= lines.length || !lines[index].includes('|')) return false;
    const separator = trimOuterPipes(lines[index + 1]);
    if (!separator.includes('|')) return false;
    return splitTableRow(separator).every((cell) => /^\s*:?-{3,}:?\s*$/.test(cell));
  }

  function tableBlock(lines, options) {
    const wrap = document.createElement('div');
    wrap.className = 'rich-table-wrap';
    const table = document.createElement('table');
    const head = document.createElement('thead');
    const body = document.createElement('tbody');
    const headerRow = document.createElement('tr');
    splitTableRow(lines[0]).forEach((value) => {
      const th = document.createElement('th');
      parseInline(th, value, options);
      headerRow.appendChild(th);
    });
    head.appendChild(headerRow);
    lines.slice(1).forEach((line) => {
      const tr = document.createElement('tr');
      splitTableRow(line).forEach((value) => {
        const td = document.createElement('td');
        parseInline(td, value, options);
        tr.appendChild(td);
      });
      body.appendChild(tr);
    });
    table.append(head, body);
    wrap.appendChild(table);
    return wrap;
  }

  function trimOuterPipes(line) {
    return line.trim().replace(/^\|/, '').replace(/\|$/, '');
  }

  function splitTableRow(line) {
    const body = trimOuterPipes(line);
    const cells = [];
    let current = '';
    let escaped = false;
    for (const char of body) {
      if (escaped) {
        current += char;
        escaped = false;
      } else if (char === '\\') {
        escaped = true;
      } else if (char === '|') {
        cells.push(current.trim());
        current = '';
      } else {
        current += char;
      }
    }
    cells.push(current.trim());
    return cells;
  }

  function codeBlock(value, language, options) {
    const wrap = document.createElement('div');
    wrap.className = 'code-block';
    const head = document.createElement('div');
    head.className = 'code-block-head';
    const label = document.createElement('span');
    label.textContent = language || 'code';
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.className = 'code-copy';
    copy.textContent = t("ui.copy");
    copy.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(value);
        copy.textContent = t("ui.copied8f6f8d9");
        setTimeout(() => { copy.textContent = t("ui.copy"); }, 1200);
      } catch {
        if (typeof options.onCopyError === 'function') options.onCopyError(value);
      }
    });
    head.append(label, copy);
    const pre = document.createElement('pre');
    const code = document.createElement('code');
    if (language) code.dataset.language = language;
    code.textContent = value;
    pre.appendChild(code);
    wrap.append(head, pre);
    return wrap;
  }

  function parseInline(parent, source, options) {
    const patterns = [
      { type: 'image', regex: /!\[([^\]]*)\]\(([^\s)]+)(?:\s+["']([^"']*)["'])?\)/g },
      { type: 'link', regex: /\[([^\]]+)\]\(([^\s)]+)(?:\s+["']([^"']*)["'])?\)/g },
      { type: 'autolink', regex: /<(https?:\/\/[^>\s]+|mailto:[^>\s]+)>/gi },
      { type: 'bare-url', regex: /https?:\/\/[^\s<>()]+[^\s<>().,;:!?]/gi },
      { type: 'code', regex: /`([^`\n]+)`/g },
      { type: 'strong', regex: /(?:\*\*|__)(.+?)(?:\*\*|__)/g },
      { type: 'strike', regex: /~~(.+?)~~/g },
      { type: 'em', regex: /(?<![\w*])\*([^*\n]+)\*(?!\*)|(?<![\w_])_([^_\n]+)_(?!_)/g },
      { type: 'mention', regex: /@(?:claude|codex|all|peer|human|user)\b/gi },
    ];
    let cursor = 0;
    while (cursor < source.length) {
      let winner = null;
      for (const pattern of patterns) {
        pattern.regex.lastIndex = cursor;
        const match = pattern.regex.exec(source);
        if (!match) continue;
        if (!winner || match.index < winner.match.index || (match.index === winner.match.index && match[0].length > winner.match[0].length)) {
          winner = { pattern, match };
        }
      }
      if (!winner) {
        appendTextWithBreaks(parent, source.slice(cursor));
        break;
      }
      if (winner.match.index > cursor) appendTextWithBreaks(parent, source.slice(cursor, winner.match.index));
      const { type } = winner.pattern;
      const match = winner.match;
      if (type === 'mention') {
        const node = document.createElement('span');
        node.className = 'mention';
        node.textContent = match[0];
        parent.appendChild(node);
      } else if (type === 'code') {
        const node = document.createElement('code');
        node.textContent = match[1];
        parent.appendChild(node);
      } else if (type === 'strong' || type === 'em' || type === 'strike') {
        const tag = type === 'strong' ? 'strong' : type === 'strike' ? 'del' : 'em';
        const node = document.createElement(tag);
        parseInline(node, match[1] || match[2] || '', options);
        parent.appendChild(node);
      } else if (type === 'link') {
        appendLink(parent, match[2], match[1], match[3] || '');
      } else if (type === 'autolink') {
        appendLink(parent, match[1], match[1].replace(/^mailto:/i, ''), '');
      } else if (type === 'bare-url') {
        appendLink(parent, match[0], match[0], '');
      } else if (type === 'image') {
        if (typeof options.createImage === 'function') parent.appendChild(options.createImage(match[2], match[1], match[3] || ''));
        else appendTextWithBreaks(parent, match[0]);
      }
      cursor = match.index + match[0].length;
    }
  }

  function appendLink(parent, rawHref, label, title) {
    const href = safeLink(rawHref);
    if (!href) {
      parent.appendChild(document.createTextNode(label));
      return;
    }
    const node = document.createElement('a');
    node.href = href;
    node.textContent = label;
    node.title = title || '';
    if (/^https?:/i.test(href)) {
      node.target = '_blank';
      node.rel = 'noopener noreferrer';
    }
    parent.appendChild(node);
  }

  function parseImageOnly(value) {
    const match = value.match(/^!\[([^\]]*)\]\(([^\s)]+)(?:\s+["']([^"']*)["'])?\)$/);
    if (!match) return null;
    return { alt: match[1], href: match[2], title: match[3] || '' };
  }

  function safeLink(value) {
    const href = String(value || '').trim();
    if (/^(https?:|mailto:)/i.test(href) || href.startsWith('#')) return href;
    return '';
  }

  function appendTextWithBreaks(parent, value) {
    const parts = String(value).split('\n');
    parts.forEach((part, index) => {
      if (index) parent.appendChild(document.createElement('br'));
      if (part) parent.appendChild(document.createTextNode(part));
    });
  }

  function escapeRegExp(value) {
    return String(value).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  }

  window.PairRoomRichText = { render };
})();

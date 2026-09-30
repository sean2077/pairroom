'use strict';

const translated = [...document.querySelectorAll('[data-i18n]')];
const english = Object.fromEntries(translated.map(element => [element.dataset.i18n,
  element.dataset.i18nAttr ? element.getAttribute(element.dataset.i18nAttr) : element.innerHTML]));
const englishTitle = document.title;
const description = document.querySelector('meta[name="description"]');
const englishDescription = description.content;
const status = document.getElementById('status');
const languageButton = document.getElementById('language');
let language = 'en';
let chinese;
let revision = 0;
let statusTimer;

function notify(message) {
  clearTimeout(statusTimer);
  status.textContent = message;
  statusTimer = setTimeout(() => { status.textContent = ''; }, 6000);
}

async function setLanguage(next, updateUrl = true) {
  const currentRevision = ++revision;
  let messages = english;
  if (next === 'zh-CN') {
    try {
      if (!chinese) {
        const response = await fetch('./zh-CN.json');
        if (!response.ok) throw new Error('Translation unavailable');
        const result = await response.json();
        if (!Object.keys(english).every(key => typeof result[key] === 'string')) throw new Error('Incomplete translation');
        chinese = result;
      }
      messages = chinese;
    } catch {
      if (currentRevision !== revision) return;
      notify('中文暂时无法加载，请重试。 Chinese could not load. Please try again.');
      return;
    }
  }
  if (currentRevision !== revision) return;
  translated.forEach(element => {
    if (element.dataset.i18nAttr) element.setAttribute(element.dataset.i18nAttr, messages[element.dataset.i18n]);
    else if (next === 'en') element.innerHTML = english[element.dataset.i18n];
    else {
      // Translation is text, never executable markup. Only explicit newlines become breaks.
      element.replaceChildren(...messages[element.dataset.i18n].split('\n').flatMap((line, index) =>
        index ? [document.createElement('br'), document.createTextNode(line)] : [document.createTextNode(line)]));
    }
  });
  document.querySelectorAll('[data-screen]').forEach(image => {
    image.src = `./images/${image.dataset.screen}${next === 'zh-CN' ? '-zh' : ''}.png`;
    image.closest('[data-full-image]').href = image.src;
  });
  document.documentElement.lang = next;
  document.title = next === 'en' ? englishTitle : chinese.pageTitle;
  description.content = next === 'en' ? englishDescription : chinese.pageDescription;
  languageButton.textContent = next === 'en' ? '中文' : 'EN';
  languageButton.lang = next === 'en' ? 'zh-CN' : 'en';
  languageButton.setAttribute('aria-label', next === 'en' ? '切换到简体中文' : 'Switch to English');
  language = next;
  status.textContent = '';
  if (updateUrl) {
    const url = new URL(location.href);
    url.searchParams.set('lang', next);
    history.replaceState(null, '', url);
  }
}

languageButton.addEventListener('click', () => { void setLanguage(language === 'en' ? 'zh-CN' : 'en'); });
window.addEventListener('popstate', () => {
  void setLanguage(new URLSearchParams(location.search).get('lang') === 'zh-CN' ? 'zh-CN' : 'en', false);
});

function selectPanel(button, buttonAttribute, panelAttribute) {
  const value = button.dataset[buttonAttribute];
  document.querySelectorAll(`[data-${buttonAttribute}]`).forEach(item => {
    item.setAttribute('aria-pressed', String(item === button));
  });
  document.querySelectorAll(`[data-${panelAttribute}]`).forEach(panel => {
    panel.hidden = panel.dataset[panelAttribute] !== value;
  });
}

document.querySelectorAll('[data-view]').forEach(button => {
  button.addEventListener('click', () => selectPanel(button, 'view', 'shot'));
});
document.querySelectorAll('[data-platform]').forEach(button => {
  button.addEventListener('click', () => selectPanel(button, 'platform', 'install'));
});
document.querySelectorAll('[data-copy]').forEach(button => {
  button.addEventListener('click', async () => {
    const code = document.getElementById(button.dataset.copy);
    try {
      if (!navigator.clipboard?.writeText) throw new Error('Clipboard unavailable');
      await navigator.clipboard.writeText(code.textContent);
      notify(language === 'en' ? 'Command copied.' : '命令已复制。');
    } catch {
      // Leave the actual command selected for manual copying on denied or unsupported clipboards.
      const range = document.createRange();
      range.selectNodeContents(code);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      notify(language === 'en' ? 'Could not copy. The command is selected; copy it manually.' : '未能复制。命令已选中，请手动复制。');
    }
  });
});

if (new URLSearchParams(location.search).get('lang') === 'zh-CN') void setLanguage('zh-CN', false);

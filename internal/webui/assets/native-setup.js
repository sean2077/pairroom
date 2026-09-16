(() => {
  'use strict';

  function text(tag, key) {
    const element = document.createElement(tag);
    element.dataset.i18n = key;
    element.textContent = window.PairRoomI18n.t(key);
    return element;
  }

  function command(value) {
    const pre = document.createElement('pre');
    const code = document.createElement('code');
    code.textContent = value;
    pre.append(code);
    return pre;
  }

  function mount(host) {
    const details = document.createElement('details');
    details.className = 'native-setup';
    details.append(text('summary', 'room.native.setup.title'));
    const body = document.createElement('div');
    body.className = 'native-setup-body';
    details.append(body);
    body.append(text('p', 'room.native.setup.intro'));
    const steps = document.createElement('ol');
    const content = [
      ['room.native.setup.prerequisites', 'room.native.setup.prerequisitesBody', 'pairroom version\ngit --version'],
      ['room.native.setup.install', 'room.native.setup.installBody', 'pairroom relay install'],
      ['room.native.setup.connect', 'room.native.setup.connectBody', 'pairroom relay bind --create --name "<topic>"'],
      ['room.native.setup.use', 'room.native.setup.useBody', 'pairroom relay status\npairroom relay wait'],
    ];
    for (const [heading, description, example] of content) {
      const step = document.createElement('li');
      step.append(text('h3', heading), text('p', description), command(example));
      if (heading === 'room.native.setup.connect') {
        step.append(text('p', 'room.native.setup.join'), command('pairroom relay bind'));
      }
      steps.append(step);
    }
    body.append(steps, text('p', 'room.native.setup.recovery'), text('p', 'room.native.setup.boundary'));
    host.append(details);
    const source = document.getElementById(host.dataset.nativeSource || '');
    if (source) {
      const sync = () => { host.hidden = source.hidden; };
      sync();
      new MutationObserver(sync).observe(source, { attributes: true, attributeFilter: ['hidden'] });
    }
  }

  function start() {
    for (const host of document.querySelectorAll('[data-native-setup]')) mount(host);
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start, { once: true });
  } else {
    start();
  }
})();

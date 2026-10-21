(() => {
  'use strict';

  // No localStorage or installation during rendering. Status is project-file
  // configuration only; vendor approval and actual session readiness are separate.
  window.PairRoomProjectSetup = { create({ t, node, actionButton, api, confirm, isCurrent }) {
    let host, project, status = null, pending = false, error = '', generation = 0;
    let selected = 'claude', loaded = false;
    const names = { claude: 'Claude Code', codex: 'Codex', grok: 'Grok Build', gemini: 'Gemini CLI' };
    const endpoint = (id) => `/api/v1/projects/${encodeURIComponent(id)}/native-setup`;
    const paths = (item) => item.files?.length ? item.files : [item.path];

    function paint() {
      if (!host) return;
      const restoreRuntimeFocus = typeof document !== 'undefined' && document.activeElement?.id === 'project-native-runtime';
      const runtime = node('select', { id: 'project-native-runtime', disabled: pending,
        'aria-label': t('workspace.nativeSetup.runtime'), onChange: (event) => { selected = event.target.value; paint(); } },
        ...Object.entries(names).map(([value, name]) => node('option', { value, textContent: name })));
      runtime.value = selected;
      const row = status?.runtimes?.find((item) => item.runtime === selected);
      const busy = pending || !project.available;
      host.replaceChildren(...[
        node('header', { className: 'project-setup-heading' },
          node('h3', { className: 'flush-heading', textContent: t('workspace.nativeSetup.title') }),
          actionButton(t(pending ? 'workspace.nativeSetup.working' : 'workspace.nativeSetup.check'), check, 'secondary-button compact-button', busy)),
        node('p', { className: 'muted', textContent: t('workspace.nativeSetup.scope') }),
        node('label', { className: 'project-setup-runtime', htmlFor: 'project-native-runtime' },
          node('span', { textContent: t('workspace.nativeSetup.runtime') }), runtime),
        error ? node('p', { className: 'form-error', role: 'alert', textContent: error }) : null,
        !project.available ? node('p', { role: 'status', textContent: t('workspace.nativeSetup.unavailable') })
          : !row ? node('p', { role: 'status', textContent: t(pending ? 'workspace.nativeSetup.checking' : 'workspace.nativeSetup.notChecked') })
            : node('div', { className: 'project-setup-items' }, ...['skill', 'hook'].map((component) => {
              const item = row[component];
              const ready = item.state === 'current' || item.state === 'shared';
              return node('section', { className: 'project-setup-item', 'data-setup-component': component },
                node('div', { className: 'project-setup-item-heading' },
                  node('strong', { textContent: t(`workspace.nativeSetup.${component}`) }),
                  node('span', { className: `project-setup-state ${ready ? 'is-ready' : ''}`, role: 'status', textContent: t(`workspace.nativeSetup.state.${item.state}`) })),
                ...paths(item).map((path) => node('code', { className: 'project-setup-path', textContent: path })),
                component === 'skill' && item.files?.length > 1 ? node('p', { className: 'muted', textContent: t('workspace.nativeSetup.skillContents') }) : null,
                item.reason ? node('p', { className: 'muted', textContent: t(`workspace.nativeSetup.reason.${item.reason}`) }) : null,
                actionButton(t(item.needs_confirmation ? 'workspace.nativeSetup.review' : 'workspace.nativeSetup.install'), () => install(selected, item),
                  'secondary-button compact-button', busy || !item.can_install));
            })),
        node('p', { className: 'project-setup-boundary', textContent: t('workspace.nativeSetup.approval') })
      ].filter(Boolean));
      if (restoreRuntimeFocus && !runtime.disabled) runtime.focus({ preventScroll: true });
    }

    async function check() {
      if (pending || !project.available) return;
      const id = project.id, epoch = generation, active = isCurrent();
      pending = true; error = ''; paint();
      try {
        const result = await api(endpoint(id));
        if (epoch !== generation || !active()) return;
        status = result; loaded = true;
      } catch (err) {
        if (epoch !== generation || !active()) return;
        status = null; loaded = true; error = err.message;
      } finally {
        if (epoch === generation) { pending = false; paint(); }
      }
    }

    function install(kind, item) {
      if (pending || !project.available || !item.can_install) return;
      // Capture the project, component and revision BEFORE opening the modal.
      // Changing selection/navigation must never retarget an existing approval.
      const id = project.id, root = project.root, epoch = generation, active = isCurrent();
      const request = { runtime: kind, component: item.component, revision: item.revision, confirm_replace: item.needs_confirmation };
      const apply = async () => {
        if (epoch !== generation || !active()) throw new Error(t('workspace.nativeSetup.changed'));
        if (pending) return;
        pending = true; error = ''; paint();
        try {
          const result = await api(endpoint(id), { method: 'POST', body: JSON.stringify(request) });
          if (epoch !== generation || !active()) return;
          status = result; loaded = true;
        } catch (err) {
          if (epoch !== generation || !active()) return;
          // Never auto-retry a write, even on unknown transport outcome.
          status = null; loaded = true;
          error = `${err.message} ${t('workspace.nativeSetup.checkBeforeRetry')}`;
        } finally {
          if (epoch === generation) { pending = false; paint(); }
        }
      };
      if (item.needs_confirmation) {
        confirm({ title: t('workspace.nativeSetup.confirmTitle'),
          message: t('workspace.nativeSetup.confirmBody', { runtime: names[kind], component: t(`workspace.nativeSetup.${item.component}`), root, paths: paths(item).join('\n') }),
          detail: t(item.component === 'skill' ? 'workspace.nativeSetup.skillReplace' : 'workspace.nativeSetup.hookMerge'),
          acknowledgement: t('workspace.nativeSetup.acknowledge'), label: t('workspace.nativeSetup.confirm'), tone: 'danger', action: apply });
      } else return apply();
    }

    return { mount(value) {
      const changed = !project || project.id !== value.id || project.root !== value.root || project.available !== value.available;
      if (changed) { generation++; status = null; pending = false; loaded = false; error = ''; }
      project = value;
      host = node('section', { className: 'panel project-native-setup', 'aria-label': t('workspace.nativeSetup.title') });
      paint();
      if (!loaded && !pending && project.available) queueMicrotask(check);
      return host;
    } };
  } };
})();

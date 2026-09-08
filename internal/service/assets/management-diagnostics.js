(() => {
  'use strict';

  // Deliberately no automatic probes, persistence, or native-session access.
  window.PairRoomDiagnostics = { create({ t, node, actionButton, api, confirm, navigate }) {
    let host, snapshot, roomID = '', report = null, controller = null, revision = 0, error = '';
    let actor = 'claude';
    const codes = new Set(['cleanup_failed', 'installed', 'cli_unavailable', 'timeout', 'started', 'responded', 'not_checked', 'workspace_unavailable', 'startup_failed', 'response_failed', 'output_limit', 'interaction_required', 'unexpected_response', 'cancelled', 'authentication_failed', 'quota_or_rate_limit', 'model_unavailable', 'network_failed', 'profile_unavailable', 'application_unavailable', 'application_available', 'registry_unhealthy', 'registry_healthy', 'storage_writable', 'storage_unwritable', 'git_unavailable', 'git_available', 'project_unavailable', 'projects_available', 'room_failed', 'rooms_healthy', 'capacity_queued', 'capacity_available', 'resolver_unavailable', 'provider_unavailable', 'selection_valid', 'mock']);
    const ids = new Set(['cleanup', 'installation', 'startup', 'response', 'selection', 'application', 'registry', 'storage', 'git', 'projects', 'rooms', 'capacity']);
    const statuses = new Set(['pass', 'warn', 'fail', 'skipped']);
    const runtimes = new Set(['claude', 'codex', 'grok']);

    // A second allowlist at the download boundary also rejects malformed API
    // responses. Never export the Service snapshot or form/Room metadata.
    function safeReport(value) {
      if (value?.schema !== 1 || !Array.isArray(value.checks) || value.checks.length > 64 || !['environment', 'runtime'].includes(value.mode) || !['service_defaults', 'default_profile', 'room'].includes(value.scope) || !/^\d+\.\d+\.\d+$/.test(value.version) || !/^(linux|darwin|windows)\/[a-z0-9]+$/.test(value.platform) || !Number.isFinite(Date.parse(value.generated_at))) throw new Error('invalid_report');
      return {
        schema: 1, version: value.version, platform: value.platform,
        generated_at: new Date(value.generated_at).toISOString(), mode: value.mode, scope: value.scope,
        checks: value.checks.map((check) => {
          if (!ids.has(check.id) || !statuses.has(check.status) || !codes.has(check.code)) throw new Error('invalid_report');
          const clean = { id: check.id, status: check.status, code: check.code, duration_ms: Math.max(0, Math.min(120000, Number(check.duration_ms) || 0)) };
          if (runtimes.has(check.runtime)) clean.runtime = check.runtime;
          if (['claude', 'codex'].includes(check.actor)) clean.actor = check.actor;
          if (/^\d+\.\d+\.\d+$/.test(check.version || '')) clean.version = check.version;
          return clean;
        }),
      };
    }

    function paint() {
      if (!host?.isConnected) return;
      const focusID = host.contains(document.activeElement) ? document.activeElement.id : '';
      const busy = Boolean(controller);
      const room = snapshot.rooms?.find((value) => value.id === roomID);
      const scopeSelect = node('select', { id: 'diagnostic-scope', disabled: busy, onChange: (event) => navigate(event.target.value ? `#/settings/diagnostics/${encodeURIComponent(event.target.value)}` : '#/settings/diagnostics') },
        node('option', { value: '', textContent: t('diagnostics.defaultPair') }),
        ...(snapshot.rooms || []).filter((item) => item.agents?.claude && item.agents?.codex).map((item) => node('option', { value: item.id, textContent: item.name }))
      );
      scopeSelect.value = roomID;
      const actorSelect = node('select', { id: 'diagnostic-actor', disabled: busy, onChange: (event) => { actor = event.target.value; } },
        ...['claude', 'codex'].map((id, index) => node('option', { value: id, textContent: `${t(index ? 'agent.agent2' : 'agent.agent1')}${room?.agents?.[id]?.runtime ? ` · ${room.agents[id].runtime}` : ''}` }))
      );
      actorSelect.value = actor;
      const live = actionButton(t('diagnostics.testRuntime'), () => {
        const selectedActor = actor;
        const confirmedScope = roomID;
        const confirmedRevision = revision;
        confirm({
          title: t('diagnostics.confirmTitle'), message: t('diagnostics.liveDisclosure'),
          detail: t('diagnostics.nativeBoundary'), tone: 'primary', label: t('diagnostics.testRuntime'),
          acknowledgement: t('diagnostics.acknowledgement'),
          // Start without holding the confirmation dialog open; the diagnostic
          // page owns progress and cancellation, not a modal spinner.
          action: () => { if (confirmedScope === roomID && confirmedRevision === revision) void run('runtime', selectedActor); },
        });
      }, 'primary-button', busy || Boolean(roomID && !room?.agents?.claude));
      live.id = 'diagnostic-live';
      const environment = actionButton(t('diagnostics.checkEnvironment'), () => run('environment'), 'secondary-button', busy);
      environment.id = 'diagnostic-environment';
      const controls = node('section', { className: 'panel diagnostic-controls', 'aria-labelledby': 'diagnostic-controls-title' },
        node('div', {}, node('p', { className: 'eyebrow', textContent: t('diagnostics.eyebrow') }), node('h2', { id: 'diagnostic-controls-title', className: 'flush-heading', textContent: t('diagnostics.startTitle') }), node('p', { className: 'muted', textContent: t('diagnostics.intro') })),
        node('div', { className: 'diagnostic-fields' },
          node('label', { for: 'diagnostic-scope' }, node('span', { textContent: t('diagnostics.scope') }), scopeSelect),
          node('label', { for: 'diagnostic-actor' }, node('span', { textContent: t('diagnostics.agent') }), actorSelect)
        ),
        node('div', { className: 'section-actions' }, environment, live, busy ? actionButton(t('diagnostics.cancel'), () => cancel(true), 'text-button') : null),
        node('p', { className: 'diagnostic-note', textContent: t('diagnostics.liveDisclosure') })
      );
      const content = node('div', { className: 'view-stack diagnostic-page' }, controls);
      const notice = node('div', { id: 'diagnostic-status', className: `callout ${error ? 'danger' : 'boundary'}`, role: error ? 'alert' : 'status', 'aria-live': 'polite' },
        node('strong', { textContent: t(error ? `diagnostics.error.${error}` : (busy ? 'diagnostics.running' : 'diagnostics.passiveBoundary')) }),
        node('span', { textContent: t(busy ? 'diagnostics.runningHelp' : 'diagnostics.nativeBoundary') })
      );
      content.append(notice);
      if (report) {
        const failed = report.checks.filter((check) => check.status === 'fail').length;
        const warned = report.checks.filter((check) => check.status === 'warn').length;
        const responded = report.checks.some((check) => check.code === 'responded' && check.status === 'pass');
        const summary = node('section', { className: 'panel diagnostic-results', 'aria-labelledby': 'diagnostic-report-title' },
          node('div', { className: 'section-heading' }, node('div', {},
            node('h2', { id: 'diagnostic-report-title', className: 'flush-heading', textContent: t(failed ? 'diagnostics.needsAttention' : responded ? 'diagnostics.verified' : 'diagnostics.checked') }),
            node('p', { className: 'muted', textContent: `${new Date(report.generated_at).toLocaleString()} · ${t(`diagnostics.scope.${report.scope}`)} · ${t('diagnostics.resultCounts', { failed, warned })}` })
          ), actionButton(t('diagnostics.download'), download, 'secondary-button compact-button')),
          node('div', { className: 'diagnostic-checks' }, ...report.checks.map((check) => node('article', { className: 'diagnostic-check', 'data-diagnostic-code': check.code },
            node('span', { className: `diagnostic-indicator ${check.status}`, 'aria-hidden': 'true', textContent: ({ pass: '✓', warn: '!', fail: '×', skipped: '—' })[check.status] }),
            node('div', { className: 'diagnostic-check-copy' },
              node('div', { className: 'diagnostic-check-title' }, node('strong', { textContent: [check.runtime, check.actor ? t(check.actor === 'claude' ? 'agent.agent1' : 'agent.agent2') : '', t(`diagnostics.check.${check.id}`)].filter(Boolean).join(' · ') }), node('span', { className: 'badge plain', textContent: t(`diagnostics.status.${check.status}`) })),
              node('p', { textContent: t(`diagnostics.code.${check.code}`) }),
              node('p', { className: 'muted diagnostic-remedy', textContent: t(`diagnostics.help.${check.code}`) })
            ),
            node('span', { className: 'diagnostic-timing', textContent: [check.version ? `v${check.version}` : '', check.duration_ms ? `${check.duration_ms} ms` : ''].filter(Boolean).join(' · ') })
          )))
        );
        content.append(summary, node('p', { className: 'muted diagnostic-note', textContent: t('diagnostics.exportBoundary') }));
      } else if (!busy && !error) {
        content.append(node('section', { className: 'diagnostic-guide' },
          ...['environment', 'startup', 'response'].map((step, index) => node('article', {}, node('span', { className: 'diagnostic-step', textContent: `0${index + 1}` }), node('h3', { textContent: t(`diagnostics.guide.${step}`) }), node('p', { className: 'muted', textContent: t(`diagnostics.guide.${step}Help`) })))
        ));
      }
      host.replaceChildren(content);
      if (focusID) host.querySelector(`#${focusID}`)?.focus({ preventScroll: true });
    }

    async function run(mode, selectedActor) {
      if (controller) return;
      const current = ++revision;
      controller = new AbortController();
      report = null;
      error = '';
      paint();
      try {
        const value = await api('/api/v1/diagnostics', { method: 'POST', signal: controller.signal, body: JSON.stringify({ mode, ...(roomID ? { room_id: roomID } : {}), ...(mode === 'runtime' ? { actor: selectedActor, confirm: true } : {}) }) });
        if (current !== revision) return;
        report = safeReport(value);
      } catch (err) {
        if (current !== revision) return;
        error = err.status === 409 ? 'busy' : err.message === 'invalid_report' ? 'invalid' : err.name === 'AbortError' ? 'cancelled' : 'request';
      } finally {
        if (current === revision) { controller = null; paint(); }
      }
    }

    function cancel(repaint = false) {
      const wasRunning = Boolean(controller);
      ++revision;
      controller?.abort();
      controller = null;
      if (wasRunning) { report = null; error = 'cancelled'; }
      if (repaint) paint();
    }
    function download() {
      if (!report) return;
      const url = URL.createObjectURL(new Blob([JSON.stringify(safeReport(report), null, 2) + '\n'], { type: 'application/json' }));
      const link = node('a', { href: url, download: 'pairroom-diagnostics.json' });
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    }
    return {
      render(container, currentSnapshot, currentRoomID = '') {
        if (roomID !== currentRoomID) { cancel(); report = null; error = ''; }
        host = container; snapshot = currentSnapshot; roomID = currentRoomID;
        paint();
      },
      cancel,
      reset() { cancel(); report = null; error = ''; roomID = ''; host = null; },
    };
  } };
})();

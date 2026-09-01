(() => {
  'use strict';

  const title = document.getElementById('title');
  const message = document.getElementById('message');
  const error = document.getElementById('error');
  const retry = document.getElementById('retry');
  const spinner = document.querySelector('.spinner');

  function setLoading(active) {
    spinner.hidden = !active;
    retry.hidden = active;
  }

  async function connect() {
    setLoading(true);
    error.hidden = true;
    error.textContent = '';
    title.textContent = '正在启动 PairRoom Service';
    message.textContent = '正在检查已安装的 daemon；若不存在，将启动安装包内置的本地 sidecar。';

    try {
      const invoke = window.__TAURI__?.core?.invoke;
      if (typeof invoke !== 'function') throw new Error('Tauri IPC 不可用；请从原生 PairRoom 应用启动。');
      const managementURL = await invoke('bootstrap_pairroom');
      title.textContent = '正在打开 Management Shell';
      message.textContent = '本地 Service 已通过认证检查。';
      window.location.replace(managementURL);
    } catch (cause) {
      const detail = cause instanceof Error ? cause.message : String(cause);
      title.textContent = 'PairRoom Service 启动失败';
      message.textContent = '未修改或恢复任何 service.lock；请按诊断处理后重试。';
      error.textContent = detail;
      error.hidden = false;
      setLoading(false);
    }
  }

  retry.addEventListener('click', connect);
  connect();
})();

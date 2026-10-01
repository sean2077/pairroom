'use strict';

// Opt-in transport evidence only. Never invoke this against an untrusted package.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const { spawn, spawnSync } = require('node:child_process');

const VERSION = '0.62.0';
const OUTPUT_LIMIT = 1024 * 1024;
// This is the entire client protocol, including on error. No generic RPC sender.
const INITIALIZE = JSON.stringify({
  jsonrpc: '2.0', id: 1, method: 'initialize',
  params: {
    protocolVersion: 1,
    clientCapabilities: { fs: {}, terminal: false },
    clientInfo: { name: 'pairroom-no-auth-probe', version: '1' },
  },
}) + '\n';

function isolatedEnvironment(root) {
  const home = path.join(root, 'home');
  const project = path.join(root, 'project');
  const tmp = path.join(root, 'tmp');
  for (const dir of [home, project, tmp]) fs.mkdirSync(dir, { recursive: true });
  // Stop Gemini's ancestor .env search even when the workspace is untrusted.
  fs.writeFileSync(path.join(project, '.env'), '');
  const settings = path.join(root, 'settings.json');
  fs.writeFileSync(settings, JSON.stringify({
    general: { enableAutoUpdate: false, enableAutoUpdateNotification: false },
    privacy: { usageStatisticsEnabled: false },
    telemetry: { enabled: false },
  }));
  const empty = path.join(root, 'empty.json');
  fs.writeFileSync(empty, '{}');
  // Do not spread process.env: API keys, proxy auth, NODE_OPTIONS, npm config,
  // Cloud SDK credentials and IDE integrations must not reach the child.
  const env = {
    HOME: home, USERPROFILE: home,
    APPDATA: path.join(home, 'AppData', 'Roaming'),
    LOCALAPPDATA: path.join(home, 'AppData', 'Local'),
    XDG_CONFIG_HOME: path.join(home, '.config'), XDG_CACHE_HOME: path.join(home, '.cache'),
    CLOUDSDK_CONFIG: path.join(home, 'gcloud'),
    TMPDIR: tmp, TMP: tmp, TEMP: tmp,
    PATH: path.dirname(process.execPath),
    GEMINI_CLI_HOME: home,
    GEMINI_CLI_SYSTEM_SETTINGS_PATH: settings,
    GEMINI_CLI_SYSTEM_DEFAULTS_PATH: empty,
    GEMINI_CLI_NO_RELAUNCH: 'true',
    CI: 'true', NO_COLOR: '1', TERM: 'dumb',
  };
  if (process.platform === 'win32') {
    const systemRoot = process.env.SystemRoot || process.env.SYSTEMROOT;
    if (!systemRoot || !path.isAbsolute(systemRoot)) throw new Error('missing_system_root');
    env.SystemRoot = systemRoot;
    env.WINDIR = systemRoot;
    env.PATH += path.delimiter + path.join(systemRoot, 'System32');
  }
  return { env, cwd: project };
}

function stopTree(child, env) {
  if (!child.pid) return;
  if (process.platform === 'win32') {
    spawnSync(path.join(env.SystemRoot, 'System32', 'taskkill.exe'),
      ['/pid', String(child.pid), '/t', '/f'], { env, windowsHide: true, stdio: 'ignore', timeout: 5000 });
  } else {
    try { process.kill(-child.pid, 'SIGKILL'); } catch { /* Already exited. */ }
  }
  child.kill('SIGKILL');
}

function capture(entry, args, isolation, timeoutMs, initialize = false) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [entry, ...args], {
      ...isolation, windowsHide: true, detached: process.platform !== 'win32', stdio: ['pipe', 'pipe', 'pipe'],
    });
    let stdout = '', bytes = 0, settled = false, value, failure;
    const finish = (error, result) => {
      if (settled) return;
      settled = true;
      failure = error;
      value = result;
      clearTimeout(timer);
      child.stdin.destroy();
      stopTree(child, isolation.env);
    };
    const timer = setTimeout(() => finish('timeout'), timeoutMs);
    child.on('error', () => finish('spawn_failed'));
    child.stdin.on('error', () => finish('stdin_failed'));
    child.stderr.on('data', (chunk) => {
      bytes += chunk.length;
      if (bytes > OUTPUT_LIMIT) finish('output_limit');
    });
    child.stdout.setEncoding('utf8');
    child.stdout.on('data', (chunk) => {
      if (settled) return;
      bytes += Buffer.byteLength(chunk);
      if (bytes > OUTPUT_LIMIT) return finish('output_limit');
      stdout += chunk;
      if (!initialize) return;
      const newline = stdout.indexOf('\n');
      if (newline < 0) return;
      try {
        const message = JSON.parse(stdout.slice(0, newline));
        if (message.jsonrpc !== '2.0' || message.id !== 1 || message.method || message.error || !message.result) {
          return finish('unexpected_initialize_response');
        }
        finish(null, message.result);
      } catch { finish('invalid_json'); }
    });
    // Version/help normally exit themselves. This waits for the direct child's
    // stdio closure; it is not containment of arbitrary detached descendants.
    child.on('close', (code) => {
      clearTimeout(timer);
      if (!settled) {
        failure = initialize ? 'missing_initialize_response' : code !== 0 ? 'command_failed' : null;
        value = stdout;
      }
      if (failure) reject(new Error(failure));
      else resolve(value);
    });
    if (initialize) child.stdin.write(INITIALIZE);
    else child.stdin.end();
  });
}

function capabilityEvidence(result) {
  if (result.protocolVersion !== 1 || result.agentInfo?.name !== 'gemini-cli' || result.agentInfo?.version !== VERSION) {
    throw new Error('incompatible_initialize_response');
  }
  const source = result.agentCapabilities;
  if (!source || typeof source !== 'object' || Array.isArray(source)) throw new Error('invalid_capabilities');
  const evidence = {};
  for (const [key, value] of [
    ['loadSessionAdvertised', source.loadSession],
    ['image', source.promptCapabilities?.image],
    ['audio', source.promptCapabilities?.audio],
    ['embeddedContext', source.promptCapabilities?.embeddedContext],
    ['mcpHTTP', source.mcpCapabilities?.http],
    ['mcpSSE', source.mcpCapabilities?.sse],
  ]) {
    if (value !== undefined && typeof value !== 'boolean') throw new Error('invalid_capabilities');
    evidence[key] = value ?? null;
  }
  return evidence;
}

async function probe(packageDir, { timeoutMs = 30000, temporaryParent = os.tmpdir() } = {}) {
  const report = {
    schemaVersion: 1, scope: 'gemini-no-auth-initialize-only',
    platform: process.platform, arch: process.arch, nodeVersion: process.versions.node,
    expectedVersion: VERSION, authenticatedE2E: false, status: 'failed', stage: 'package',
  };
  let root;
  try {
    const metadata = JSON.parse(fs.readFileSync(path.join(packageDir, 'package.json'), 'utf8'));
    if (metadata.name !== '@google/gemini-cli' || metadata.version !== VERSION || metadata.bin?.gemini !== 'bundle/gemini.js') {
      throw new Error('package_mismatch');
    }
    const entry = path.resolve(packageDir, 'bundle/gemini.js');
    report.entrySHA256 = crypto.createHash('sha256').update(fs.readFileSync(entry)).digest('hex');
    root = fs.mkdtempSync(path.join(temporaryParent, 'pairroom-no-auth-'));
    const isolation = isolatedEnvironment(root);
    report.stage = 'version';
    const version = (await capture(entry, ['--version'], isolation, timeoutMs)).trim();
    if (version !== VERSION) throw new Error('version_mismatch');
    report.observedVersion = version;
    report.stage = 'help';
    const help = await capture(entry, ['--help'], isolation, timeoutMs);
    const acpFlag = /(?:^|\s)--acp(?:[\s,=]|$)/m.test(help) ? '--acp' :
      /(?:^|\s)--experimental-acp(?:[\s,=]|$)/m.test(help) ? '--experimental-acp' : null;
    if (!acpFlag) throw new Error('acp_not_advertised');
    report.acpFlag = acpFlag;
    report.stage = 'initialize';
    const result = await capture(entry, [acpFlag], isolation, timeoutMs, true);
    report.capabilities = capabilityEvidence(result);
    report.protocolVersion = 1;
    report.requestMethods = ['initialize'];
    report.status = 'passed';
    report.stage = 'complete';
  } catch (error) {
    // Never persist raw CLI output, error messages, environment values or paths.
    const safeErrors = new Set(['package_mismatch', 'missing_system_root', 'timeout', 'spawn_failed',
      'stdin_failed', 'output_limit', 'unexpected_initialize_response', 'invalid_json',
      'missing_initialize_response', 'command_failed', 'incompatible_initialize_response',
      'invalid_capabilities', 'version_mismatch', 'acp_not_advertised']);
    report.failure = safeErrors.has(error.message) ? error.message : 'probe_setup_failed';
  } finally {
    if (root) fs.rmSync(root, { recursive: true, force: true });
  }
  return report;
}

if (require.main === module) {
  const [packageDir, output, ...extra] = process.argv.slice(2);
  if (!packageDir || !output || extra.length) {
    console.error('Usage: node scripts/probe_gemini_no_auth.js PACKAGE_DIRECTORY OUTPUT_JSON');
    process.exitCode = 2;
  } else {
    probe(path.resolve(packageDir)).then((report) => {
      fs.mkdirSync(path.dirname(path.resolve(output)), { recursive: true });
      fs.writeFileSync(output, JSON.stringify(report, null, 2) + '\n');
      console.log(`Gemini no-auth probe: ${report.status} (${report.stage})`);
      process.exitCode = report.status === 'passed' ? 0 : 1;
    }).catch(() => {
      console.error('Gemini no-auth probe: evidence_write_or_cleanup_failed');
      process.exitCode = 1;
    });
  }
}

module.exports = { probe };

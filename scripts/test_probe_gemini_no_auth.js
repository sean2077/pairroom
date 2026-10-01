'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { test } = require('node:test');
const { probe } = require('./probe_gemini_no_auth');

function fixture(t, mode = 'success') {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'pairroom probe fixture '));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const packageDir = path.join(root, 'package');
  const record = path.join(root, 'wire.jsonl');
  fs.mkdirSync(path.join(packageDir, 'bundle'), { recursive: true });
  fs.writeFileSync(path.join(packageDir, 'package.json'), JSON.stringify({
    name: '@google/gemini-cli', version: '0.62.0', bin: { gemini: 'bundle/gemini.js' },
  }));
  fs.writeFileSync(path.join(packageDir, 'bundle/gemini.js'), `
    const fs = require('node:fs');
    const readline = require('node:readline');
    const mode = ${JSON.stringify(mode)};
    const record = ${JSON.stringify(record)};
    function log(value) { fs.appendFileSync(record, JSON.stringify(value) + '\\n'); }
    log({ args: process.argv.slice(2), env: process.env, cwd: process.cwd(),
      envFile: fs.readFileSync('.env', 'utf8'),
      settings: JSON.parse(fs.readFileSync(process.env.GEMINI_CLI_SYSTEM_SETTINGS_PATH, 'utf8')) });
    if (process.argv[2] === '--version') {
      console.log(mode === 'bad-version' ? '0.62.1 SECRET_FIXTURE' : '0.62.0');
      process.exit(mode === 'version-exit' ? 2 : 0);
    }
    if (process.argv[2] === '--help') {
      console.log(mode === 'no-acp' ? '--model' : mode === 'legacy' ? '--experimental-acp' : '--acp');
      process.exit(0);
    }
    readline.createInterface({ input: process.stdin }).on('line', (line) => {
      const request = JSON.parse(line);
      log({ request });
      if (mode === 'timeout') return;
      if (mode === 'exit') process.exit(0);
      if (mode === 'malformed') return console.log('SECRET_FIXTURE');
      if (mode === 'overflow') return console.log('x'.repeat(1024 * 1024 + 1));
      if (mode === 'server-request') return console.log(JSON.stringify({
        jsonrpc: '2.0', id: 2, method: 'session/request_permission', params: { secret: 'SECRET_FIXTURE' },
      }));
      if (mode === 'rpc-error') return console.log(JSON.stringify({
        jsonrpc: '2.0', id: 1, error: { code: -32000, message: 'SECRET_FIXTURE' },
      }));
      console.log(JSON.stringify({ jsonrpc: '2.0', id: mode === 'wrong-id' ? 2 : request.id,
        result: {
          protocolVersion: mode === 'wrong-protocol' ? 2 : 1,
          agentInfo: { name: 'gemini-cli', version: '0.62.0' },
          agentCapabilities: {
            loadSession: mode === 'bad-capability' ? 'SECRET_FIXTURE' : true,
            promptCapabilities: { image: true, audio: false, embeddedContext: true },
            mcpCapabilities: { http: true, sse: false }, extra: 'SECRET_FIXTURE',
          }, authMethods: [{ id: 'SECRET_FIXTURE' }],
        },
      }));
    });
  `);
  return { root, packageDir, records: () => fs.existsSync(record) ?
    fs.readFileSync(record, 'utf8').trim().split('\n').map((line) => JSON.parse(line)) : [] };
}

function assertInitializeOnly(records) {
  assert.deepEqual(records.filter((record) => record.request).map((record) => record.request), [{
    jsonrpc: '2.0', id: 1, method: 'initialize',
    params: {
      protocolVersion: 1, clientCapabilities: { fs: {}, terminal: false },
      clientInfo: { name: 'pairroom-no-auth-probe', version: '1' },
    },
  }]);
  // This observes the actual stdin frames, rather than an implementation constant.
  for (const forbidden of ['authenticate', 'session/new', 'session/load', 'session/prompt']) {
    assert.ok(!records.some((record) => record.request?.method === forbidden));
  }
}

test('isolated subprocesses send initialize only and retain redacted evidence', async (t) => {
  const f = fixture(t);
  const keys = ['GEMINI_API_KEY', 'GOOGLE_API_KEY', 'GOOGLE_APPLICATION_CREDENTIALS', 'GOOGLE_CLOUD_PROJECT',
    'AWS_SECRET_ACCESS_KEY', 'GITHUB_TOKEN', 'HTTP_PROXY', 'GEMINI_CLI_IDE_SERVER_STDIO_COMMAND',
    'GEMINI_CLI_SYSTEM_SETTINGS_PATH', 'GEMINI_CLI_SYSTEM_DEFAULTS_PATH', 'GEMINI_CLI_HOME',
    'GEMINI_CLI_TRUST_WORKSPACE', 'GEMINI_CLI_TRUSTED_FOLDERS_PATH', 'GEMINI_CLI_FUTURE_CREDENTIAL',
    'GEMINI_SANDBOX', 'DOTENV_CONFIG_PATH', 'DOTENV_CONFIG_OVERRIDE', 'NODE_OPTIONS', 'NODE_PATH'];
  for (const key of keys) {
    const previous = process.env[key];
    process.env[key] = 'SECRET_FIXTURE';
    t.after(() => { if (previous === undefined) delete process.env[key]; else process.env[key] = previous; });
  }
  fs.writeFileSync(path.join(f.root, '.env'), 'GEMINI_API_KEY=SECRET_FIXTURE\n');
  const report = await probe(f.packageDir, { temporaryParent: f.root });
  assert.equal(report.status, 'passed');
  assert.equal(report.authenticatedE2E, false);
  assert.equal(report.observedVersion, '0.62.0');
  assert.match(report.entrySHA256, /^[0-9a-f]{64}$/);
  assert.deepEqual(report.capabilities, {
    loadSessionAdvertised: true, image: true, audio: false, embeddedContext: true, mcpHTTP: true, mcpSSE: false,
  });
  assert.ok(!JSON.stringify(report).includes('SECRET_FIXTURE'));
  const records = f.records();
  assertInitializeOnly(records);
  const invocations = records.filter((record) => record.args);
  assert.deepEqual(invocations.map((record) => record.args), [['--version'], ['--help'], ['--acp']]);
  for (const invocation of invocations) {
    assert.ok(!JSON.stringify(invocation).includes('SECRET_FIXTURE'));
    assert.equal(invocation.env.HOME, invocation.env.USERPROFILE);
    assert.equal(invocation.env.HOME, invocation.env.GEMINI_CLI_HOME);
    assert.deepEqual(Object.keys(invocation.env).filter((key) => key.startsWith('GEMINI_')).sort(), [
      'GEMINI_CLI_HOME', 'GEMINI_CLI_NO_RELAUNCH', 'GEMINI_CLI_SYSTEM_DEFAULTS_PATH', 'GEMINI_CLI_SYSTEM_SETTINGS_PATH',
    ]);
    assert.ok(invocation.cwd.startsWith(f.root + path.sep));
    assert.notEqual(invocation.cwd, process.cwd());
    assert.equal(invocation.envFile, '');
    assert.equal(invocation.settings.privacy.usageStatisticsEnabled, false);
    assert.equal(invocation.settings.telemetry.enabled, false);
    assert.equal(invocation.settings.general.enableAutoUpdate, false);
    assert.ok(!fs.existsSync(invocation.cwd), 'temporary project must be removed after reaping child');
    assert.ok(!fs.existsSync(invocation.env.HOME), 'temporary home must be removed');
  }
});

test('legacy ACP switch is selected only when advertised', async (t) => {
  const f = fixture(t, 'legacy');
  const report = await probe(f.packageDir);
  assert.equal(report.status, 'passed');
  assert.equal(report.acpFlag, '--experimental-acp');
  assertInitializeOnly(f.records());
});

for (const [mode, failure, stage] of [
  ['bad-version', 'version_mismatch', 'version'], ['version-exit', 'command_failed', 'version'],
  ['no-acp', 'acp_not_advertised', 'help'], ['wrong-id', 'unexpected_initialize_response', 'initialize'],
  ['server-request', 'unexpected_initialize_response', 'initialize'], ['rpc-error', 'unexpected_initialize_response', 'initialize'],
  ['wrong-protocol', 'incompatible_initialize_response', 'initialize'], ['bad-capability', 'invalid_capabilities', 'initialize'],
  ['malformed', 'invalid_json', 'initialize'], ['overflow', 'output_limit', 'initialize'],
  ['exit', 'missing_initialize_response', 'initialize'], ['timeout', 'timeout', 'initialize'],
]) {
  test(`fails closed on ${mode} without another request or raw output`, async (t) => {
    const f = fixture(t, mode);
    const report = await probe(f.packageDir, { timeoutMs: 1500, temporaryParent: f.root });
    assert.equal(report.status, 'failed');
    assert.equal(report.failure, failure);
    assert.equal(report.stage, stage);
    assert.ok(!JSON.stringify(report).includes('SECRET_FIXTURE'));
    if (stage === 'initialize') assertInitializeOnly(f.records());
    else assert.ok(!f.records().some((record) => record.request));
    assert.deepEqual(fs.readdirSync(f.root).sort(), ['package', 'wire.jsonl']);
  });
}

test('package mismatch never launches a process', async (t) => {
  const f = fixture(t);
  fs.writeFileSync(path.join(f.packageDir, 'package.json'), '{"name":"untrusted","version":"0.62.0"}');
  const report = await probe(f.packageDir);
  assert.equal(report.failure, 'package_mismatch');
  assert.deepEqual(f.records(), []);
});

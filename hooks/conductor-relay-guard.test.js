'use strict';

const assert = require('assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const test = require('node:test');

const { relayCommand, runHook } = require('./conductor-relay-guard.js');

const WORKER_HEADER = '[attestation] role=worker provider=codex model=gpt-5(parsed) effort=high perm=write action="implement issue #101" exit=0 dur=42s tokens=120/34|154 verified=true';
const WORKER_PRETTY = [
  'attestation: verified',
  '  role        worker',
  '  provider    OpenAI',
  '  tool        codex',
  '  model       gpt-5 (detected)',
  '  effort      high',
  '  permission  write',
  '  action      "implement issue #101"',
  '  exit        0',
  '  started     2026-06-28 00:00:00 UTC',
  '  ended       2026-06-28 00:00:42 UTC',
  '  duration    42s (42.0 s)',
  '  tokens      input=120  output=34  total=154',
  '  verified    true',
].join('\n');

const VERIFIER_HEADER = '[attestation] role=verifier provider=claude model=claude-opus-4 effort=high perm=read action="review PR #202" exit=0 dur=17s tokens=80/21|101 verified=true';
const VERIFIER_PRETTY = [
  'attestation: verified',
  '  role        verifier',
  '  provider    Anthropic',
  '  tool        claude',
  '  model       claude-opus-4',
  '  effort      high',
  '  permission  read',
  '  action      "review PR #202"',
  '  exit        0',
  '  started     2026-06-28 00:01:00 UTC',
  '  ended       2026-06-28 00:01:17 UTC',
  '  duration    17s (17.0 s)',
  '  tokens      input=80  output=21  total=101',
  '  verified    true',
].join('\n');

function hookEnv(stateDir) {
  return {
    LOOPCODER_RELAY_GUARD_SCOPE: 'always',
    LOOPCODER_RELAY_GUARD_STATE_DIR: stateDir,
  };
}

function writeWorkerLedger(root) {
  const dir = path.join(root, '.loopcoder', 'relay', 'run-test');
  fs.mkdirSync(dir, { recursive: true });
  const ledgerPath = path.join(dir, 'job-101-1.attest');
  const block = `${WORKER_HEADER}\n${WORKER_PRETTY}\n`;
  fs.writeFileSync(ledgerPath, [
    '# loopcoder relay attestation',
    '# command=dispatch',
    '# role=worker',
    '# run_id=run-test',
    '# issue=101',
    block,
  ].join('\n'));
  return { ledgerPath, block };
}

function writeVerifierLedger(root) {
  const dir = path.join(root, '.loopcoder', 'relay', 'run-test');
  fs.mkdirSync(dir, { recursive: true });
  const ledgerPath = path.join(dir, 'pr-202-1.attest');
  const block = `${VERIFIER_HEADER}\n${VERIFIER_PRETTY}\n`;
  fs.writeFileSync(ledgerPath, [
    '# loopcoder relay attestation',
    '# command=loopreview',
    '# role=verifier',
    '# run_id=run-test',
    '# pr=202',
    block,
  ].join('\n'));
  return { ledgerPath, block };
}

function dispatchPostTool(root, response) {
  return {
    session_id: 'session-1',
    cwd: root,
    hook_event_name: 'PostToolUse',
    tool_name: 'Bash',
    tool_input: {
      command: 'loopcoder dispatch --repo . --issue-number 101 --issue-title "Implement"',
    },
    tool_response: response,
  };
}

function loopreviewPostTool(root, response) {
  return {
    session_id: 'session-1',
    cwd: root,
    hook_event_name: 'PostToolUse',
    tool_name: 'Bash',
    tool_input: {
      command: 'loopcoder loopreview --repo . --pr-number 202',
    },
    tool_response: response,
  };
}

function stopInput(root) {
  return {
    session_id: 'session-1',
    cwd: root,
    hook_event_name: 'Stop',
    stop_hook_active: false,
  };
}

test('surfaced relay block on PostToolUse allows Stop', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-relay-hook-'));
  const stateDir = path.join(root, 'state');
  const env = hookEnv(stateDir);
  writeWorkerLedger(root);

  const postTool = runHook(JSON.stringify(dispatchPostTool(root, {
    stdout: `${WORKER_HEADER}\n{"role":"worker","verified":true}\n`,
    stderr: `${WORKER_PRETTY}\n`,
    exit_code: 0,
  })), { env });
  assert.strictEqual(postTool.exitCode, 0);

  const stop = runHook(JSON.stringify(stopInput(root)), { env });
  assert.strictEqual(stop.exitCode, 0);
  assert.strictEqual(stop.stderr, '');
});

test('swallowed relay block blocks Stop and prints ledger block', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-relay-hook-'));
  const stateDir = path.join(root, 'state');
  const env = hookEnv(stateDir);
  const { block } = writeWorkerLedger(root);

  const postTool = runHook(JSON.stringify(dispatchPostTool(root, {
    stdout: 'dispatch completed without visible attestation\n',
    stderr: '',
    exit_code: 0,
  })), { env });
  assert.strictEqual(postTool.exitCode, 0);

  const firstStop = runHook(JSON.stringify(stopInput(root)), { env });
  assert.strictEqual(firstStop.exitCode, 2);
  assert.match(firstStop.stderr, /local verbatim attestation relay was missing/);
  assert.ok(firstStop.stderr.includes(block.trimEnd()), firstStop.stderr);

  const secondStop = runHook(JSON.stringify(stopInput(root)), { env });
  assert.strictEqual(secondStop.exitCode, 0);
});

test('surfaced verifier relay block on loopreview PostToolUse allows Stop', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-relay-hook-'));
  const stateDir = path.join(root, 'state');
  const env = hookEnv(stateDir);
  writeVerifierLedger(root);

  const postTool = runHook(JSON.stringify(loopreviewPostTool(root, {
    stdout: `${VERIFIER_HEADER}\n{"role":"verifier","verified":true}\n`,
    stderr: `${VERIFIER_PRETTY}\n`,
    exit_code: 0,
  })), { env });
  assert.strictEqual(postTool.exitCode, 0);

  const stop = runHook(JSON.stringify(stopInput(root)), { env });
  assert.strictEqual(stop.exitCode, 0);
  assert.strictEqual(stop.stderr, '');
});

test('swallowed verifier relay block blocks Stop and prints ledger block', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-relay-hook-'));
  const stateDir = path.join(root, 'state');
  const env = hookEnv(stateDir);
  const { block } = writeVerifierLedger(root);

  const postTool = runHook(JSON.stringify(loopreviewPostTool(root, {
    stdout: 'loopreview completed without visible attestation\n',
    stderr: '',
    exit_code: 0,
  })), { env });
  assert.strictEqual(postTool.exitCode, 0);

  const firstStop = runHook(JSON.stringify(stopInput(root)), { env });
  assert.strictEqual(firstStop.exitCode, 2);
  assert.match(firstStop.stderr, /local verbatim attestation relay was missing/);
  assert.ok(firstStop.stderr.includes(block.trimEnd()), firstStop.stderr);

  const secondStop = runHook(JSON.stringify(stopInput(root)), { env });
  assert.strictEqual(secondStop.exitCode, 0);
});

test('malformed hook input allows session to continue', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-relay-hook-'));
  const result = runHook('{not-json', { env: hookEnv(path.join(root, 'state')) });
  assert.strictEqual(result.exitCode, 0);
});

test('auto scope ignores non-conductor workspace', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-relay-hook-'));
  const result = runHook(JSON.stringify({
    session_id: 'session-2',
    cwd: root,
    hook_event_name: 'Stop',
  }), { env: {} });

  assert.strictEqual(result.exitCode, 0);
});

test('scope off disables guard', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-relay-hook-'));
  writeWorkerLedger(root);
  const env = {
    LOOPCODER_RELAY_GUARD_SCOPE: 'off',
    LOOPCODER_RELAY_GUARD_STATE_DIR: path.join(root, 'state'),
  };

  const postTool = runHook(JSON.stringify(dispatchPostTool(root, {
    stdout: 'dispatch completed without visible attestation\n',
    stderr: '',
    exit_code: 0,
  })), { env });
  assert.strictEqual(postTool.exitCode, 0);

  const stop = runHook(JSON.stringify(stopInput(root)), { env });
  assert.strictEqual(stop.exitCode, 0);
});

test('recognizes loopcoder dispatch and loopreview commands', () => {
  assert.deepStrictEqual(relayCommand('loopcoder dispatch --repo .'), { kind: 'dispatch', role: 'worker' });
  assert.deepStrictEqual(relayCommand('"C:\\Tools\\loopcoder.exe" loopreview --repo .'), { kind: 'loopreview', role: 'verifier' });
  assert.deepStrictEqual(relayCommand('$env:LOOPCODER_BIN dispatch --repo .'), { kind: 'dispatch', role: 'worker' });
  assert.strictEqual(relayCommand('loopcoder attest --role conductor'), null);
});

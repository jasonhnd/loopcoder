'use strict';

const assert = require('assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const test = require('node:test');

const { isConductorAttestCommand, runHook } = require('./conductor-attest.js');

function hookEnv(stateDir) {
  return {
    LOOPCODER_CONDUCTOR_ATTEST_SCOPE: 'always',
    LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR: stateDir,
  };
}

test('denies Stop until conductor attest is recorded, then allows', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-attest-hook-'));
  const stateDir = path.join(root, 'state');
  const env = hookEnv(stateDir);
  const stopInput = {
    session_id: 'session-1',
    cwd: root,
    hook_event_name: 'Stop',
    stop_hook_active: false,
  };

  const firstStop = runHook(JSON.stringify(stopInput), { env });
  assert.strictEqual(firstStop.exitCode, 2);
  assert.match(firstStop.stderr, /loopcoder conductor attestation is required/);

  const postTool = runHook(JSON.stringify({
    session_id: 'session-1',
    cwd: root,
    hook_event_name: 'PostToolUse',
    tool_name: 'Bash',
    tool_input: {
      command: 'loopcoder attest --role conductor --provider claude-code --model opus --permission orchestrate --action "merge PR #10" --duration-ms 1 --total-tokens 2',
    },
    tool_response: {
      stdout: '{"role":"conductor","model_source":"self-reported","verified":false}\n[attestation] role=conductor provider=claude-code model=opus(self-reported) effort=default perm=orchestrate action="merge PR #10" exit=0 dur=1ms tokens=2 verified=false\n',
      stderr: '',
      interrupted: false,
      isImage: false,
    },
  }), { env });
  assert.strictEqual(postTool.exitCode, 0);

  const secondStop = runHook(JSON.stringify(stopInput), { env });
  assert.strictEqual(secondStop.exitCode, 0);
  assert.strictEqual(secondStop.stderr, '');
});

test('allows malformed hook input instead of breaking the session', () => {
  const result = runHook('{not-json', { env: hookEnv(fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-attest-hook-'))) });
  assert.strictEqual(result.exitCode, 0);
});

test('auto scope ignores unrelated workspaces', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'loopcoder-attest-hook-'));
  const result = runHook(JSON.stringify({
    session_id: 'session-2',
    cwd: root,
    hook_event_name: 'Stop',
  }), { env: {} });

  assert.strictEqual(result.exitCode, 0);
});

test('recognizes loopcoder attest role syntax', () => {
  assert.strictEqual(isConductorAttestCommand('loopcoder attest --role conductor --provider codex'), true);
  assert.strictEqual(isConductorAttestCommand('"./loopcoder.exe" attest -Role conductor'), true);
  assert.strictEqual(isConductorAttestCommand('"C:\\Tools\\loopcoder.exe" attest --role conductor'), true);
  assert.strictEqual(isConductorAttestCommand('$env:LOOPCODER_BIN attest --role=conductor'), true);
  assert.strictEqual(isConductorAttestCommand('loopcoder attest --role worker'), false);
});

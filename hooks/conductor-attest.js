#!/usr/bin/env node
'use strict';

const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');

const MAX_STATE_FILES = 128;
const MAX_STATE_AGE_MS = 7 * 24 * 60 * 60 * 1000;
const MAX_TEXT_FIELD = 4096;

function runHook(inputText, deps = {}) {
  const env = deps.env || process.env;
  const now = deps.now || (() => new Date());
  const fsMod = deps.fs || fs;
  const pathMod = deps.path || path;
  const osMod = deps.os || os;

  let input;
  try {
    input = JSON.parse(inputText || '{}');
  } catch {
    return allow();
  }

  const eventName = String(input.hook_event_name || '');
  const sessionID = String(input.session_id || '');
  const cwd = String(input.cwd || env.PWD || process.cwd());

  if (!sessionID || !shouldEnforce(input, cwd, env, fsMod, pathMod)) {
    return allow();
  }

  let statePath;
  try {
    statePath = stateFilePath(sessionID, cwd, env, fsMod, pathMod, osMod);
    pruneStateDir(pathMod.dirname(statePath), now(), fsMod, pathMod);
  } catch {
    return allow();
  }

  if (isToolCompleteEvent(eventName)) {
    if (isSuccessfulConductorAttest(input)) {
      try {
        writeState(statePath, input, now(), fsMod);
      } catch {
        return allow();
      }
    }
    return allow();
  }

  if (!isStopEvent(eventName)) {
    return allow();
  }

  let state;
  try {
    state = readState(statePath, fsMod);
  } catch {
    return allow();
  }

  if (state && state.attested === true) {
    return allow();
  }

  return block([
    'loopcoder conductor attestation is required before completing this delivery turn.',
    'Run `loopcoder attest --role conductor --provider <provider> --model <model> --permission orchestrate --action "<delivery action>" --duration-ms <ms> --total-tokens <tokens>` with the actual host model and usage, then finish the turn.',
    'Keep the emitted attestation local: use command output and gitignored .loopcoder/ run records for recovery; do not copy it into PR bodies, comments, merge commits, or merge comments.',
  ].join('\n'));
}

function allow() {
  return { exitCode: 0, stdout: '', stderr: '' };
}

function block(stderr) {
  return { exitCode: 2, stdout: '', stderr };
}

function shouldEnforce(input, cwd, env, fsMod, pathMod) {
  const scope = String(env.LOOPCODER_CONDUCTOR_ATTEST_SCOPE || 'auto').toLowerCase();
  if (['0', 'false', 'off', 'never'].includes(scope)) {
    return false;
  }
  if (['1', 'true', 'on', 'always'].includes(scope)) {
    return true;
  }

  const eventName = String(input.hook_event_name || '');
  if (!isToolCompleteEvent(eventName) && !isStopEvent(eventName)) {
    return false;
  }

  return looksLikeLoopcoderConductorWorkspace(cwd, fsMod, pathMod);
}

function looksLikeLoopcoderConductorWorkspace(cwd, fsMod, pathMod) {
  const skillPath = pathMod.join(cwd, 'SKILL.md');
  const agentsPath = pathMod.join(cwd, 'AGENTS.md');
  const deliveryPath = pathMod.join(cwd, '.delivery.yml');

  if (fileContains(skillPath, 'loopcoder Conductor Playbook', fsMod)) {
    return true;
  }
  if (fileContains(agentsPath, 'loopcoder Codex CLI Entrypoint', fsMod)) {
    return true;
  }
  return fileContains(deliveryPath, 'conductor:', fsMod) && fileContains(deliveryPath, 'worker:', fsMod);
}

function fileContains(filePath, needle, fsMod) {
  try {
    return fsMod.readFileSync(filePath, 'utf8').includes(needle);
  } catch {
    return false;
  }
}

function isToolCompleteEvent(eventName) {
  return eventName === 'PostToolUse' || eventName === 'AfterTool';
}

function isStopEvent(eventName) {
  return eventName === 'Stop' || eventName === 'AfterAgent';
}

function stateFilePath(sessionID, cwd, env, fsMod, pathMod, osMod) {
  const baseDir = env.LOOPCODER_CONDUCTOR_ATTEST_STATE_DIR ||
    pathMod.join(cwd || osMod.tmpdir(), '.loopcoder', 'hooks', 'conductor-attest');
  fsMod.mkdirSync(baseDir, { recursive: true });
  const hash = crypto.createHash('sha256').update(sessionID).digest('hex').slice(0, 32);
  return pathMod.join(baseDir, `session-${hash}.json`);
}

function pruneStateDir(dirPath, now, fsMod, pathMod) {
  let entries;
  try {
    entries = fsMod.readdirSync(dirPath, { withFileTypes: true })
      .filter((entry) => entry.isFile() && entry.name.endsWith('.json'))
      .map((entry) => {
        const fullPath = pathMod.join(dirPath, entry.name);
        let mtimeMs = 0;
        try {
          mtimeMs = fsMod.statSync(fullPath).mtimeMs;
        } catch {
          mtimeMs = 0;
        }
        return { fullPath, mtimeMs };
      });
  } catch {
    return;
  }

  const cutoff = now.getTime() - MAX_STATE_AGE_MS;
  for (const entry of entries) {
    if (entry.mtimeMs < cutoff) {
      safeUnlink(entry.fullPath, fsMod);
    }
  }

  const remaining = entries
    .filter((entry) => entry.mtimeMs >= cutoff)
    .sort((a, b) => a.mtimeMs - b.mtimeMs);
  const deleteCount = Math.max(0, remaining.length - MAX_STATE_FILES);
  for (const entry of remaining.slice(0, deleteCount)) {
    safeUnlink(entry.fullPath, fsMod);
  }
}

function safeUnlink(filePath, fsMod) {
  try {
    fsMod.unlinkSync(filePath);
  } catch {
    // Best-effort cleanup only.
  }
}

function readState(statePath, fsMod) {
  try {
    return JSON.parse(fsMod.readFileSync(statePath, 'utf8'));
  } catch (err) {
    if (err && err.code === 'ENOENT') {
      return null;
    }
    throw err;
  }
}

function writeState(statePath, input, now, fsMod) {
  const command = String((((input.tool_input || {}).command) || ''));
  const responseText = collectStrings(input.tool_response).join('\n');
  const header = firstMatchingLine(responseText, /\[attestation\]\s+role=conductor\b/);

  const state = {
    attested: true,
    attested_at: now.toISOString(),
    session_id_hash: crypto.createHash('sha256').update(String(input.session_id || '')).digest('hex').slice(0, 32),
    command: truncate(command, MAX_TEXT_FIELD),
    header: truncate(header, MAX_TEXT_FIELD),
  };

  fsMod.writeFileSync(statePath, `${JSON.stringify(state, null, 2)}\n`, { mode: 0o600 });
}

function isSuccessfulConductorAttest(input) {
  if (!isShellTool(input.tool_name)) {
    return false;
  }

  const command = String((((input.tool_input || {}).command) || ''));
  if (!isConductorAttestCommand(command)) {
    return false;
  }

  const response = input.tool_response || {};
  if (response && typeof response === 'object') {
    if (response.interrupted === true || response.error) {
      return false;
    }
    const exitCode = response.exit_code ?? response.exitCode ?? response.status;
    if (exitCode !== undefined && Number(exitCode) !== 0) {
      return false;
    }
  }

  return containsConductorAttestation(response);
}

function isShellTool(toolName) {
  return ['Bash', 'run_shell_command', 'shell_command'].includes(String(toolName || ''));
}

function containsConductorAttestation(value) {
  const text = collectStrings(value).join('\n');
  if (/\[attestation\]\s+role=conductor\b/.test(text)) {
    return true;
  }
  return /"role"\s*:\s*"conductor"/.test(text) &&
    /"model_source"\s*:\s*"self-reported"/.test(text) &&
    /"verified"\s*:\s*false/.test(text);
}

function isConductorAttestCommand(command) {
  const words = shellWords(command);
  for (let i = 0; i < words.length - 1; i += 1) {
    if (!isLoopcoderToken(words[i]) || words[i + 1] !== 'attest') {
      continue;
    }
    const args = words.slice(i + 2, nextSeparatorIndex(words, i + 2));
    if (hasRoleConductor(args)) {
      return true;
    }
  }
  return false;
}

function shellWords(command) {
  const words = [];
  let current = '';
  let quote = '';

  const pushCurrent = () => {
    if (current.length > 0) {
      words.push(current);
      current = '';
    }
  };

  for (let i = 0; i < command.length; i += 1) {
    const char = command[i];
    if (quote) {
      if (char === quote) {
        quote = '';
      } else {
        current += char;
      }
      continue;
    }
    if (char === '"' || char === "'") {
      quote = char;
      continue;
    }
    if (/\s/.test(char)) {
      pushCurrent();
      continue;
    }
    if (char === ';' || char === '|') {
      pushCurrent();
      words.push(char);
      continue;
    }
    if (char === '&') {
      pushCurrent();
      if (command[i + 1] === '&') {
        words.push('&&');
        i += 1;
      } else {
        words.push('&');
      }
      continue;
    }
    current += char;
  }
  pushCurrent();
  return words;
}

function isLoopcoderToken(token) {
  if (/\bLOOPCODER_BIN\b/i.test(token)) {
    return true;
  }
  const normalized = token.replace(/\\/g, '/');
  const base = normalized.slice(normalized.lastIndexOf('/') + 1).toLowerCase();
  return base === 'loopcoder' || base === 'loopcoder.exe';
}

function nextSeparatorIndex(words, start) {
  for (let i = start; i < words.length; i += 1) {
    if ([';', '|', '&&', '||', '&'].includes(words[i])) {
      return i;
    }
  }
  return words.length;
}

function hasRoleConductor(args) {
  for (let i = 0; i < args.length; i += 1) {
    const arg = args[i].toLowerCase();
    if ((arg === '--role' || arg === '-role') && String(args[i + 1] || '').toLowerCase() === 'conductor') {
      return true;
    }
    if (arg === '--role=conductor' || arg === '-role=conductor') {
      return true;
    }
  }
  return false;
}

function collectStrings(value, out = [], depth = 0) {
  if (depth > 8 || value === null || value === undefined) {
    return out;
  }
  if (typeof value === 'string') {
    out.push(value);
    return out;
  }
  if (typeof value !== 'object') {
    return out;
  }
  if (Array.isArray(value)) {
    for (const item of value) {
      collectStrings(item, out, depth + 1);
    }
    return out;
  }
  for (const item of Object.values(value)) {
    collectStrings(item, out, depth + 1);
  }
  return out;
}

function firstMatchingLine(text, pattern) {
  return String(text || '').split(/\r?\n/).find((line) => pattern.test(line)) || '';
}

function truncate(value, maxLength) {
  const text = String(value || '');
  return text.length > maxLength ? text.slice(0, maxLength) : text;
}

if (require.main === module) {
  const chunks = [];
  process.stdin.setEncoding('utf8');
  process.stdin.on('data', (chunk) => chunks.push(chunk));
  process.stdin.on('end', () => {
    const result = runHook(chunks.join(''));
    if (result.stdout) {
      process.stdout.write(result.stdout);
    }
    if (result.stderr) {
      process.stderr.write(result.stderr);
      if (!result.stderr.endsWith('\n')) {
        process.stderr.write('\n');
      }
    }
    process.exitCode = result.exitCode;
  });
}

module.exports = {
  containsConductorAttestation,
  isConductorAttestCommand,
  runHook,
  shellWords,
};

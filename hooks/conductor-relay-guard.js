#!/usr/bin/env node
'use strict';

const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');

const MAX_STATE_FILES = 128;
const MAX_STATE_AGE_MS = 7 * 24 * 60 * 60 * 1000;
const MAX_STATE_RECORDS = 256;
const MAX_LEDGER_FILES = 256;
const MAX_LEDGER_BYTES = 256 * 1024;
const RECENT_LEDGER_GRACE_MS = 10 * 60 * 1000;

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
    return handleToolComplete(input, statePath, cwd, now, fsMod, pathMod);
  }

  if (isStopEvent(eventName)) {
    return handleStop(statePath, now, fsMod);
  }

  return allow();
}

function handleToolComplete(input, statePath, cwd, now, fsMod, pathMod) {
  if (!isShellTool(input.tool_name)) {
    return allow();
  }

  const command = String((((input.tool_input || {}).command) || ''));
  const enforced = relayCommand(command);
  if (!enforced) {
    return allow();
  }

  try {
    const state = readState(statePath, fsMod) || freshState(input);
    const cutoffMs = now().getTime() - RECENT_LEDGER_GRACE_MS;
    const records = discoverLedgerRecords(cwd, fsMod, pathMod)
      .filter((record) => record.mtimeMs >= cutoffMs)
      .filter((record) => record.role === enforced.role && recordCommandMatches(enforced.kind, record))
      .filter((record) => !state.records[record.id]);

    if (records.length === 0) {
      return allow();
    }

    const responseText = collectStrings(input.tool_response).join('\n');
    const seenAt = now().toISOString();
    for (const record of records) {
      state.records[record.id] = {
        id: record.id,
        role: record.role,
        command: record.command || enforced.kind,
        status: containsSurfacedAttestation(responseText, record) ? 'surfaced' : 'pending',
        ledger_path: record.path,
        header: record.header,
        recorded_at: seenAt,
        updated_at: seenAt,
      };
    }
    state.updated_at = seenAt;
    writeState(statePath, trimStateRecords(state), fsMod);
  } catch {
    return allow();
  }

  return allow();
}

function handleStop(statePath, now, fsMod) {
  let state;
  try {
    state = readState(statePath, fsMod);
  } catch {
    return allow();
  }
  if (!state || !state.records || typeof state.records !== 'object') {
    return allow();
  }

  const pending = Object.values(state.records).filter((record) => record && record.status === 'pending');
  if (pending.length === 0) {
    return allow();
  }

  let blocks;
  try {
    blocks = pending.map((record) => readLedgerRecord(record.ledger_path, fsMod, path).block);
    const surfacedAt = now().toISOString();
    for (const record of pending) {
      state.records[record.id].status = 'surfaced_by_hook';
      state.records[record.id].updated_at = surfacedAt;
    }
    state.updated_at = surfacedAt;
    writeState(statePath, trimStateRecords(state), fsMod);
  } catch {
    return allow();
  }

  return block([
    'loopcoder local verbatim attestation relay was missing from the completed command output.',
    'The missing local-only attestation block(s) are printed below:',
    '',
    blocks.join('\n'),
    'Keep these attestations local-only: do not copy them into PRs, issues, comments, commits, merge artifacts, docs, or tracked files.',
  ].join('\n'));
}

function allow() {
  return { exitCode: 0, stdout: '', stderr: '' };
}

function block(stderr) {
  return { exitCode: 2, stdout: '', stderr };
}

function shouldEnforce(input, cwd, env, fsMod, pathMod) {
  const scope = String(env.LOOPCODER_RELAY_GUARD_SCOPE || 'auto').toLowerCase();
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
  const baseDir = env.LOOPCODER_RELAY_GUARD_STATE_DIR ||
    pathMod.join(cwd || osMod.tmpdir(), '.loopcoder', 'hooks', 'conductor-relay-guard');
  fsMod.mkdirSync(baseDir, { recursive: true });
  const hash = hashText(sessionID).slice(0, 32);
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

function writeState(statePath, state, fsMod) {
  fsMod.writeFileSync(statePath, `${JSON.stringify(state, null, 2)}\n`, { mode: 0o600 });
}

function freshState(input) {
  return {
    version: 1,
    session_id_hash: hashText(String(input.session_id || '')).slice(0, 32),
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    records: {},
  };
}

function trimStateRecords(state) {
  const entries = Object.entries(state.records || {})
    .sort((a, b) => String(a[1].updated_at || '').localeCompare(String(b[1].updated_at || '')));
  const keep = entries.slice(Math.max(0, entries.length - MAX_STATE_RECORDS));
  state.records = Object.fromEntries(keep);
  return state;
}

function discoverLedgerRecords(cwd, fsMod, pathMod) {
  const root = pathMod.join(cwd, '.loopcoder', 'relay');
  const files = [];
  walkLedger(root, files, fsMod, pathMod);
  files.sort((a, b) => b.mtimeMs - a.mtimeMs);
  return files.slice(0, MAX_LEDGER_FILES).map((entry) => readLedgerRecord(entry.path, fsMod, pathMod)).filter(Boolean);
}

function walkLedger(dirPath, files, fsMod, pathMod) {
  let entries;
  try {
    entries = fsMod.readdirSync(dirPath, { withFileTypes: true });
  } catch (err) {
    if (err && err.code === 'ENOENT') {
      return;
    }
    throw err;
  }

  for (const entry of entries) {
    const fullPath = pathMod.join(dirPath, entry.name);
    if (entry.isDirectory()) {
      walkLedger(fullPath, files, fsMod, pathMod);
      continue;
    }
    if (!entry.isFile() || !entry.name.endsWith('.attest')) {
      continue;
    }
    const stat = fsMod.statSync(fullPath);
    if (stat.size > MAX_LEDGER_BYTES) {
      throw new Error(`relay ledger too large: ${fullPath}`);
    }
    files.push({ path: fullPath, mtimeMs: stat.mtimeMs });
  }
}

function readLedgerRecord(filePath, fsMod, pathMod) {
  const stat = fsMod.statSync(filePath);
  if (stat.size > MAX_LEDGER_BYTES) {
    throw new Error(`relay ledger too large: ${filePath}`);
  }
  const text = fsMod.readFileSync(filePath, 'utf8');
  const block = ledgerAttestationBlock(text);
  if (!block) {
    return null;
  }
  const header = block.split(/\r?\n/, 1)[0];
  const roleMatch = header.match(/\[attestation\]\s+role=(worker|verifier)\b/);
  if (!roleMatch) {
    return null;
  }
  const command = firstMetaValue(text, 'command');
  const id = hashText(`${pathMod.resolve(filePath)}\0${header}\0${block}`);
  return {
    id,
    path: filePath,
    mtimeMs: stat.mtimeMs,
    command,
    role: roleMatch[1],
    header,
    block,
  };
}

function ledgerAttestationBlock(text) {
  const match = String(text || '').match(/^\[attestation\]\s+role=(?:worker|verifier)\b/m);
  if (!match) {
    return '';
  }
  return `${String(text).slice(match.index).replace(/(?:\r?\n)*$/g, '')}\n`;
}

function firstMetaValue(text, key) {
  const pattern = new RegExp(`^#\\s+${escapeRegExp(key)}=([^\\r\\n]*)`, 'm');
  const match = String(text || '').match(pattern);
  return match ? match[1].trim() : '';
}

function recordCommandMatches(kind, record) {
  if (record.command) {
    return record.command === kind;
  }
  return (kind === 'dispatch' && record.role === 'worker') ||
    (kind === 'loopreview' && record.role === 'verifier');
}

function containsSurfacedAttestation(text, record) {
  const output = String(text || '');
  if (record.header && output.includes(record.header)) {
    return true;
  }
  const rolePattern = new RegExp(`\\[attestation\\]\\s+role=${escapeRegExp(record.role)}\\b`);
  if (rolePattern.test(output)) {
    return true;
  }
  const roleJSON = new RegExp(`"role"\\s*:\\s*"${escapeRegExp(record.role)}"`);
  return roleJSON.test(output) && /"verified"\s*:\s*true/.test(output);
}

function relayCommand(command) {
  const words = shellWords(command);
  for (let i = 0; i < words.length - 1; i += 1) {
    if (!isLoopcoderToken(words[i])) {
      continue;
    }
    const subcommand = words[i + 1];
    if (subcommand === 'dispatch') {
      return { kind: 'dispatch', role: 'worker' };
    }
    if (subcommand === 'loopreview') {
      return { kind: 'loopreview', role: 'verifier' };
    }
    i = nextSeparatorIndex(words, i + 1);
  }
  return null;
}

function isShellTool(toolName) {
  return ['Bash', 'run_shell_command', 'shell_command'].includes(String(toolName || ''));
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

function hashText(value) {
  return crypto.createHash('sha256').update(String(value)).digest('hex');
}

function escapeRegExp(value) {
  return String(value).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
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
  containsSurfacedAttestation,
  ledgerAttestationBlock,
  relayCommand,
  runHook,
  shellWords,
};

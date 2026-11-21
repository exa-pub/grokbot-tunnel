// Runs the desktop client's local-exec daemon without the desktop: it does
// what the Electron main process does for the daemon and nothing else.
//
//   node launcher.mjs <config.json>
//
// The main process writes the daemon's files into the data root (the
// credential it was issued, sealed with the file key, the settings, a
// heartbeat), starts
// local-exec-daemon/main.cjs with an IPC channel and hands it the file key and
// the machine id in one message. The launcher repeats that with the values of
// the config. Its stdout is a protocol for the test harness: one JSON object a
// line. Everything the daemon prints goes to stderr.
//
// Commands on stdin, one JSON object a line:
//   {"op":"credential", ...}  rewrite the credential file (fields of the config)
//   {"op":"settings", ...}    rewrite settings.json (permission, ceiling)
//
// SIGTERM stops the daemon the way the desktop does and exits with its code.
import {spawn} from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import readline from 'node:readline';

// The daemon is someone else's program that works on $HOME: it only runs in
// the image built for it, which has no network when the tests start it.
if (process.env.GROKBOT_COMPARE_IMAGE !== 'desktop') {
  console.error('launcher.mjs runs only inside the grokbot-compare image (make compare)');
  process.exit(2);
}

const config = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const say = message => process.stdout.write(JSON.stringify(message) + '\n');
const file = name => path.join(config.dataRoot, name);
const write = (name, value, text = JSON.stringify(value)) => {
  // Like the desktop's durable files: a temporary file renamed into place, so
  // the daemon never reads half of one.
  const tmp = file(`.${name}.${process.pid}.tmp`);
  fs.writeFileSync(tmp, text, {mode: 0o600});
  fs.renameSync(tmp, file(name));
};

// The key the daemon is handed. A daemon that knows the envelope reads its
// credential sealed with it ({"sandSealedFile": 1, data: base64(iv, tag,
// text)}, AES-256-GCM), as the main process writes it; since 1.2 it also
// ignores a plain one once it has a key. Older releases read it plain.
const entry = path.join(config.dist, 'local-exec-daemon', 'main.cjs');
const fileKey = crypto.randomBytes(32);
const seals = fs.readFileSync(entry, 'utf8').includes('"sandSealedFile"');
const seal = value => {
  const iv = crypto.randomBytes(12);
  const cipher = crypto.createCipheriv('aes-256-gcm', fileKey, iv);
  const body = Buffer.concat([cipher.update(JSON.stringify(value), 'utf8'), cipher.final()]);
  const data = Buffer.concat([iv, cipher.getAuthTag(), body]).toString('base64');
  return JSON.stringify({sandSealedFile: 1, data});
};

function writeCredential(c) {
  const value = {
    backendUrl: c.backendUrl,
    userComputers: [{
      credential: c.credential,
      machineId: c.machineId,
      expiresAtMs: Date.now() + 3600e3,
      accountScope: c.accountScope,
      ...(c.serverAuthoritative ? {serverAuthoritative: true} : {}),
    }],
  };
  write('local-exec-daemon-credential.json', value, seals ? seal(value) : undefined);
}

function writeSettings(s) {
  write('settings.json', {
    version: 1,
    localToolPermission: s.permission,
    ...(s.ceiling === undefined ? {} : {localToolPermissionCeiling: s.ceiling}),
  });
}

fs.mkdirSync(config.dataRoot, {recursive: true, mode: 0o700});
writeCredential(config);
writeSettings(config);
for (const [name, value] of Object.entries(config.files ?? {})) write(name, value);

// The daemon exits when the desktop that owns it stops writing this file.
const beat = () => write('local-exec-supervisor.json', {pid: process.pid, at: Date.now()});
beat();
setInterval(beat, 5000).unref();

const env = {
  ...config.env,
  // What the main process of a released desktop sets for itself and so for
  // the daemon: without it the daemon is a development build with another
  // namespace and version.
  SAND_PACKAGED: '1',
  SAND_LAB: '0',
  SAND_BACKEND_URL: config.backendUrl,
  SAND_DATA_ROOT: config.dataRoot,
  SAND_LOCAL_EXEC_ROOT: config.root,
  SAND_LOCAL_EXEC_GENERATION: '0',
  SAND_DISABLE_TELEMETRY: '1',
  NODE_USE_SYSTEM_CA: '1',
};
// Patches of the bundle a scenario asks for, see preload.cjs.
const patches = config.patches ?? [];
if (patches.length > 0) env.GROKBOT_COMPARE_PATCHES = patches.join(',');
const preload = patches.length > 0 ? ['--require', path.join(import.meta.dirname, 'preload.cjs')] : [];
const daemon = spawn(process.execPath, [...preload, entry], {stdio: ['ignore', 2, 2, 'ipc'], env});
daemon.once('spawn', () => {
  daemon.send({type: 'sand-local-exec-file-key', key: fileKey.toString('base64'), computerId: config.machineId});
  say({event: 'spawned', pid: daemon.pid, node: process.versions.node});
});
daemon.once('error', error => {
  console.error(`launcher: ${error}`);
  process.exit(1);
});
daemon.once('exit', (code, signal) => {
  say({event: 'exit', code, signal});
  process.exit(code ?? 1);
});

process.on('SIGTERM', () => daemon.kill('SIGTERM'));
process.on('SIGINT', () => daemon.kill('SIGINT'));

readline.createInterface({input: process.stdin}).on('line', line => {
  const command = JSON.parse(line);
  switch (command.op) {
    case 'credential':
      writeCredential({...config, ...command});
      break;
    case 'settings':
      writeSettings({...config, ...command});
      break;
    default:
      console.error(`launcher: unknown command ${line}`);
      process.exit(2);
  }
  say({event: 'done', op: command.op});
});

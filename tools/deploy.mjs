#!/usr/bin/env node
import fs from 'node:fs/promises';
import path from 'node:path';
import https from 'node:https';
import http from 'node:http';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const runFile = promisify(execFile);
const SERVER_KEY = '856869e1c2b5a425495ca054823d71c7085f3f2aaddbbf42685bbddd4bc0332d';
const SERVER = '101.37.26.209';
const ROUTER = '192.168.2.1';
const FRPS_CONFIG = '/opt/1panel/apps/frps/frps/data/frps.toml';
const ROC_VHOST = '/opt/1panel/www/conf.d/roc.hubu.wiki.conf';
const ROC_PROXY = '/opt/1panel/www/sites/roc.hubu.wiki/proxy/root.conf';
const OPENRESTY = '1Panel-openresty-wmqb';
const PRIVATE = path.join(ROOT, 'private');
const ARGS = parseArgs(process.argv.slice(2));
const USAGE = `Roc WOL deployment (Node 24+, npm install --prefix tools)
  node tools/deploy.mjs inspect [--router-host-key HEX] [--direct-host wol.hubu.wiki]
  node tools/deploy.mjs stage [--phase private|https|all] [--rotate-mtls]
  node tools/deploy.mjs cutover --device-id PHONE_DEVICE_ID
Options:
  --config private/deployment.json   Unique protected deployment configuration
  --ssh-key private/credentials/SSH.pem  Read only into memory; never uploaded
  --router-password-file FILE        Default private/credentials/router-password.txt; password-only, root/password lines, or JSON
  --router-host-key HEX              Explicit router SSH SHA256 pin (64 lowercase hex)
  --direct-host wol-direct.hubu.wiki  Only when wol's existing explicit record belongs to another service
  --phase private                    Stage binaries/mTLS/STCP, before external DNS prerequisites
  --phase https                      Continue HTTP01/public certificate/direct-origin gateway staging
  --rotate-mtls                      Reissue both private leaf certificates and restart project services
  --help
inspect never changes remote hosts; first router handshake without a pin reports its fingerprint and stops.
Verify that fingerprint locally before passing --router-host-key. The fixed server pin is a first-observed pin,
not an out-of-band identity guarantee. No command reads or prints credentials until a connection is needed.
Prerequisites: official signed build artifacts via tools/build.ps1; server openssl/curl/docker/systemd/tar;
router etherwake/ping/frpc. Existing shared frps is never changed or restarted.
DNS console: selected direct A -> 101.37.26.209, DNS-only, TTL300, no unverified AAAA; preserve other records.
Do not overwrite an unrelated explicit record; use the approved wol-direct alternative and rebuild the APK.
HTTPS phase needs public HTTP01 on BOTH hostnames, including through Cloudflare; edge redirects/challenges
must not block /.well-known/acme-challenge/. Certbot staging runs before one production issuance.
Before cutover: install APK, pair the real phone through direct origin, verify waking and system-authenticated
external-browser LuCI access. --device-id is checked against live authorized non-deployment devices; it is not
proof of true-phone biometric/Clash/power-state acceptance. The operator must finish those checks first.
After cutover: manually read back host-only Cloudflare ssl=strict, no content injection, cache bypass, and any
precise API challenge exception. No whole-zone changes. Public 15201 is closed even if fallback is blocked.
Clash rule-mode suggestion: DOMAIN,<selected-direct-host>,DIRECT (not a VPN bypass; global mode may differ).
Failure after cutover never restores the public LuCI mapping; LAN LuCI remains available.
`;

function parseArgs(values) {
  const out = { command: values[0] && !values[0].startsWith('--') ? values.shift() : undefined };
  const valued = new Set(['config', 'ssh-key', 'router-password-file', 'router-host-key', 'direct-host', 'phase', 'device-id']);
  const flags = new Set(['help', 'rotate-mtls']);
  for (let i = 0; i < values.length; i++) {
    const key = values[i].replace(/^--/, '');
    if (!values[i].startsWith('--') || (!valued.has(key) && !flags.has(key))) throw new Error('Unknown argument; use --help');
    if (flags.has(key)) out[key] = true;
    else if (!values[i + 1] || values[i + 1].startsWith('--')) throw new Error(`Missing --${key} value`);
    else out[key] = values[++i];
  }
  return out;
}
function must(ok, message) { if (!ok) throw new Error(message); }
function quote(s) { return `'${String(s).replaceAll("'", "'\\''")}'`; }
function hash(data) { return crypto.createHash('sha256').update(data).digest('hex'); }
function text(data) { return Buffer.isBuffer(data) ? data.toString('utf8') : String(data); }
function nonce() { return crypto.randomBytes(16).toString('hex'); }
function log(message) { process.stdout.write(`${message}\n`); }
function json(data) { return Buffer.from(`${JSON.stringify(data, null, 2)}\n`); }
function parsePrivateJSON(source) {
  try { return JSON.parse(source); }
  catch { throw new Error('Invalid protected JSON; contents are intentionally not printed'); }
}
async function protect(directory) {
  await fs.mkdir(directory, { recursive: true, mode: 0o700 });
  if (process.platform === 'win32') {
    const aclScript = `$ErrorActionPreference='Stop'; $p='${directory.replaceAll("'", "''")}'; $u=[Security.Principal.WindowsIdentity]::GetCurrent().User; $a=Get-Acl -LiteralPath $p; $a.SetAccessRuleProtection($true,$false); foreach($r in @($a.Access)){[void]$a.RemoveAccessRuleSpecific($r)}; foreach($s in @($u,(New-Object Security.Principal.SecurityIdentifier('S-1-5-18')))) { $a.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($s,'FullControl','ContainerInherit,ObjectInherit','None','Allow'))) }; [IO.Directory]::SetAccessControl($p,$a)`;
    await runFile('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', aclScript]);
  } else await fs.chmod(directory, 0o700);
}
async function privateWrite(filename, data) {
  await protect(path.dirname(filename));
  const tmp = `${filename}.${nonce()}.tmp`;
  await fs.writeFile(tmp, data, { mode: 0o600, flag: 'wx' });
  await fs.rename(tmp, filename);
}
function configPath() {
  const filename = path.resolve(ROOT, ARGS.config || 'private/deployment.json');
  must(filename.startsWith(PRIVATE + path.sep), 'Deployment configuration must remain under private/');
  return filename;
}
async function loadConfig() {
  try {
    const config = parsePrivateJSON(await fs.readFile(configPath(), 'utf8'));
    must(config.version === 1 && config.server === SERVER && config.router === ROUTER && config.serverKey === SERVER_KEY, 'Unexpected deployment identity');
    must(['wol.hubu.wiki', 'wol-direct.hubu.wiki'].includes(config.directHost) && config.fallbackHost === 'roc.hubu.wiki', 'Invalid deployment origins');
    must(/^[a-f0-9]{64}$/.test(config.routerKey), 'Missing pinned router identity');
    must(/^[A-Za-z0-9_-]{43}$/.test(config.stcpKey), 'Invalid deployment STCP key');
    for (const [key, choices] of Object.entries({ gatewayPort: [18081, 28081, 38081], visitorPort: [18082, 28082, 38082], agentPort: [19991, 29991, 39991] })) must(choices.includes(config[key]), `Invalid ${key}`);
    if (ARGS['router-host-key']) must(ARGS['router-host-key'] === config.routerKey, 'Router pin differs from saved deployment');
    if (ARGS['direct-host']) must(ARGS['direct-host'] === config.directHost, 'Origin already fixed in configuration; do not silently migrate deployed identities');
    return config;
  } catch (error) { if (error.code === 'ENOENT') return null; throw error; }
}
async function saveConfig(config) { await privateWrite(configPath(), json(config)); }
function routerPassword(content) {
  const value = text(content).trim();
  if (value.startsWith('{')) {
    const parsed = parsePrivateJSON(value);
    must(parsed.username === 'root' && typeof parsed.password === 'string' && parsed.password, 'Router credential JSON must contain root username and password');
    return parsed.password;
  }
  const lines = value.split(/\r?\n/).filter(Boolean);
  if (lines.length === 1) return lines[0];
  if (lines.length === 2 && /^(?:root|(?:user(?:name)?|用户名|账号)\s*[:：=]\s*root)$/i.test(lines[0])) return lines[1].replace(/^(?:password|密码)\s*[:：=]\s*/i, '');
  throw new Error('Unsupported router credential format; use a protected password-only file or {username,password} JSON');
}
class Remote {
  constructor(client, label) { this.client = client; this.label = label; }
  async exec(script, label = 'remote operation') {
    return new Promise((resolve, reject) => {
      this.client.exec('sh -s', (error, stream) => {
        if (error) return reject(new Error(`${this.label}: unable to start ${label}`));
        let output = ''; let size = 0;
        stream.on('data', data => { size += data.length; if (size > 16 * 1024 * 1024) stream.destroy(); else output += data.toString(); });
        stream.stderr.resume();
        stream.on('error', () => reject(new Error(`${this.label}: transport failed during ${label}`)));
        stream.on('close', code => code === 0 && size <= 16 * 1024 * 1024 ? resolve(output) : reject(new Error(`${this.label}: ${label} failed (${code ?? 'connection lost'}); inspect the protected host logs without publishing secrets`)));
        stream.end(`set -eu\numask 077\n${script}\n`);
      });
    });
  }
  async sftp() {
    if (!this.files) this.files = await new Promise((resolve, reject) => this.client.sftp((error, sftp) => error ? reject(error) : resolve(sftp)));
    return this.files;
  }
  async read(filename, optional = false) {
    const sftp = await this.sftp();
    return new Promise((resolve, reject) => {
      const stream = sftp.createReadStream(filename); const chunks = [];
      stream.on('data', chunk => chunks.push(chunk));
      stream.on('error', error => optional && error.code === 2 ? resolve(null) : reject(new Error(`${this.label}: unable to read required remote file`)));
      stream.on('end', () => resolve(Buffer.concat(chunks)));
    });
  }
  async upload(filename, content, mode = 0o600, owner = 'root:root') {
    must(/^\/[a-zA-Z0-9_./-]+$/.test(filename) && /^(?:root:root|roc-wol:roc-wol)$/.test(owner), 'Unsafe generated upload path');
    const bytes = Buffer.isBuffer(content) ? content : Buffer.from(content);
    const tmp = `${filename}.${nonce()}.new`;
    await this.exec(`mkdir -p ${quote(path.posix.dirname(filename))}`, 'prepare upload');
    const sftp = await this.sftp();
    await new Promise((resolve, reject) => {
      const stream = sftp.createWriteStream(tmp, { flags: 'wx', mode });
      stream.on('error', () => reject(new Error(`${this.label}: private SFTP upload failed`)));
      stream.on('close', resolve); stream.end(bytes);
    });
    await this.exec(`test "$(sha256sum ${quote(tmp)} | cut -d ' ' -f 1)" = ${quote(hash(bytes))}\nchmod ${mode.toString(8)} ${quote(tmp)}\nchown ${quote(owner)} ${quote(tmp)}\nmv -f ${quote(tmp)} ${quote(filename)}`, 'verify and atomically install upload');
  }
  close() { this.files?.end(); this.client.end(); }
}
async function connect(host, expectedKey, auth, label) {
  const { Client } = await import('ssh2');
  return new Promise((resolve, reject) => {
    const client = new Client(); let observed;
    client.on('ready', () => resolve(new Remote(client, label)));
    client.on('error', () => {
      client.end();
      if (!expectedKey && observed) reject(new Error(`Router SSH SHA256 first observation: ${observed}. Verify independently, then rerun inspect --router-host-key ${observed}`));
      else if (observed && observed !== expectedKey) reject(new Error(`${label} SSH host identity mismatch; stopped, never accept a replacement automatically`));
      else reject(new Error(`${label} SSH connection/authentication failed`));
    });
    client.connect({ host, port: 22, username: 'root', ...auth, readyTimeout: 20000, keepaliveInterval: 10000, keepaliveCountMax: 3, hostHash: 'sha256', hostVerifier: key => { observed = key; return !!expectedKey && key === expectedKey; } });
  });
}
async function connections(config) {
  const privateKey = await fs.readFile(path.resolve(ROOT, ARGS['ssh-key'] || 'private/credentials/SSH.pem'));
  const passwordBytes = await fs.readFile(path.resolve(ROOT, ARGS['router-password-file'] || 'private/credentials/router-password.txt'));
  let server;
  try {
    const password = routerPassword(passwordBytes);
    server = await connect(SERVER, SERVER_KEY, { privateKey }, 'server');
    const router = await connect(ROUTER, config?.routerKey || ARGS['router-host-key'], { password }, 'router');
    return { server, router };
  } catch (error) { server?.close(); throw error; }
  finally { privateKey.fill(0); passwordBytes.fill(0); }
}
async function request(url, { method = 'GET', headers = {}, body, address, ...tls } = {}) {
  const target = new URL(url);
  must(['https:', 'http:'].includes(target.protocol), 'Unsupported request protocol');
  return new Promise((resolve, reject) => {
    const req = (target.protocol === 'https:' ? https : http).request(target, {
      method, headers, timeout: 10000, ...tls,
      ...(address ? { lookup: (_host, options, callback) => options.all ? callback(null, [{ address, family: 4 }]) : callback(null, address, 4) } : {}),
    }, res => {
      const chunks = []; let size = 0;
      res.on('data', chunk => { size += chunk.length; if (size > 2 * 1024 * 1024) { req.destroy(); reject(new Error('Probe response exceeds limit')); } else chunks.push(chunk); });
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString('utf8') }));
      res.on('error', () => reject(new Error(`Response failed for ${target.hostname}`)));
    });
    req.on('upgrade', (res, socket) => {
      socket.destroy();
      resolve({ status: 101, headers: res.headers, body: '' });
    });
    req.on('timeout', () => req.destroy(new Error('Probe timeout')));
    req.on('error', () => reject(new Error(`TLS/network probe failed for ${target.hostname}; no insecure TLS fallback is permitted`)));
    if (body) req.write(typeof body === 'string' ? body : JSON.stringify(body));
    req.end();
  });
}
async function fetchOfficial(url) {
  const response = await fetch(url, { headers: { 'User-Agent': 'RocWol-deployer/1.0', Accept: 'application/json' }, signal: AbortSignal.timeout(180000), redirect: 'follow' });
  must(response.ok && new URL(response.url).protocol === 'https:', 'Official release download failed');
  return Buffer.from(await response.arrayBuffer());
}
async function inspectHosts(server, router, config) {
  await server.exec('for command in openssl curl docker systemctl sha256sum tar getent useradd flock; do command -v "$command" >/dev/null; done\ndocker inspect 1Panel-frps-kxiO >/dev/null\ndocker inspect 1Panel-openresty-wmqb >/dev/null', 'check server prerequisites');
  await router.exec('test -x /usr/bin/etherwake\ntest -x /usr/bin/frpc\ncommand -v sha256sum >/dev/null\ncommand -v uci >/dev/null\ntest -x /etc/init.d/frpc', 'check router prerequisites');
  const pingPath = (await router.exec('command -v ping', 'locate router ping')).trim();
  must(/^\/[a-zA-Z0-9_/-]+$/.test(pingPath), 'Router ping executable path is invalid');
  const generator = text(await router.read('/etc/init.d/frpc'));
  must(generator.includes('stcp') && /\bsk\b/.test(generator), 'Existing frpc generator lacks required STCP support; do not replace it');
  const selected = (await router.exec("uci -q get frpc.ssh.name || true\nuci -q get frpc.ssh.local_ip || true\nuci -q get frpc.ssh.local_port || true\nuci -q get frpc.ssh.remote_port || true", 'inspect exact old mapping')).trim().split('\n');
  if (selected.some(Boolean)) must(selected.join('|') === 'Roc|127.0.0.1|80|15201', 'frpc.ssh no longer matches the approved legacy LuCI mapping; preserve and investigate');
  const target = text(await router.read('/etc/config/luci-wol'));
  must(/B0:25:AA:83:C6:EF/i.test(target) && target.includes('br-lan') && target.includes('DESKTOP-IENBTJG'), 'Actual luci-wol target changed; stop before replacing target configuration');
  const topology = await server.exec(`docker exec ${OPENRESTY} openresty -T 2>&1`, 'read OpenResty include topology');
  checkTopology(topology, config?.directHost || ARGS['direct-host'] || 'wol.hubu.wiki');
  const serverPorts = await server.exec('ss -lntp', 'inspect server listeners');
  const routerPorts = await router.exec('netstat -lntp', 'inspect router listeners');
  if (config) {
    for (const [port, listing, names] of [[config.gatewayPort, serverPorts, ['roc-wol']], [config.visitorPort, serverPorts, ['frpc']], [config.agentPort, routerPorts, ['roc-wol-agent']]]) {
      const used = listeningLines(listing, port);
      must(used.every(line => /127\.0\.0\.1:/.test(line) && names.some(name => line.includes(name))), `Configured port ${port} is occupied by an unrelated/non-loopback process; stop rather than replacing it`);
    }
  }
  return { pingPath, serverPorts, routerPorts, topology, oldMapping: selected.some(Boolean) };
}
function listeningLines(listing, port) { return listing.split('\n').filter(line => new RegExp(`\\S+:${port}(?:\\s|$)`).test(line)); }
function choosePort(listing, ports) { const port = ports.find(value => listeningLines(listing, value).length === 0); must(port, `All approved ports ${ports.join('/')} are occupied; no service was stopped`); return port; }
function checkTopology(contents, directHost) {
  const sections = contents.split(/# configuration file /).slice(1);
  const named = [];
  for (const section of sections) {
    const end = section.indexOf(':\n'); if (end < 0) continue;
    const filename = section.slice(0, end); const source = section.slice(end + 2);
    const declarations = [...source.matchAll(/\bserver_name\s+([^;]+);/g)];
    const names = declarations.flatMap(match => match[1].split(/\s+/).filter(Boolean));
    if (names.includes('roc.hubu.wiki') || names.includes(directHost)) {
      const expected = names.includes('roc.hubu.wiki') ? 'roc.hubu.wiki' : directHost;
      must(names.every(name => name === expected), 'Project vhost also serves unrelated hostnames; preserve it and resolve ownership before replacement');
      must([...source.matchAll(/\bserver\s*\{/g)].length === declarations.length, 'Project vhost contains an unnamed server block; preserve it');
      const ports = [...source.matchAll(/\blisten\s+([^;]+);/g)].map(match => match[1].trim().split(/\s+/)[0].split(':').at(-1));
      must(ports.every(port => port === '80' || port === '443'), 'Project vhost also serves unrelated ports; preserve it');
    }
    if (names.includes('roc.hubu.wiki')) named.push(filename);
    if (names.includes(directHost) && filename !== `/usr/local/openresty/nginx/conf/conf.d/${directHost}.conf`) throw new Error('Direct hostname is already configured in an unrelated vhost; use approved alternate or resolve conflict');
  }
  must(named.length > 0 && named.every(name => name === '/usr/local/openresty/nginx/conf/conf.d/roc.hubu.wiki.conf'), 'roc is served from an unexpected include; do not modify unrelated vhosts');
}
async function initialize(server, router) {
  must(ARGS.command === 'inspect', 'Run inspect first to fix ports, identity, and origins in protected configuration');
  const directHost = ARGS['direct-host'] || 'wol.hubu.wiki';
  must(['wol.hubu.wiki', 'wol-direct.hubu.wiki'].includes(directHost), 'Only approved direct origins are supported');
  must(/^[a-f0-9]{64}$/.test(ARGS['router-host-key'] || ''), 'Explicit verified router pin is required');
  const info = await inspectHosts(server, router);
  await server.exec('test ! -e /etc/roc-wol/gateway.json\ntest ! -e /opt/roc-wol/frpc.toml\ntest ! -e /etc/systemd/system/roc-wol.service', 'ensure new project paths are not owned by an existing deployment');
  await router.exec('test ! -e /etc/roc-wol/agent.json\ntest ! -e /usr/sbin/roc-wol-agent', 'ensure agent paths are not owned by an existing deployment');
  const config = {
    version: 1, server: SERVER, serverKey: SERVER_KEY, router: ROUTER, routerKey: ARGS['router-host-key'],
    directHost, fallbackHost: 'roc.hubu.wiki', gatewayPort: choosePort(info.serverPorts, [18081, 28081, 38081]),
    visitorPort: choosePort(info.serverPorts, [18082, 28082, 38082]), agentPort: choosePort(info.routerPorts, [19991, 29991, 39991]),
    pingPath: info.pingPath, stcpKey: crypto.randomBytes(32).toString('base64url'), createdAt: new Date().toISOString(),
  };
  const existingDirect = await server.read(`/opt/1panel/www/conf.d/${directHost}.conf`, true);
  must(!existingDirect, 'Direct hostname has an existing vhost; do not assume it belongs to this deployment');
  await saveConfig(config);
  return { config, info };
}
async function backup(server, router, config, label) {
  const id = `${new Date().toISOString().replaceAll(/[:.]/g, '-')}-${label}-${nonce().slice(0, 8)}`;
  const directory = path.join(PRIVATE, 'backups', id);
  await protect(directory);
  await server.exec(`mkdir -p /root/roc-wol-private/backups/${quote(id)}\nchmod 700 /root/roc-wol-private /root/roc-wol-private/backups /root/roc-wol-private/backups/${quote(id)}`, 'prepare root-only backup');
  for (const [labelName, remote, filename] of [['router-frpc', router, '/etc/config/frpc'], ['roc-vhost', server, ROC_VHOST], ['roc-proxy', server, ROC_PROXY], ['wol-vhost', server, `/opt/1panel/www/conf.d/${config.directHost}.conf`]]) {
    const data = await remote.read(filename, true);
    if (!data) continue;
    await privateWrite(path.join(directory, labelName), data);
    await server.upload(`/root/roc-wol-private/backups/${id}/${labelName}`, data);
  }
  config.lastBackup = id; await saveConfig(config);
}
async function buildArtifacts(config) {
  const manifest = JSON.parse(await fs.readFile(path.join(ROOT, 'dist', 'build-manifest.json'), 'utf8'));
  must(manifest.directOrigin === `https://${config.directHost}` && manifest.fallbackOrigin === 'https://roc.hubu.wiki', 'APK origins differ from deployment; rebuild with this configuration');
  const files = {};
  for (const name of ['roc-wol-linux-amd64', 'roc-wol-agent-linux-arm64', 'roc-wol-release.apk']) {
    const data = await fs.readFile(path.join(ROOT, 'dist', name));
    must(manifest.sha256?.[name] === hash(data), 'Build artifact checksum does not match signed-build manifest');
    files[name] = data;
  }
  return files;
}
function parseFrpsToken(source) {
  let section = '';
  for (let line of source.split(/\r?\n/)) {
    line = line.trim(); if (line.startsWith('#') || !line) continue;
    const heading = line.match(/^\[([^\]]+)\]/); if (heading) { section = heading[1]; continue; }
    const match = line.match(/^(auth\.token|token)\s*=\s*("(?:[^"\\]|\\.)*"|'[^']*')\s*(?:#.*)?$/);
    if (match && (match[1] === 'auth.token' || section === 'auth')) {
      const token = match[2].startsWith('"') ? parsePrivateJSON(match[2]) : match[2].slice(1, -1);
      must(token.length > 0 && !/[\r\n\0]/.test(token), 'Existing frps token has unsupported format'); return token;
    }
  }
  throw new Error('Cannot safely parse the existing frps token; shared frps configuration is not modified');
}
async function installFrpc(server) {
  const name = 'frp_0.69.1_linux_amd64.tar.gz';
  const base = 'https://github.com/fatedier/frp/releases/download/v0.69.1/';
  const checksums = text(await fetchOfficial(base + 'frp_sha256_checksums.txt'));
  const expected = checksums.match(new RegExp(`^([a-fA-F0-9]{64})\\s+\\*?${name.replaceAll('.', '\\.')}\\s*$`, 'm'))?.[1]?.toLowerCase();
  must(expected === '7be257b72dbbc60bcb3e0e25a5afd1dfac7b63f897084864d3c956dd3d5674e1', 'Official frp release checksum differs from the pinned release');
  const tarball = await fetchOfficial(base + name);
  must(hash(tarball) === expected, 'Official frp checksum mismatch');
  await server.upload('/opt/roc-wol/frp-release.tar.gz', tarball);
  await server.exec(`mkdir -p /opt/roc-wol/frp-unpack\ntar -xzf /opt/roc-wol/frp-release.tar.gz -C /opt/roc-wol/frp-unpack ${quote('frp_0.69.1_linux_amd64/frpc')}\ninstall -o root -g root -m 755 /opt/roc-wol/frp-unpack/frp_0.69.1_linux_amd64/frpc /opt/roc-wol/bin/frpc.new\nmv -f /opt/roc-wol/bin/frpc.new /opt/roc-wol/bin/frpc\nrm -rf /opt/roc-wol/frp-unpack /opt/roc-wol/frp-release.tar.gz`, 'install verified project visitor');
}
async function pki(server, router, config) {
  await server.exec('mkdir -p /root/roc-wol-private/pki /etc/roc-wol/pki\nchmod 700 /root/roc-wol-private /root/roc-wol-private/pki\nchmod 750 /etc/roc-wol /etc/roc-wol/pki\nchown root:roc-wol /etc/roc-wol /etc/roc-wol/pki', 'prepare certificate directories');
  const caExists = await server.read('/root/roc-wol-private/pki/ca.pem', true);
  if (!caExists) {
    await server.exec('test ! -e /root/roc-wol-private/pki/ca.key\nopenssl ecparam -name prime256v1 -genkey -noout -out /root/roc-wol-private/pki/ca.key\nopenssl req -new -x509 -sha256 -days 3650 -key /root/roc-wol-private/pki/ca.key -subj /CN=Roc-WOL-Private-CA -addext basicConstraints=critical,CA:TRUE,pathlen:0 -addext keyUsage=critical,keyCertSign,cRLSign -out /root/roc-wol-private/pki/ca.pem', 'create project private CA');
  }
  const routerCertificate = await router.read('/etc/roc-wol/pki/server.pem', true);
  const routerPrivateKey = await router.read('/etc/roc-wol/pki/server.key', true);
  let routerPairValid = false;
  try {
    routerPairValid = !!routerCertificate && !!routerPrivateKey && new crypto.X509Certificate(routerCertificate).checkPrivateKey(crypto.createPrivateKey(routerPrivateKey));
    if (routerPairValid) {
      const issuedCertificate = await server.read('/root/roc-wol-private/pki/agent.pem', true);
      routerPairValid = !!issuedCertificate && issuedCertificate.equals(routerCertificate);
    }
  } catch { routerPairValid = false; }
  finally { routerPrivateKey?.fill(0); }
  const rotate = !!ARGS['rotate-mtls'] || !(await server.read('/root/roc-wol-private/pki/gateway.pem', true)) || !routerPairValid;
  if (rotate) {
    for (const [name, extensions] of [['agent', 'subjectAltName=DNS:router.roc-wol.internal\nextendedKeyUsage=serverAuth'], ['gateway', 'subjectAltName=URI:urn:roc-wol:gateway\nextendedKeyUsage=clientAuth']]) {
      await server.upload(`/root/roc-wol-private/pki/${name}.ext`, `basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n${extensions}\n`);
      await server.exec(`openssl ecparam -name prime256v1 -genkey -noout -out /root/roc-wol-private/pki/${name}.key.new\nopenssl req -new -sha256 -key /root/roc-wol-private/pki/${name}.key.new -subj /CN=roc-wol-${name} -out /root/roc-wol-private/pki/${name}.csr\nopenssl x509 -req -sha256 -days 1095 -set_serial 0x${nonce()} -in /root/roc-wol-private/pki/${name}.csr -CA /root/roc-wol-private/pki/ca.pem -CAkey /root/roc-wol-private/pki/ca.key -extfile /root/roc-wol-private/pki/${name}.ext -out /root/roc-wol-private/pki/${name}.pem.new\nopenssl verify -CAfile /root/roc-wol-private/pki/ca.pem /root/roc-wol-private/pki/${name}.pem.new >/dev/null\nmv -f /root/roc-wol-private/pki/${name}.key.new /root/roc-wol-private/pki/${name}.key\nmv -f /root/roc-wol-private/pki/${name}.pem.new /root/roc-wol-private/pki/${name}.pem`, 'issue project leaf certificate');
    }
  }
  const ca = await server.read('/root/roc-wol-private/pki/ca.pem');
  await server.upload('/etc/roc-wol/pki/ca.pem', ca, 0o640, 'root:root');
  await server.exec('chown root:roc-wol /etc/roc-wol/pki/ca.pem', 'grant CA certificate read access');
  for (const suffix of ['pem', 'key']) {
    const client = await server.read(`/root/roc-wol-private/pki/gateway.${suffix}`);
    await server.upload(`/etc/roc-wol/pki/client.${suffix}`, client, 0o600, 'roc-wol:roc-wol');
    if (rotate) {
      const agentKeyOrCert = await server.read(`/root/roc-wol-private/pki/agent.${suffix}`);
      try { await router.upload(`/etc/roc-wol/pki/server.${suffix}`, agentKeyOrCert); }
      finally { if (suffix === 'key') agentKeyOrCert.fill(0); }
    }
    if (suffix === 'key') client.fill(0);
  }
  // Only the CA and gateway key have server-side backups. Agent keys persist solely on the router.
  await server.exec('rm -f /root/roc-wol-private/pki/agent.key /root/roc-wol-private/pki/agent.csr', 'remove transferred agent private-key staging file');
  await router.upload('/etc/roc-wol/pki/ca.pem', ca);
  await router.exec('chmod 700 /etc/roc-wol /etc/roc-wol/pki', 'protect agent key directory');
  config.privateCertificateExpires = (await server.exec('openssl x509 -in /root/roc-wol-private/pki/agent.pem -noout -enddate', 'read private certificate expiry')).trim();
  await saveConfig(config);
}
async function resources(server) {
  for (const name of ['roc-wol.service', 'roc-wol-visitor.service', 'roc-wol-renew.service', 'roc-wol-renew.timer']) await server.upload(`/etc/systemd/system/${name}`, await fs.readFile(path.join(ROOT, 'deploy/server', name)), 0o644);
  await server.upload('/opt/roc-wol/bin/renew-certificates.sh', await fs.readFile(path.join(ROOT, 'deploy/server/renew-certificates.sh')), 0o700);
}
async function privateStatus(server, config) {
  const reply = await server.exec(`curl --silent --show-error --fail --max-time 10 --noproxy '*' --resolve router.roc-wol.internal:${config.visitorPort}:127.0.0.1 --cacert /etc/roc-wol/pki/ca.pem --cert /etc/roc-wol/pki/client.pem --key /etc/roc-wol/pki/client.key https://router.roc-wol.internal:${config.visitorPort}/_roc-agent/v1/status`, 'verify end-to-end private mTLS');
  const data = JSON.parse(reply);
  must(data.targetId === 'desktop' && ['reachable', 'unreachable', 'unknown'].includes(data.reachability), 'Agent returned an invalid status contract');
  await server.exec(`if curl --silent --max-time 5 --noproxy '*' --resolve router.roc-wol.internal:${config.visitorPort}:127.0.0.1 --cacert /etc/roc-wol/pki/ca.pem https://router.roc-wol.internal:${config.visitorPort}/_roc-agent/v1/status >/dev/null 2>&1; then exit 1; fi\nif curl --silent --max-time 5 --noproxy '*' --cacert /etc/roc-wol/pki/ca.pem --cert /etc/roc-wol/pki/client.pem --key /etc/roc-wol/pki/client.key https://127.0.0.1:${config.visitorPort}/_roc-agent/v1/status >/dev/null 2>&1; then exit 1; fi`, 'verify certificate-less and wrong-name rejection');
  await server.exec(`if curl --silent --max-time 5 --noproxy '*' --resolve router.roc-wol.internal:${config.visitorPort}:127.0.0.1 --cacert /etc/ssl/certs/ca-certificates.crt --cert /etc/roc-wol/pki/client.pem --key /etc/roc-wol/pki/client.key https://router.roc-wol.internal:${config.visitorPort}/_roc-agent/v1/status >/dev/null 2>&1; then exit 1; fi`, 'verify wrong trust-root rejection');
  return data;
}
async function stagePrivate(server, router, config) {
  const files = await buildArtifacts(config);
  const info = await inspectHosts(server, router, config);
  must(info.pingPath === config.pingPath, 'Router ping path changed; inspect and reconcile configuration');
  await backup(server, router, config, 'stage-private');
  await server.exec('getent passwd roc-wol >/dev/null || useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin roc-wol\nmkdir -p /opt/roc-wol/bin /etc/roc-wol /var/lib/roc-wol\nchmod 755 /opt/roc-wol /opt/roc-wol/bin\nchown roc-wol:roc-wol /var/lib/roc-wol\nchmod 700 /var/lib/roc-wol', 'prepare isolated project service account');
  await server.upload('/opt/roc-wol/bin/roc-wol', files['roc-wol-linux-amd64'], 0o755);
  await router.upload('/usr/sbin/roc-wol-agent', files['roc-wol-agent-linux-arm64'], 0o755);
  await server.exec('/opt/roc-wol/bin/roc-wol --help >/dev/null', 'verify server executable entrypoint');
  await router.exec('/usr/sbin/roc-wol-agent --help >/dev/null', 'verify router executable entrypoint');
  await pki(server, router, config);
  const gateway = { listen: `127.0.0.1:${config.gatewayPort}`, publicOrigins: [`https://${config.directHost}`, 'https://roc.hubu.wiki'], agentURL: `https://127.0.0.1:${config.visitorPort}`, agentServerName: 'router.roc-wol.internal', caFile: '/etc/roc-wol/pki/ca.pem', clientCertFile: '/etc/roc-wol/pki/client.pem', clientKeyFile: '/etc/roc-wol/pki/client.key', dataFile: '/var/lib/roc-wol/gateway.db', adminSocket: '/run/roc-wol/admin.sock' };
  const agent = { listen: `127.0.0.1:${config.agentPort}`, serverCertFile: '/etc/roc-wol/pki/server.pem', serverKeyFile: '/etc/roc-wol/pki/server.key', clientCAFile: '/etc/roc-wol/pki/ca.pem', clientURI: 'urn:roc-wol:gateway', dataFile: '/etc/roc-wol/agent.db', luciOrigin: 'http://127.0.0.1:80', pingPath: config.pingPath, targets: [{ id: 'desktop', name: 'DESKTOP-IENBTJG', mac: 'B0:25:AA:83:C6:EF', interface: 'br-lan', broadcast: true, ip: '192.168.2.100' }] };
  await server.upload('/etc/roc-wol/gateway.json', json(gateway), 0o600, 'roc-wol:roc-wol');
  await router.upload('/etc/roc-wol/agent.json', json(agent));
  const token = parseFrpsToken(text(await server.read(FRPS_CONFIG)));
  const visitor = `serverAddr = "127.0.0.1"\nserverPort = 7000\nauth.method = "token"\nauth.token = ${JSON.stringify(token)}\ntransport.tls.enable = true\n\n[[visitors]]\nname = "roc-wol-agent-visitor"\ntype = "stcp"\nserverName = "roc-wol-agent"\nsecretKey = ${JSON.stringify(config.stcpKey)}\nbindAddr = "127.0.0.1"\nbindPort = ${config.visitorPort}\n`;
  await installFrpc(server);
  await server.upload('/opt/roc-wol/frpc.toml', visitor, 0o600, 'roc-wol:roc-wol');
  await server.exec('/opt/roc-wol/bin/frpc verify -c /opt/roc-wol/frpc.toml >/dev/null', 'validate generated visitor configuration');
  await resources(server);
  await router.upload('/etc/init.d/roc-wol-agent', await fs.readFile(path.join(ROOT, 'deploy/router/roc-wol-agent')), 0o755);
  const priorName = (await router.exec('uci -q get frpc.roc_wol_agent.name || true', 'check project UCI ownership')).trim();
  must(!priorName || priorName === 'roc-wol-agent', 'Named UCI section is owned by an unrelated proxy');
  // Secrets are in stdin to sh, never process command arguments or logs.
  await router.exec(`uci batch <<'ROC_WOL_UCI'\nset frpc.roc_wol_agent=conf\nset frpc.roc_wol_agent.name='roc-wol-agent'\nset frpc.roc_wol_agent.type='stcp'\nset frpc.roc_wol_agent.local_ip='127.0.0.1'\nset frpc.roc_wol_agent.local_port='${config.agentPort}'\nset frpc.roc_wol_agent.enabled='1'\nset frpc.roc_wol_agent.sk=${quote(config.stcpKey)}\nROC_WOL_UCI\nuci -q delete frpc.roc_wol_agent.remote_port || true\nuci commit frpc\n/etc/init.d/roc-wol-agent enable\n/etc/init.d/roc-wol-agent restart`, 'install precise project UCI section');
  log('Restarting only this router frpc: its legacy LuCI mapping will briefly disconnect. Shared frps stays running.');
  await router.exec('/etc/init.d/frpc restart', 'restart router tunnel after UCI change');
  await server.exec('systemctl daemon-reload\nsystemctl enable roc-wol-visitor.service roc-wol.service\nsystemctl restart roc-wol-visitor.service\nsystemctl restart roc-wol.service', 'start isolated project services');
  await retry(() => privateStatus(server, config), 10);
  config.privateStagedAt = new Date().toISOString(); await saveConfig(config);
  log(`Private STCP+mTLS status verified. ${config.privateCertificateExpires}; leaf renewal requires stage --rotate-mtls. Public routing not switched.`);
}
async function retry(fn, attempts) { let last; for (let i = 0; i < attempts; i++) { try { return await fn(); } catch (error) { last = error; if (i + 1 < attempts) await new Promise(resolve => setTimeout(resolve, 1500)); } } throw last; }
function acmeLocation() { return '    location ^~ /.well-known/acme-challenge { root /usr/share/nginx/html; }\n'; }
function bootstrapVhost(host) { return `# roc-wol managed HTTP01 bootstrap; no unauthenticated LuCI upstream\nserver {\n    listen 80;\n    listen [::]:80;\n    server_name ${host};\n${acmeLocation()}    location / { return 503; }\n}\n`; }
function proxyLocation(config) {
  return `location / {\n    proxy_pass http://127.0.0.1:${config.gatewayPort};\n    proxy_http_version 1.1;\n    proxy_set_header Host $host;\n    proxy_set_header X-Forwarded-Proto https;\n    proxy_set_header X-Forwarded-For "";\n    proxy_set_header X-Real-IP "";\n    proxy_set_header Forwarded "";\n    proxy_set_header CF-Connecting-IP "";\n    proxy_set_header X-Forwarded-User "";\n    proxy_set_header X-Auth-Request-User "";\n    proxy_set_header X-Remote-User "";\n    proxy_set_header Upgrade $http_upgrade;\n    proxy_set_header Connection $roc_wol_connection_upgrade;\n    proxy_buffering off;\n    proxy_request_buffering off;\n    proxy_cache off;\n    proxy_read_timeout 960s;\n    proxy_send_timeout 960s;\n}\n`;
}
function publicVhost(host, config, includeProxy = false) {
  return `# roc-wol managed: every non-ACME path goes to the authenticated gateway\nserver {\n    listen 80;\n    listen [::]:80;\n    server_name ${host};\n${acmeLocation()}    location / { return 301 https://${host}$request_uri; }\n}\nserver {\n    listen 443 ssl;\n    listen [::]:443 ssl;\n    server_name ${host};\n    ssl_certificate /www/sites/${host}/ssl/fullchain.pem;\n    ssl_certificate_key /www/sites/${host}/ssl/privkey.pem;\n    ssl_protocols TLSv1.2 TLSv1.3;\n    client_max_body_size 128m;\n    client_header_buffer_size 4k;\n    large_client_header_buffers 4 4k;\n    access_log off;\n${acmeLocation()}${includeProxy ? '    include /www/sites/roc.hubu.wiki/proxy/root.conf;\n' : proxyLocation(config)}\n}\n`;
}
async function reloadNginx(server) { await server.exec(`docker exec ${OPENRESTY} openresty -t >/dev/null 2>&1\ndocker exec ${OPENRESTY} openresty -s reload`, 'validate and reload OpenResty without restarting it'); }
async function replaceNginx(server, updates) {
  const old = new Map();
  for (const [filename, content] of updates) { old.set(filename, await server.read(filename, true)); await server.upload(filename, content, 0o644); }
  try { await reloadNginx(server); }
  catch (error) {
    // This transaction only rolls back candidate text before a successful reload. It is not a LuCI fail-open fallback.
    for (const [filename, content] of old) if (content) await server.upload(filename, content, 0o644); else await server.exec(`rm -f ${quote(filename)}`, 'remove invalid new candidate');
    throw error;
  }
}
async function directDNS(config) {
  // The workstation VPN may return synthetic 198.18/15 addresses. Validate
  // public records over authenticated DoH instead of its intercepted resolver.
  const resolve = async type => {
    const response = await fetch(`https://dns.google/resolve?name=${encodeURIComponent(config.directHost)}&type=${type}`, { signal: AbortSignal.timeout(10000) });
    must(response.ok, 'Public DNS lookup failed');
    const result = await response.json();
    must(result.Status === 0, 'Public DNS lookup returned an error');
    return (result.Answer || []).filter(answer => answer.type === type).map(answer => answer.data);
  };
  const addresses = await resolve(1);
  must(addresses.length > 0 && addresses.every(address => address === SERVER), 'Direct public DNS does not resolve exclusively to the approved origin IP');
  const ipv6 = await resolve(28);
  must(ipv6.length === 0, 'Direct hostname has unverified AAAA records; resolve ownership in DNS console before continuing');
}
async function http01Probe(server, config) {
  const value = crypto.randomBytes(24).toString('base64url');
  const filename = `/opt/1panel/apps/openresty/openresty/root/.well-known/acme-challenge/roc-wol-${nonce()}`;
  await server.upload(filename, value, 0o644);
  await server.exec('chmod 755 /opt/1panel/apps/openresty/openresty/root/.well-known /opt/1panel/apps/openresty/openresty/root/.well-known/acme-challenge', 'allow public HTTP01 token directory traversal');
  try {
    for (const host of [config.directHost, config.fallbackHost]) {
      const result = await request(`http://${host}/.well-known/acme-challenge/${path.posix.basename(filename)}`);
      must(result.status === 200 && result.body === value, `${host}: HTTP01 probe failed (redirect/edge challenge/ICP interception also block issuance). Fix only the relevant host before continuing`);
    }
  } finally { await server.exec(`rm -f ${quote(filename)}`, 'remove HTTP01 probe'); }
}
async function certificateImage(server, config) {
  if (!config.certbotImage) {
    await server.exec('docker pull certbot/certbot:latest >/dev/null', 'download official current certbot image');
    const digests = JSON.parse((await server.exec("docker image inspect certbot/certbot:latest --format '{{json .RepoDigests}}'", 'resolve certbot immutable digest')).trim());
    config.certbotImage = digests.find(item => /^certbot\/certbot@sha256:[a-f0-9]{64}$/.test(item));
    must(config.certbotImage, 'Official certbot image digest unavailable'); await saveConfig(config);
  }
  must(/^certbot\/certbot@sha256:[a-f0-9]{64}$/.test(config.certbotImage), 'Invalid pinned certbot image');
  return config.certbotImage;
}
async function stageHTTPS(server, router, config) {
  await privateStatus(server, config);
  await directDNS(config);
  await inspectHosts(server, router, config);
  await backup(server, router, config, 'stage-https');
  const directPath = `/opt/1panel/www/conf.d/${config.directHost}.conf`;
  const current = await server.read(directPath, true);
  must(!current || text(current).startsWith('# roc-wol managed'), 'Existing direct vhost is not owned by this project');
  const roc = text(await server.read(ROC_VHOST));
  let preparedRoc = roc;
  if (!roc.startsWith('# roc-wol managed')) {
    const redirect = /if\s*\(\s*\$scheme\s*=\s*http\s*\)\s*\{\s*return\s+301\s+https:\/\/[^;]+;\s*\}/g;
    const matches = [...roc.matchAll(redirect)];
    must(matches.length <= 1, 'Unexpected existing HTTP redirect structure; preserve it and investigate');
    preparedRoc = roc.replace(redirect, `set $roc_wol_redirect 0;\n    if ($scheme = http) { set $roc_wol_redirect 1; }\n    if ($uri ~ "^/\\.well-known/acme-challenge/") { set $roc_wol_redirect 0; }\n    if ($roc_wol_redirect = 1) { return 301 https://roc.hubu.wiki$request_uri; }`);
    must(/location\s+\^~\s+\/\.well-known\/acme-challenge\s*\{\s*(?:allow\s+all;\s*)?root\s+\/usr\/share\/nginx\/html;\s*\}/.test(preparedRoc), 'Existing roc HTTP01 location differs from approved mount; refusing broad vhost overwrite before cutover');
  }
  await replaceNginx(server, [[ROC_VHOST, preparedRoc], [directPath, current || bootstrapVhost(config.directHost)]]);
  await http01Probe(server, config);
  const image = await certificateImage(server, config);
  await server.exec('mkdir -p /opt/roc-wol/acme/etc /opt/roc-wol/acme/staging\nchmod 700 /opt/roc-wol/acme /opt/roc-wol/acme/etc /opt/roc-wol/acme/staging', 'prepare private ACME state');
  const common = `--webroot -w /var/www/acme --cert-name roc-wol -d ${config.directHost} -d roc.hubu.wiki --non-interactive --agree-tos --register-unsafely-without-email`;
  const existing = await server.read('/opt/roc-wol/acme/etc/live/roc-wol/fullchain.pem', true);
  if (!existing) {
    await server.exec(`docker run --rm -v /opt/roc-wol/acme/staging:/etc/letsencrypt -v /opt/1panel/apps/openresty/openresty/root:/var/www/acme ${quote(image)} certonly --staging ${common} >/dev/null`, 'verify both HTTP01 authorizations with staging CA');
    await server.exec(`docker run --rm -v /opt/roc-wol/acme/etc:/etc/letsencrypt -v /opt/1panel/apps/openresty/openresty/root:/var/www/acme ${quote(image)} certonly ${common} >/dev/null`, 'issue single production SAN certificate');
  }
  await server.upload('/etc/roc-wol/renew.env', `CERTBOT_IMAGE=${quote(image)}\nDIRECT_HOST=${quote(config.directHost)}\nFALLBACK_HOST='roc.hubu.wiki'\nOPENRESTY_CONTAINER=${quote(OPENRESTY)}\n`);
  await server.exec('/opt/roc-wol/bin/renew-certificates.sh --install-only', 'validate public chain/SAN/key and atomically install certificate');
  const upgradePath = '/opt/1panel/www/conf.d/00-roc-wol-upgrade.conf';
  const upgradeContents = '# roc-wol managed WebSocket connection mapping\nmap $http_upgrade $roc_wol_connection_upgrade { default upgrade; "" close; }\n';
  const priorUpgrade = await server.read(upgradePath, true);
  must(!priorUpgrade || text(priorUpgrade) === upgradeContents, 'WebSocket map path contains unrelated configuration; do not overwrite it');
  await replaceNginx(server, [[upgradePath, upgradeContents], [directPath, publicVhost(config.directHost, config)]]);
  await server.exec('systemctl daemon-reload\nsystemctl enable --now roc-wol-renew.timer', 'enable pinned-certificate renewal timer');
  await publicGate(config, config.directHost, true);
  const rocSource = await request(`https://roc.hubu.wiki/_wol/manage`, { address: SERVER });
  must(rocSource.status >= 200 && rocSource.status < 500, 'Publicly trusted roc origin TLS probe failed');
  config.httpsStagedAt = new Date().toISOString(); await saveConfig(config);
  log(`Direct authenticated origin verified: https://${config.directHost}. roc still retains its pre-cutover route. Pair and verify the phone now; do not pair through roc until cutover.`);
  log('Cloudflare console prerequisite now safe: roc host-only ssl=strict; disable content injection and bypass cache for this host. Read back the actual rule, not merely an HTTPS result.');
}
async function publicGate(config, host, direct = false) {
  const opts = direct ? { address: SERVER } : {};
  for (const route of ['/', '/cgi-bin/luci/', '/ubus', '/luci-static/resources/view/wol.js']) {
    const response = await request(`https://${host}${route}`, opts);
    must(response.status === 401 && response.headers['cache-control']?.includes('no-store'), `${host}: unauthenticated management path was not denied by gateway`);
  }
  const reserved = await request(`https://${host}/_roc-agent/v1/status`, opts);
  must(reserved.status === 404 || reserved.status === 403, `${host}: agent reserved path was exposed`);
  const upgrade = await request(`https://${host}/ubus`, { ...opts, headers: { Connection: 'Upgrade', Upgrade: 'websocket', Origin: `https://${host}`, 'Sec-WebSocket-Version': '13', 'Sec-WebSocket-Key': crypto.randomBytes(16).toString('base64') } });
  must(upgrade.status === 401, `${host}: unauthenticated upgrade was not denied`);
  const forged = await request(`https://${host}/cgi-bin/luci/`, { ...opts, headers: { Cookie: '__Host-roc_session=invalid', 'X-Forwarded-User': 'root', 'X-Auth-Request-User': 'root' } });
  must(forged.status === 401, `${host}: forged authentication headers were accepted`);
  const page = await request(`https://${host}/_wol/manage`, opts);
  must(page.status === 200 && page.headers['cache-control']?.includes('no-store') && page.headers['content-security-policy']?.includes("frame-ancestors 'none'"), `${host}: management authorization page security headers missing`);
}
async function api(origin, route, payload, token) {
  const response = await request(`${origin}/_wol/v1/${route}`, { method: payload ? 'POST' : 'GET', headers: { Origin: origin, ...(payload ? { 'Content-Type': 'application/json' } : {}), ...(token ? { Authorization: `Bearer ${token}` } : {}) }, body: payload });
  let body; try { body = JSON.parse(response.body); } catch { throw new Error('Public API returned non-JSON; a Cloudflare challenge/edge injection may be blocking this origin'); }
  return { status: response.status, body, headers: response.headers };
}
function sign(key, message) { return crypto.sign('sha256', Buffer.from(message), key).toString('base64url'); }
async function protocolSmoke(server, config, host) {
  // Fresh short-lived deployment identity; no wake side effect and no real phone private key is exported.
  const wake = crypto.generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
  const manage = crypto.generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
  const wakeDER = wake.publicKey.export({ type: 'spki', format: 'der' });
  const manageDER = manage.publicKey.export({ type: 'spki', format: 'der' });
  const origin = `https://${host}`;
  let deviceId;
  try {
    const code = (await server.exec('/opt/roc-wol/bin/roc-wol pair-code --socket /run/roc-wol/admin.sock --ttl 2m', 'create temporary protocol-smoke enrollment')).trim();
    const challenge = await api(origin, 'pair/challenge', { code, deviceName: `deployment-check-${nonce().slice(0, 8)}`, wakePublicKey: wakeDER.toString('base64url'), managePublicKey: manageDER.toString('base64url') });
    must(challenge.status === 200, 'Temporary pair challenge failed (ensure an available device slot)');
    const c = challenge.body;
    const message = `ROCWOL-PAIR-V1\nroc-wol\n${c.challengeId}\n${c.nonce}\n${hash(wakeDER)}\n${hash(manageDER)}`;
    const paired = await api(origin, 'pair/complete', { challengeId: c.challengeId, wakeSignature: sign(wake.privateKey, message), manageSignature: sign(manage.privateKey, message) });
    must(paired.status >= 200 && paired.status < 300 && /^[a-f0-9]{32}$/.test(paired.body.deviceId || ''), 'Temporary dual-key pairing failed');
    deviceId = paired.body.deviceId;
    const auth = await api(origin, 'auth/challenge', { deviceId });
    must(auth.status === 200, 'Wake authentication challenge failed');
    const signed = `ROCWOL-WAKE-AUTH-V1\nroc-wol\n${deviceId}\n${auth.body.challengeId}\n${auth.body.nonce}`;
    const tokenReply = await api(origin, 'auth/complete', { deviceId, challengeId: auth.body.challengeId, signature: sign(wake.privateKey, signed) });
    must(tokenReply.status === 200, 'Wake authentication completion failed');
    const token = tokenReply.body.accessToken;
    const target = await api(origin, 'target', null, token);
    must(target.status === 200 && target.body.targetId === 'desktop' && target.body.router === 'available', 'Authenticated target cannot reach private agent');
    const otherHost = host === config.directHost ? config.fallbackHost : config.directHost;
    const canonical = (challenge, sessionMinutes, allowAllDevices, selectedOrigin = origin) =>
      ['ROCWOL-MANAGE-V2', 'roc-wol', deviceId, challenge.challengeId, challenge.nonce, selectedOrigin, String(sessionMinutes), allowAllDevices ? 'all-devices' : 'browser-only'].join('\n');
    const statusMatches = async grant => {
      const status = await api(origin, 'manage/status', null, token);
      must(status.status === 200 && status.body.active === true && status.body.expiresAt === grant.expiresAt &&
        status.body.sessionMinutes === grant.sessionMinutes && status.body.allowAllDevices === grant.allowAllDevices &&
        status.body.origin === origin, 'Management status changed the authorization scope or expiry');
    };
    const authorize = async (sessionMinutes, allowAllDevices) => {
      const management = await api(origin, 'manage/challenge', { origin, sessionMinutes, allowAllDevices }, token);
      must(management.status === 200, `${sessionMinutes}-minute management challenge failed`);
      const payload = { challengeId: management.body.challengeId, signature: sign(manage.privateKey, canonical(management.body, sessionMinutes, allowAllDevices)) };
      const override = await api(origin, 'manage/authorize', { ...payload, sessionMinutes: 30 }, token);
      must(override.status === 400, 'Management authorization accepted a duration override');
      const authorized = await api(origin, 'manage/authorize', payload, token);
      const remaining = Date.parse(authorized.body.expiresAt) - Date.parse(authorized.headers.date);
      must(authorized.status === 200 && authorized.body.active === true &&
        authorized.body.sessionMinutes === sessionMinutes && authorized.body.allowAllDevices === allowAllDevices &&
        authorized.body.origin === origin && remaining > 0 && remaining <= sessionMinutes * 60000 + 1000,
      'Hardware signature did not establish the selected bounded authorization');
      await statusMatches(authorized.body); // Active before requesting or redeeming any browser ticket.
      return authorized.body;
    };
    const browserTicket = async grant => {
      const ticket = await api(origin, 'manage/browser', {}, token);
      must(ticket.status === 200 && ticket.body.authorizationExpiresAt === grant.expiresAt, 'Browser ticket changed authorization expiry');
      const ticketExpiry = Date.parse(ticket.body.expiresAt);
      must(ticketExpiry <= Date.parse(grant.expiresAt) && ticketExpiry > Date.parse(ticket.headers.date) &&
        ticketExpiry <= Date.parse(ticket.headers.date) + 61000, 'Browser ticket exceeds one minute or its authorization');
      const url = new URL(ticket.body.url);
      must(url.origin === origin && url.pathname === '/_wol/manage' && !url.search, 'Management ticket origin/path mismatch');
      const params = new URLSearchParams(url.hash.slice(1));
      must(params.size === 1 && /^[A-Za-z0-9_-]{43}$/.test(params.get('t') || ''), 'Management browser ticket is malformed');
      await statusMatches(grant);
      return params.get('t');
    };
    const redeem = async (ticket, grant) => {
      const redeemed = await api(origin, 'manage/redeem', { ticket });
      must(redeemed.status === 200 && redeemed.body.redirect === '/cgi-bin/luci/', 'Management ticket redemption failed');
      const cookieHeader = (redeemed.headers['set-cookie'] || []).find(value => value.startsWith('__Host-roc_session='));
      must(cookieHeader && /;\s*Secure/i.test(cookieHeader) && /;\s*HttpOnly/i.test(cookieHeader) &&
        /;\s*SameSite=Strict/i.test(cookieHeader) && !/;\s*Domain=/i.test(cookieHeader), 'Management session cookie constraints failed');
      const maxAge = Number(cookieHeader.match(/;\s*Max-Age=(\d+)/i)?.[1]);
      const cookieExpiry = Date.parse(redeemed.headers.date) + maxAge * 1000;
      must(maxAge > 0 && Math.abs(cookieExpiry - Date.parse(grant.expiresAt)) <= 2000, 'Browser cookie did not inherit existing authorization expiry');
      await statusMatches(grant);
      return cookieHeader.split(';')[0];
    };
    const reachesLogin = panel => {
      // No LuCI credentials are supplied: a real login form must remain, not merely an HTTP success.
      return [200, 403].includes(panel.status) && /<form\b/i.test(panel.body) &&
        /<input\b[^>]*\bname=["']luci_username["']/i.test(panel.body) &&
        /<input\b[^>]*\bname=["']luci_password["']/i.test(panel.body);
    };
    const end = async cookie => {
      const ended = await api(origin, 'manage/end', {}, token);
      must(ended.status === 200 && ended.body.ended === true, 'Manual management cancellation failed');
      const status = await api(origin, 'manage/status', null, token);
      must(status.status === 200 && status.body.active === false, 'Ended authorization remains active');
      for (const headers of [{}, { Cookie: cookie }]) {
        must((await request(`${origin}/cgi-bin/luci/`, { headers })).status === 401, 'Ended authorization still reached LuCI');
      }
      must([401, 403].includes((await api(origin, 'manage/browser', {}, token)).status), 'Wake bearer created a browser ticket without active authorization');
      must((await api(origin, 'target', null, token)).status === 200, 'Ending management revoked wake access');
      const repeated = await api(origin, 'manage/end', {}, token);
      must(repeated.status === 200 && repeated.body.ended === true, 'Manual cancellation was not idempotent');
    };
    must((await api(origin, 'manage/status', null, token)).body.active === false, 'Fresh device unexpectedly has management authorization');
    must([401, 403].includes((await api(origin, 'manage/browser', {}, token)).status), 'Wake bearer alone created management access');
    must((await request(`${origin}/cgi-bin/luci/`)).status === 401, 'Anonymous LuCI access was not denied');
    const grant = await authorize(10, false);
    must((await request(`${origin}/cgi-bin/luci/`)).status === 401, 'Browser-only authorization opened anonymous LuCI access');
    await new Promise(resolve => setTimeout(resolve, 1100));
    await statusMatches(grant);
    const opaque = await browserTicket(grant);
    const redeemOverride = await api(origin, 'manage/redeem', { ticket: opaque, sessionMinutes: 30 });
    must(redeemOverride.status === 400, 'Management redemption accepted a duration override');
    await new Promise(resolve => setTimeout(resolve, 1100));
    let cookie = await redeem(opaque, grant);
    must(reachesLogin(await request(`${origin}/cgi-bin/luci/`, { headers: { Cookie: cookie } })), 'Authorized browser did not reach separate LuCI login');
    const repeat = await api(origin, 'manage/redeem', { ticket: opaque });
    must(repeat.status === 409 || repeat.status === 401, 'Management ticket replay was accepted');
    const pendingTicket = await browserTicket(grant);
    await end(cookie);
    must([401, 409].includes((await api(origin, 'manage/redeem', { ticket: pendingTicket })).status), 'Manual cancellation left an unused browser ticket valid');
    // Ten device challenge attempts total: one wake auth, four grants, three signature
    // scope-tamper cases and two invalid durations. Pairing uses the separate global quota.
    for (const sessionMinutes of [0, 31]) {
      const invalid = await api(origin, 'manage/challenge', { origin, sessionMinutes, allowAllDevices: false }, token);
      must(invalid.status === 400, `Invalid management duration ${sessionMinutes} was not rejected`);
    }
    for (const [minutes, allDevices, signedOrigin] of [[30, false, origin], [10, true, origin], [10, false, `https://${otherHost}`]]) {
      const challenge = await api(origin, 'manage/challenge', { origin, sessionMinutes: 10, allowAllDevices: false }, token);
      must(challenge.status === 200, 'Scope-tamper management challenge failed');
      const rejected = await api(origin, 'manage/authorize', {
        challengeId: challenge.body.challengeId,
        signature: sign(manage.privateKey, canonical(challenge.body, minutes, allDevices, signedOrigin)),
      }, token);
      must([401, 403].includes(rejected.status), 'Management signature accepted a changed duration, access scope or origin');
      const status = await api(origin, 'manage/status', null, token);
      must(status.status === 200 && status.body.active === false, 'Rejected signature established management authorization');
    }
    const malformed = await api(origin, 'manage/authorize', { challengeId: 1, signature: false }, token);
    must(malformed.status === 400, 'Malformed management authorization was accepted');
    const allGrant = await authorize(30, true);
    must(reachesLogin(await request(`${origin}/cgi-bin/luci/`)), 'All-device authorization did not retain anonymous separate LuCI login');
    // Probe the gateway directly on loopback: the alternate public vhost may still
    // be the legacy route before cutover, which this smoke must never change.
    const otherHostStatus = await server.exec(`curl --silent --show-error --max-time 10 --noproxy '*' --output /dev/null --write-out '%{http_code}' -H ${quote(`Host: ${otherHost}`)} -H 'X-Forwarded-Proto: https' http://127.0.0.1:${config.gatewayPort}/cgi-bin/luci/`, 'verify all-device authorization is bound to exact host');
    must(otherHostStatus.trim() === '401', 'All-device authorization leaked onto the other gateway origin');
    const crossOrigin = await request(`${origin}/ubus`, { headers: { Connection: 'Upgrade', Upgrade: 'websocket', Origin: `https://${otherHost}`, 'Sec-WebSocket-Version': '13', 'Sec-WebSocket-Key': crypto.randomBytes(16).toString('base64') } });
    must(crossOrigin.status === 403, 'All-device authorization bypassed Origin checks');
    must((await api(origin, 'target')).status === 401, 'All-device authorization bypassed API authentication');
    const reserved = await request(`${origin}/_roc-agent/v1/status`);
    must([403, 404].includes(reserved.status), 'All-device authorization exposed a reserved agent route');
    cookie = await redeem(await browserTicket(allGrant), allGrant);
    await end(cookie);
    const shortGrant = await authorize(5, false);
    const oldCookie = await redeem(await browserTicket(shortGrant), shortGrant);
    const oldTicket = await browserTicket(shortGrant);
    const freshGrant = await authorize(15, false);
    must((await request(`${origin}/cgi-bin/luci/`, { headers: { Cookie: oldCookie } })).status === 401, 'New authorization retained the previous browser cookie');
    must([401, 409].includes((await api(origin, 'manage/redeem', { ticket: oldTicket })).status), 'New authorization retained the previous browser ticket');
    cookie = await redeem(await browserTicket(freshGrant), freshGrant);
    await server.exec(`/opt/roc-wol/bin/roc-wol devices revoke --socket /run/roc-wol/admin.sock --id ${deviceId} >/dev/null`, 'revoke temporary smoke identity');
    const denied = await request(`${origin}/cgi-bin/luci/`, { headers: { Cookie: cookie } });
    must(denied.status === 401, 'Device revocation did not invalidate management session');
    deviceId = null;
  } finally {
    if (deviceId) await server.exec(`/opt/roc-wol/bin/roc-wol devices revoke --socket /run/roc-wol/admin.sock --id ${deviceId} >/dev/null`, 'revoke temporary smoke identity after failure');
  }
}
async function closeLegacy(server, router, config) {
  const current = (await router.exec('uci -q get frpc.ssh.name || true\nuci -q get frpc.ssh.local_ip || true\nuci -q get frpc.ssh.local_port || true\nuci -q get frpc.ssh.remote_port || true', 're-read legacy section before exact deletion')).trim();
  if (current) {
    must(current.split('\n').join('|') === 'Roc|127.0.0.1|80|15201', 'Legacy section changed after staging; not deleting unrelated UCI work');
    await router.exec('uci delete frpc.ssh\nuci commit frpc', 'remove only legacy public LuCI mapping');
  }
  // Reconcile the running tunnel even after an earlier cutover stopped between
  // committing UCI and restarting frpc. Absence in UCI alone is not closure.
  await router.exec('/etc/init.d/frpc restart', 'reconcile router tunnel with committed private-only UCI');
  await retry(async () => {
    const generated = text(await router.read('/var/etc/frpc.toml'));
    must(!/remotePort\s*=\s*15201\b/.test(generated) && !/^\s*name\s*=\s*["']Roc["']/m.test(generated), 'Generated frpc still exposes legacy LuCI mapping');
    const listeners = await server.exec('ss -lntp', 'verify legacy public listener removal');
    must(listeningLines(listeners, 15201).length === 0, 'Server still listens on legacy port 15201');
    await privateStatus(server, config);
  }, 12);
  // A workstation-origin TCP attempt is independent of the server's own ss observation.
  const net = await import('node:net');
  const reachable = await new Promise(resolve => {
    const socket = net.createConnection({ host: SERVER, port: 15201 });
    socket.setTimeout(5000); socket.on('connect', () => { socket.destroy(); resolve(true); });
    socket.on('timeout', () => { socket.destroy(); resolve(false); }); socket.on('error', () => resolve(false));
  });
  must(!reachable, 'Legacy port 15201 remains reachable from this workstation');
}
async function cutover(server, router, config) {
  must(/^[a-f0-9]{32}$/.test(ARGS['device-id'] || ''), 'cutover requires --device-id of the phone paired and verified on the direct origin');
  await inspectHosts(server, router, config);
  await privateStatus(server, config);
  await directDNS(config);
  await publicGate(config, config.directHost, true);
  await publicGate(config, config.directHost);
  const devices = JSON.parse(await server.exec('/opt/roc-wol/bin/roc-wol devices list --socket /run/roc-wol/admin.sock', 'verify actual paired phone enrollment'));
  must(Array.isArray(devices) && devices.some(item => item.deviceId === ARGS['device-id'] && !item.revoked && !item.deviceName?.startsWith('deployment-check-')), 'Selected phone is not an active non-deployment enrollment');
  await protocolSmoke(server, config, config.directHost);
  // Real source certificate validation must precede Cloudflare strict mode and public routing changes.
  await request('https://roc.hubu.wiki/_wol/manage', { address: SERVER });
  await backup(server, router, config, 'cutover');
  await replaceNginx(server, [[ROC_PROXY, proxyLocation(config)], [ROC_VHOST, publicVhost('roc.hubu.wiki', config, true)]]);
  let fallbackFailure;
  try {
    config.cutoverStartedAt = new Date().toISOString();
    await saveConfig(config);
    await publicGate(config, 'roc.hubu.wiki');
    await protocolSmoke(server, config, 'roc.hubu.wiki');
  } catch (error) { fallbackFailure = error; }
  finally {
    // Once roc points at the gateway, neither edge trouble nor a local state-write failure keeps the old port alive.
    await closeLegacy(server, router, config);
  }
  await publicGate(config, config.directHost);
  const finalListeners = await server.exec('ss -lntp', 'verify final private loopback listeners');
  for (const port of [config.gatewayPort, config.visitorPort]) {
    const lines = listeningLines(finalListeners, port);
    must(lines.length === 1 && lines.every(line => line.includes(`127.0.0.1:${port}`)), `Final project listener ${port} is missing or not exclusively loopback`);
  }
  config.cutoverAt = new Date().toISOString(); config.fallbackProbeAt = fallbackFailure ? null : config.cutoverAt;
  await saveConfig(config);
  log('Legacy 15201 mapping removed, server listener absent, workstation connection rejected. Direct gateway remains authenticated; LAN LuCI is unchanged.');
  if (fallbackFailure) throw new Error(`Cutover is fail-closed and legacy port is CLOSED. Cloudflare fallback verification remains blocked: ${fallbackFailure.message}. Do not restore the old public mapping`);
  log('Both public backend gates verified. True phone/Clash/power-state/browser WebSocket acceptance and Cloudflare host-only rule readback remain operator evidence, not inferred from these probes.');
}

async function main() {
  if (ARGS.help || !ARGS.command) { log(USAGE); return; }
  must(['inspect', 'stage', 'cutover'].includes(ARGS.command), 'Unknown command; use --help');
  must(!ARGS['rotate-mtls'] || ARGS.command === 'stage', '--rotate-mtls is only valid for stage');
  must(!ARGS.phase || ['private', 'https', 'all'].includes(ARGS.phase), 'Invalid stage phase');
  await protect(PRIVATE);
  let config = await loadConfig();
  const { server, router } = await connections(config);
  try {
    let info;
    if (!config) ({ config, info } = await initialize(server, router));
    if (ARGS.command === 'inspect') {
      info ||= await inspectHosts(server, router, config);
      log(JSON.stringify({ directOrigin: `https://${config.directHost}`, fallbackOrigin: 'https://roc.hubu.wiki', ports: { gateway: config.gatewayPort, visitor: config.visitorPort, agent: config.agentPort }, pingPath: info.pingPath, legacyMappingPresent: info.oldMapping, sharedFrps: 'read-only, unchanged', prerequisites: 'DNS console ownership and Cloudflare host-only rules require user readback; see --help' }, null, 2));
    } else if (ARGS.command === 'stage') {
      const phase = ARGS.phase || 'all';
      if (phase !== 'https') await stagePrivate(server, router, config);
      if (phase !== 'private') await stageHTTPS(server, router, config);
    } else await cutover(server, router, config);
  } finally { server.close(); router.close(); }
}
main().catch(error => { console.error(`Stopped: ${error.message}`); process.exitCode = 1; });

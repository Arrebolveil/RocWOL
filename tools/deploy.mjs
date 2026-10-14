#!/usr/bin/env node
import fs from 'node:fs/promises';
import path from 'node:path';
import https from 'node:https';
import http from 'node:http';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { validateConfig, deploymentTemplate, migrateConfig, configDigest, deploymentModes, runtimeConfigs, renderFrp, renderProxy, certificatePaths, mapContainerPath, validateOwner, checkResource, checkVhosts, selectArtifact, FRP_RELEASE, sha256, requireValue as must } from './deployment-config.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const runFile = promisify(execFile);
const OWNER = '/etc/roc-wol/deployment-owner.json';
const quote = value => `'${String(value).replaceAll("'", "'\\''")}'`;
const nonce = () => crypto.randomBytes(16).toString('hex');
const json = value => Buffer.from(`${JSON.stringify(value, null, 2)}\n`);
const parse = bytes => { try { return JSON.parse(bytes); } catch { throw new Error('Invalid protected JSON; contents are not printed'); } };
const USAGE = `RocWOL self-hosted deployment (Node 24+, npm install --prefix tools)
  node tools/deploy.mjs init --config FILE
  node tools/deploy.mjs migrate-config --config FILE
  node tools/deploy.mjs inspect --config FILE
  node tools/deploy.mjs stage --config FILE
  node tools/deploy.mjs verify --config FILE
inspect is read-only. stage requires a successful inspection of the unchanged manifest.
Verify SSH fingerprints independently before filling pins. No trust-on-first-use acceptance.
verify temporarily enrolls and revokes its own identity; it never sends a wake request.
manual proxy stages the services and local snippets, then reports waiting for proxy installation.
Mode changes require manual retirement of the previous owned resources; shared FRP is never modified.
`;
function args(values) {
  const command = values.shift(); let config;
  if (command === '--help' || command === 'help') return { command: 'help' };
  while (values.length) { must(values.shift() === '--config' && !config && values[0] && !values[0].startsWith('--'), 'Unknown or incomplete argument; use --help'); config = path.resolve(values.shift()); }
  must(['init', 'migrate-config', 'inspect', 'stage', 'verify'].includes(command) && config, 'A command and --config FILE are required; use --help');
  return { command, config };
}
async function protect(directory) {
  await fs.mkdir(directory, { recursive: true, mode: 0o700 });
  if (process.platform === 'win32') {
    const script = `$ErrorActionPreference='Stop'; $p='${directory.replaceAll("'", "''")}'; $u=[Security.Principal.WindowsIdentity]::GetCurrent().User; $a=Get-Acl -LiteralPath $p; $a.SetAccessRuleProtection($true,$false); foreach($r in @($a.Access)){[void]$a.RemoveAccessRuleSpecific($r)}; foreach($s in @($u,(New-Object Security.Principal.SecurityIdentifier('S-1-5-18')))) { $a.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($s,'FullControl','ContainerInherit,ObjectInherit','None','Allow'))) }; [IO.Directory]::SetAccessControl($p,$a)`;
    await runFile('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script]);
  } else await fs.chmod(directory, 0o700);
}
async function privateWrite(filename, content, exclusive = false) {
  await protect(path.dirname(filename)); const temp = `${filename}.${nonce()}.tmp`;
  await fs.writeFile(temp, content, { flag: 'wx', mode: 0o600 });
  try { if (exclusive) { await fs.link(temp, filename); await fs.unlink(temp); } else await fs.rename(temp, filename); }
  catch (error) { await fs.unlink(temp).catch(() => {}); throw error; }
}
async function secret(filename) {
  const stat = await fs.stat(filename); must(stat.isFile() && (process.platform === 'win32' || !(stat.mode & 0o077)), 'Credential file must be a protected regular file');
  const bytes = await fs.readFile(filename); must(bytes.length > 0 && bytes.length <= 1024 * 1024, 'Credential file has invalid size'); return bytes;
}
class Remote {
  constructor(client, label) { this.client = client; this.label = label; }
  async exec(script, label = 'remote operation') {
    return new Promise((resolve, reject) => this.client.exec('sh -s', (error, stream) => {
      if (error) return reject(new Error(`${this.label}: cannot start ${label}`));
      let output = '', size = 0;
      stream.on('data', data => { size += data.length; if (size > 16 * 1024 * 1024) stream.destroy(); else output += data; }); stream.stderr.resume();
      stream.on('error', () => reject(new Error(`${this.label}: transport failure during ${label}`)));
      stream.on('close', code => code === 0 && size <= 16 * 1024 * 1024 ? resolve(output) : reject(new Error(`${this.label}: ${label} failed; inspect protected host logs`)));
      stream.end(`set -eu\numask 077\n${script}\n`);
    }));
  }
  async sftp() { return this.files ??= await new Promise((resolve, reject) => this.client.sftp((e, f) => e ? reject(new Error('SFTP unavailable')) : resolve(f))); }
  async read(filename, optional = false) {
    const sftp = await this.sftp();
    return new Promise((resolve, reject) => {
      const stream = sftp.createReadStream(filename), chunks = []; let size = 0;
      stream.on('data', b => { size += b.length; if (size > 64 * 1024 * 1024) stream.destroy(new Error('Remote file too large')); else chunks.push(b); });
      stream.on('error', e => optional && e.code === 2 ? resolve(null) : reject(new Error(`${this.label}: required remote file unavailable`)));
      stream.on('end', () => resolve(Buffer.concat(chunks)));
    });
  }
  async upload(filename, bytes, mode = 0o600, owner = 'root:root') {
    bytes = Buffer.from(bytes); const tmp = `${filename}.${nonce()}.new`;
    must(['root:root', 'roc-wol:roc-wol', 'root:roc-wol'].includes(owner), 'Invalid generated ownership');
    await this.exec(`mkdir -p ${quote(path.posix.dirname(filename))}`, 'prepare owned upload');
    const sftp = await this.sftp();
    await new Promise((resolve, reject) => { const stream = sftp.createWriteStream(tmp, { flags: 'wx', mode }); stream.on('error', () => reject(new Error('Private SFTP upload failed'))); stream.on('close', resolve); stream.end(bytes); });
    await this.exec(`test "$(sha256sum ${quote(tmp)} | cut -d ' ' -f 1)" = ${quote(sha256(bytes))}\nchmod ${mode.toString(8)} ${quote(tmp)}\nchown ${quote(owner)} ${quote(tmp)}\nmv -f ${quote(tmp)} ${quote(filename)}`, 'atomically install owned file');
  }
  close() { this.files?.end(); this.client.end(); }
}
async function connect(endpoint, label) {
  const { Client } = await import('ssh2'); const key = Object.keys(endpoint.auth)[0]; const bytes = await secret(endpoint.auth[key]);
  try {
    return await new Promise((resolve, reject) => {
      const client = new Client(); let observed;
      client.on('ready', () => resolve(new Remote(client, label)));
      client.on('error', () => { client.end(); reject(new Error(!endpoint.hostKeySHA256 && observed ? `${label} SSH SHA256 first observation: ${observed}. Verify independently and fill hostKeySHA256 before continuing.` : observed && observed !== endpoint.hostKeySHA256 ? `${label}: SSH host identity mismatch` : `${label}: SSH connection/authentication failed`)); });
      client.connect({ host: endpoint.host, port: endpoint.port, username: endpoint.user, ...(key === 'privateKeyFile' ? { privateKey: bytes } : { password: bytes.toString('utf8').replace(/\r?\n$/, '') }), readyTimeout: 20000, keepaliveInterval: 10000, keepaliveCountMax: 3, hostHash: 'sha256', hostVerifier: value => { observed = value; return !!endpoint.hostKeySHA256 && value === endpoint.hostKeySHA256; } });
    });
  } finally { bytes.fill(0); }
}
async function owner(remote, c) {
  await remote.exec(`if test -L ${quote(OWNER)}; then exit 1; fi\nif test -e ${quote(OWNER)}; then test "$(stat -c %a ${quote(OWNER)})" = 600; test "$(stat -c %u ${quote(OWNER)})" = 0; fi`, 'validate ownership record permissions');
  const bytes = await remote.read(OWNER, true); const value = bytes === null ? null : parse(bytes); validateOwner(value, c); return value;
}
async function currentHash(remote, filename) {
  return (await remote.exec(`if test -L ${quote(filename)}; then exit 1; elif test -e ${quote(filename)}; then test -f ${quote(filename)}; sha256sum ${quote(filename)} | cut -d ' ' -f 1; else printf absent; fi`, 'inspect resource hash')).trim();
}
async function preflight(remote, owned, resources) {
  for (const resource of resources) { const h = await currentHash(remote, resource.path); checkResource(owned, resource.path, h === 'absent' ? null : h); }
  // Also protect retained resources that are not being replaced this run.
  for (const resource of owned?.files ?? []) if (!resources.some(r => r.path === resource.path)) { const h = await currentHash(remote, resource.path); checkResource(owned, resource.path, h === 'absent' ? null : h); }
}
async function install(remote, c, owned, resources) {
  await preflight(remote, owned, resources);
  const records = (owned?.files ?? []).map(f => ({ ...f }));
  // A previous interrupted upload may already equal its desired hash. Preserve
  // that observed value before replacing the next desired hash.
  for (const f of records) { const h = await currentHash(remote, f.path); f.installedSHA256 = h === 'absent' ? null : h; }
  for (const r of resources) { const existing = records.find(f => f.path === r.path); if (existing) existing.desiredSHA256 = sha256(r.bytes); else records.push({ path: r.path, installedSHA256: null, desiredSHA256: sha256(r.bytes) }); }
  const record = { version: 1, deploymentId: c.deploymentId, phase: 'installing', files: records, modes: deploymentModes(c) };
  await remote.upload(OWNER, json(record));
  for (const r of resources) {
    const h = await currentHash(remote, r.path); const f = records.find(v => v.path === r.path); checkResource({ files: [f] }, r.path, h === 'absent' ? null : h);
    if (h !== f.desiredSHA256) {
      if (h !== 'absent') { const backup = `/opt/roc-wol/rollback/${nonce()}/${path.posix.basename(r.path)}`; await remote.upload(backup, await remote.read(r.path)); await remote.upload(`${backup}.sha256`, Buffer.from(`${h}\n`)); }
      await remote.upload(r.path, r.bytes, r.mode ?? 0o600, r.owner ?? 'root:root');
    }
    f.installedSHA256 = f.desiredSHA256; await remote.upload(OWNER, json(record));
  }
  return record;
}
async function ready(remote, record) { record.phase = 'ready'; await remote.upload(OWNER, json(record)); }
function proxyCommand(c, operation) { return c.proxy.mode === 'nginx' ? `nginx ${operation}` : `docker exec ${quote(c.proxy.container)} openresty ${operation}`; }
async function topology(remote, c) {
  let source = await remote.exec(c.proxy.mode === 'nginx' ? 'nginx -T 2>&1' : `docker exec ${quote(c.proxy.container)} openresty -T 2>&1`, 'inspect proxy topology');
  if (c.proxy.mode === 'onepanel') {
    const info = parse(await remote.exec(`docker inspect ${quote(c.proxy.container)}`, 'resolve proxy configuration mounts'));
    must(info.length === 1 && info[0].State.Running, 'Configured proxy container is not running');
    const mounts = (info[0].Mounts ?? []).filter(m => ['bind', 'volume'].includes(m.Type)).sort((a, b) => b.Destination.length - a.Destination.length);
    source = source.replace(/^# configuration file (.+):\s*$/gm, (line, filename) => {
      const mount = mounts.find(m => filename.startsWith(`${m.Destination.replace(/\/$/, '')}/`));
      return mount ? `# configuration file ${path.posix.join(mount.Source, path.posix.relative(mount.Destination, filename))}:` : line;
    });
    must(source.includes(c.proxy.configDir), 'Configured configDir is not mounted and included by OpenResty');
  }
  return source;
}
async function inspectMount(remote, c, hostRoot, containerRoot) {
  const info = parse(await remote.exec(`docker inspect ${quote(c.proxy.container)}`, 'inspect configured proxy container'));
  must(info.length === 1 && info[0].State.Running, 'Configured proxy container is not running');
  const mounts = info[0].Mounts ?? [];
  const found = mounts.some(m => { if (!['bind', 'volume'].includes(m.Type)) return false; const relative = path.posix.relative(m.Source, hostRoot); return relative !== '..' && !relative.startsWith('../') && path.posix.resolve(m.Destination, relative) === containerRoot; });
  must(found, 'Host/container mapping does not match docker inspect mounts');
}
async function certificateCheck(remote, c) {
  const files = certificatePaths(c, false);
  await remote.exec(`test -s ${quote(files.cert)}\ntest -s ${quote(files.key)}\nopenssl x509 -in ${quote(files.cert)} -noout -checkend 0 >/dev/null\ntest "$(openssl x509 -in ${quote(files.cert)} -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum)" = "$(openssl pkey -in ${quote(files.key)} -pubout -outform DER | sha256sum)"\nopenssl verify -CApath /etc/ssl/certs -untrusted ${quote(files.cert)} ${quote(files.cert)} >/dev/null\n${c.publicOrigins.map(origin => { const hostname = new URL(origin).hostname.replace(/^\[|\]$/g, ''); return `openssl x509 -in ${quote(files.cert)} -noout ${hostname.includes(':') || /^\d+\./.test(hostname) ? '-checkip' : '-checkhost'} ${quote(hostname)} >/dev/null`; }).join('\n')}`, 'validate public certificate SAN, trust and private key');
  if (c.proxy.mode === 'onepanel') {
    await inspectMount(remote, c, c.proxy.tlsDirHost, c.proxy.tlsDirContainer);
    const mapped = certificatePaths(c, true);
    for (const key of ['cert', 'key']) {
      // Resolving inside the container proves every link target exists in its namespace.
      const source = await remote.exec(`sha256sum ${quote(files[key])} | cut -d ' ' -f 1`, 'read host certificate digest');
      const dest = await remote.exec(`docker exec ${quote(c.proxy.container)} sh -c ${quote('p=$(readlink -f "$1"); test -n "$p"; test -f "$p"; sha256sum "$p" | cut -d " " -f 1')} sh ${quote(mapped[key])}`, 'validate container certificate link target and digest');
      must(source.trim() === dest.trim(), 'Container certificate mapping differs from host');
    }
  }
}
async function portCheck(remote, c, owned, ports) {
  for (const [port, executable] of ports) {
    const listing = await remote.exec('if command -v ss >/dev/null; then ss -lntp; else netstat -lntp; fi', 'inspect listening ports');
    const lines = listing.split('\n').filter(line => new RegExp(`(?:\\]|[0-9.*]):${port}\\s`).test(line));
    if (!lines.length) continue;
    must(owned?.files.some(f => f.path === executable), 'Configured port is occupied without project ownership');
    for (const line of lines) {
      must(line.includes(`127.0.0.1:${port}`), 'Configured port has a non-loopback listener');
      const pid = line.match(/pid=(\d+)/)?.[1] ?? line.match(/\s(\d+)\/[\w.-]+/)?.[1]; must(pid, 'Cannot establish listening process ownership');
      const exe = (await remote.exec(`readlink -f /proc/${pid}/exe`, 'check listener executable')).trim(); must(exe === executable, 'Port belongs to an unrelated executable');
      const h = await currentHash(remote, exe); checkResource(owned, exe, h === 'absent' ? null : h);
    }
  }
}
async function inspectHosts(server, router, c) {
  const serverOwner = await owner(server, c), routerOwner = await owner(router, c);
  await server.exec('test "$(uname -s)" = Linux\nfor cmd in openssl curl systemctl sha256sum tar getent useradd flock stat readlink; do command -v "$cmd" >/dev/null; done\ntest -d /run/systemd/system', 'check server dependencies');
  await router.exec(`test "$(uname -s)" = Linux\ntest -f /etc/rc.common\nfor cmd in sha256sum stat readlink netstat; do command -v "$cmd" >/dev/null; done\ntest -x ${quote(c.pingPath)}\ntest -x ${quote(c.etherwakePath)}\n${c.targets.map(t => `test -d ${quote(`/sys/class/net/${t.interface}`)}`).join('\n')}`, 'check router dependencies and target interfaces');
  const machines = { server: (await server.exec('uname -m', 'read server architecture')).trim(), router: (await router.exec('uname -m', 'read router architecture')).trim() };
  const manifest = parse(await fs.readFile(path.join(ROOT, 'dist/build-manifest.json')));
  selectArtifact(manifest, 'gateway', machines.server); selectArtifact(manifest, 'agent', machines.router);
  await portCheck(server, c, serverOwner, [[c.gatewayPort, '/opt/roc-wol/bin/roc-wol'], ...(c.transport.mode === 'frp' ? [[c.visitorPort, '/opt/roc-wol/bin/frpc']] : [])]);
  await portCheck(router, c, routerOwner, [[c.agentPort, '/usr/sbin/roc-wol-agent']]);
  if (c.transport.mode === 'external') {
    const result = await server.exec(`curl --silent --max-time 5 --insecure --output /dev/null --write-out '%{http_code}' https://127.0.0.1:${c.visitorPort}/_roc-agent/v2/targets || true`, 'inspect supplied private forwarding availability');
    // mTLS normally rejects this probe; a missing TCP forward instead returns 000.
    must(result.trim() !== '000' || !routerOwner, 'External private forwarding is unavailable');
    const listing = await server.exec('ss -lntp', 'inspect supplied private listener');
    must(listing.split('\n').some(line => line.includes(`127.0.0.1:${c.visitorPort}`)) && !listing.split('\n').some(line => /(?:0\.0\.0\.0|\*|\[::\]):/.test(line) && line.includes(`:${c.visitorPort}`)), 'External forwarding must listen only on loopback at visitorPort');
  }
  if (c.proxy.mode !== 'manual') {
    await server.exec(c.proxy.mode === 'nginx' ? 'command -v nginx >/dev/null' : 'command -v docker >/dev/null', 'check proxy prerequisite');
    checkVhosts(await topology(server, c), c, serverOwner);
    if (c.proxy.mode === 'onepanel') {
      await inspectMount(server, c, c.proxy.tlsDirHost, c.proxy.tlsDirContainer);
      if (c.tls.mode === 'acme-http01') await inspectMount(server, c, c.proxy.webrootHost, c.proxy.webrootContainer);
    }
  }
  if (c.tls.mode === 'external') await certificateCheck(server, c);
  else await server.exec('command -v certbot >/dev/null', 'check host certbot prerequisite');
  if (c.transport.mode === 'frp') {
    const bytes = await secret(c.transport.tokenFile); bytes.fill(0);
    await router.exec(`test -x ${quote(c.transport.routerFrpcPath)}\n${quote(c.transport.routerFrpcPath)} verify --help >/dev/null`, 'check existing router frpc verification support');
  }
  for (const [remote, owned] of [[server, serverOwner], [router, routerOwner]]) await preflight(remote, owned, []);
  return { machines, serverOwner, routerOwner, manifest };
}
async function request(url, { method = 'GET', headers = {}, body } = {}) {
  const u = new URL(url); must(['https:', 'http:'].includes(u.protocol), 'Unsupported probe protocol');
  return new Promise((resolve, reject) => {
    const req = (u.protocol === 'https:' ? https : http).request(u, { method, headers, timeout: 15000 }, res => {
      const chunks = []; let size = 0; res.on('data', b => { size += b.length; if (size > 64 * 1024) req.destroy(); else chunks.push(b); });
      res.on('end', () => size <= 64 * 1024 ? resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString('utf8') }) : reject(new Error('Probe response exceeds 64 KiB')));
      res.on('error', () => reject(new Error('Probe response failed')));
    });
    req.on('timeout', () => req.destroy()); req.on('error', () => reject(new Error('TLS/network probe failed; no insecure public TLS fallback')));
    if (body !== undefined) req.write(JSON.stringify(body)); req.end();
  });
}
async function download(url) {
  const allowed = new Set(['github.com', 'release-assets.githubusercontent.com', 'objects.githubusercontent.com']);
  for (let n = 0; n < 6; n++) {
    const u = new URL(url); must(u.protocol === 'https:' && allowed.has(u.hostname), 'Release download escaped supplier allowlist');
    const response = await fetch(u, { redirect: 'manual', signal: AbortSignal.timeout(180000) });
    if ([301, 302, 303, 307, 308].includes(response.status)) { url = new URL(response.headers.get('location'), u).href; continue; }
    must(response.ok, 'Official FRP download failed'); return Buffer.from(await response.arrayBuffer());
  }
  throw new Error('Official download redirect limit exceeded');
}
async function installFrpc(machine, directory) {
  const arch = { x86_64: 'amd64', aarch64: 'arm64', arm64: 'arm64' }[machine]; must(arch, 'Unsupported FRP platform');
  const name = `frp_${FRP_RELEASE.version}_linux_${arch}`, base = `https://github.com/fatedier/frp/releases/download/v${FRP_RELEASE.version}/`;
  const sums = (await download(`${base}frp_sha256_checksums.txt`)).toString();
  must(sums.split('\n').some(line => line.trim() === `${FRP_RELEASE.checksums[arch]}  ${name}.tar.gz`), 'Official FRP checksum manifest differs from pinned release');
  const tarball = await download(`${base}${name}.tar.gz`); must(sha256(tarball) === FRP_RELEASE.checksums[arch], 'FRP archive checksum mismatch');
  const archive = path.join(directory, `${name}.tar.gz`); await privateWrite(archive, tarball);
  await runFile('tar', ['-xzf', archive, '-C', directory, `${name}/frpc`]);
  const bytes = await fs.readFile(path.join(directory, name, 'frpc')); await fs.rm(path.join(directory, name), { recursive: true }); await fs.unlink(archive); return bytes;
}
async function createPKI(directory, state, save) {
  if (state.pki) return Object.fromEntries(Object.entries(state.pki).map(([key, value]) => [key, Buffer.from(value, 'base64')]));
  const work = path.join(directory, `pki-${nonce()}`); await protect(work);
  try {
    const cmd = argv => runFile('openssl', argv, { cwd: work });
    await cmd(['ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', 'ca.key']);
    await cmd(['req', '-new', '-x509', '-sha256', '-days', '3650', '-key', 'ca.key', '-subj', '/CN=Roc-WOL-Private-CA', '-addext', 'basicConstraints=critical,CA:TRUE,pathlen:0', '-addext', 'keyUsage=critical,keyCertSign,cRLSign', '-out', 'ca.pem']);
    for (const [name, ext] of [['client', 'subjectAltName=URI:urn:roc-wol:gateway\nextendedKeyUsage=clientAuth'], ['server', 'subjectAltName=DNS:router.roc-wol.internal\nextendedKeyUsage=serverAuth']]) {
      await privateWrite(path.join(work, `${name}.ext`), `basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\n${ext}\n`);
      await cmd(['ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', `${name}.key`]);
      await cmd(['req', '-new', '-sha256', '-key', `${name}.key`, '-subj', `/CN=roc-wol-${name}`, '-out', `${name}.csr`]);
      await cmd(['x509', '-req', '-sha256', '-days', '1095', '-set_serial', `0x${nonce()}`, '-in', `${name}.csr`, '-CA', 'ca.pem', '-CAkey', 'ca.key', '-extfile', `${name}.ext`, '-out', `${name}.pem`]);
    }
    state.pki = {}; for (const filename of ['ca.key', 'ca.pem', 'client.key', 'client.pem', 'server.key', 'server.pem']) state.pki[filename] = (await fs.readFile(path.join(work, filename))).toString('base64');
    await save(); return Object.fromEntries(Object.entries(state.pki).map(([key, value]) => [key, Buffer.from(value, 'base64')]));
  } finally { await fs.rm(work, { recursive: true, force: true }); }
}
async function buildResources(c, info, directory, state, save) {
  const runtime = runtimeConfigs(c), pki = await createPKI(directory, state, save), server = [], router = [];
  const add = (list, filename, bytes, mode = 0o600, owner = 'root:root') => list.push({ path: filename, bytes: Buffer.from(bytes), mode, owner });
  for (const [role, side, filename] of [['gateway', 'server', '/opt/roc-wol/bin/roc-wol'], ['agent', 'router', '/usr/sbin/roc-wol-agent']]) {
    const artifact = selectArtifact(info.manifest, role, info.machines[side]), bytes = await fs.readFile(path.join(ROOT, 'dist', artifact)); must(sha256(bytes) === info.manifest.sha256[artifact], 'Artifact checksum mismatch'); add(side === 'server' ? server : router, filename, bytes, 0o755);
  }
  add(server, '/etc/roc-wol/gateway.json', json(runtime.gateway), 0o600, 'roc-wol:roc-wol'); add(router, '/etc/roc-wol/agent.json', json(runtime.agent));
  for (const filename of ['ca.pem', 'client.pem', 'client.key']) add(server, `/etc/roc-wol/pki/${filename}`, pki[filename], 0o600, 'roc-wol:roc-wol');
  for (const filename of ['ca.pem', 'server.pem', 'server.key']) add(router, `/etc/roc-wol/pki/${filename}`, pki[filename]);
  add(server, '/etc/systemd/system/roc-wol.service', await fs.readFile(path.join(ROOT, 'deploy/server/roc-wol.service')), 0o644);
  add(router, '/etc/init.d/roc-wol-agent', await fs.readFile(path.join(ROOT, 'deploy/router/roc-wol-agent')), 0o755);
  if (c.transport.mode === 'frp') {
    const tokenBytes = await secret(c.transport.tokenFile); let frp;
    try { const token = tokenBytes.toString('utf8').replace(/\r?\n$/, ''); must(token && !/[\x00-\x1f\x7f]/.test(token), 'FRP token file contains an invalid token'); frp = renderFrp(c, state.stcpKey, token); } finally { tokenBytes.fill(0); }
    add(server, '/opt/roc-wol/bin/frpc', await installFrpc(info.machines.server, directory), 0o755);
    add(server, '/opt/roc-wol/frpc.toml', frp.visitor, 0o600, 'roc-wol:roc-wol'); add(router, '/etc/roc-wol/frpc.toml', frp.router);
    add(server, '/etc/systemd/system/roc-wol-visitor.service', await fs.readFile(path.join(ROOT, 'deploy/server/roc-wol-visitor.service')), 0o644);
    const tunnel = (await fs.readFile(path.join(ROOT, 'deploy/router/roc-wol-tunnel'), 'utf8')).replace('@FRPC_PATH@', quote(c.transport.routerFrpcPath)); add(router, '/etc/init.d/roc-wol-tunnel', tunnel, 0o755);
  }
  if (c.tls.mode === 'acme-http01') {
    const env = { DOMAINS: c.publicOrigins.map(o => new URL(o).hostname).join(' '), WEBROOT_HOST: c.proxy.webrootHost, TLS_DEST_HOST: `${c.proxy.tlsDirHost}/${c.deploymentId}`, PROXY_MODE: c.proxy.mode, ...(c.proxy.mode === 'onepanel' ? { OPENRESTY_CONTAINER: c.proxy.container } : {}) };
    add(server, '/etc/roc-wol/renew.env', Object.entries(env).map(([key, value]) => `${key}=${quote(value)}\n`).join(''));
    for (const name of ['roc-wol-renew.service', 'roc-wol-renew.timer']) add(server, `/etc/systemd/system/${name}`, await fs.readFile(path.join(ROOT, 'deploy/server', name)), 0o644);
    add(server, '/opt/roc-wol/bin/renew-certificates.sh', await fs.readFile(path.join(ROOT, 'deploy/server/renew-certificates.sh')), 0o700);
  }
  return { server, router };
}
async function privateStatus(server, c) {
  const url = `https://router.roc-wol.internal:${c.visitorPort}`;
  const curl = `curl --silent --show-error --fail --max-time 10 --noproxy '*' --resolve router.roc-wol.internal:${c.visitorPort}:127.0.0.1 --cacert /etc/roc-wol/pki/ca.pem --cert /etc/roc-wol/pki/client.pem --key /etc/roc-wol/pki/client.key`;
  const list = parse(await server.exec(`${curl} ${quote(`${url}/_roc-agent/v2/targets`)}`, 'check authenticated agent inventory'));
  must(Array.isArray(list.targets) && list.targets.length === c.targets.length && list.managementEnabled === !!c.luciOrigin, 'Agent inventory/capability differs from deployment');
  for (const target of c.targets) {
    const item = list.targets.find(t => t.targetId === target.id); must(item && item.name === target.name && item.mac.toLowerCase() === target.mac && item.ip === target.ip, 'Agent target differs from deployment');
    const status = parse(await server.exec(`${curl} ${quote(`${url}/_roc-agent/v2/targets/${target.id}/status`)}`, 'check read-only per-target status')); must(status.targetId === target.id && ['reachable', 'unreachable', 'unknown'].includes(status.reachability), 'Invalid agent target status');
  }
  await server.exec(`if curl --silent --max-time 5 --noproxy '*' --resolve router.roc-wol.internal:${c.visitorPort}:127.0.0.1 --cacert /etc/roc-wol/pki/ca.pem ${quote(`${url}/_roc-agent/v2/targets`)} >/dev/null 2>&1; then exit 1; fi\nif curl --silent --max-time 5 --noproxy '*' --cacert /etc/roc-wol/pki/ca.pem --cert /etc/roc-wol/pki/client.pem --key /etc/roc-wol/pki/client.key https://127.0.0.1:${c.visitorPort}/_roc-agent/v2/targets >/dev/null 2>&1; then exit 1; fi\nif curl --silent --max-time 5 --noproxy '*' --resolve router.roc-wol.internal:${c.visitorPort}:127.0.0.1 --cacert /etc/ssl/certs/ca-certificates.crt --cert /etc/roc-wol/pki/client.pem --key /etc/roc-wol/pki/client.key ${quote(`${url}/_roc-agent/v2/targets`)} >/dev/null 2>&1; then exit 1; fi`, 'check missing client, wrong name and wrong CA rejection');
}
async function publicGate(c) {
  for (const origin of c.publicOrigins) {
    const info = await request(`${origin}/_wol/v2/info`); const value = parse(info.body); must(info.status === 200 && value.serviceId === 'roc-wol' && value.apiVersion === 2 && /^[a-f0-9]{32}$/.test(value.instanceId), 'Public v2 info gate failed');
    for (const route of ['/_wol/v2/targets', '/cgi-bin/luci/', '/']) must((await request(origin + route)).status === 401, 'Public unauthenticated gate failed');
  }
}
async function stageHTTPS(server, c, record, directory) {
  const rendered = renderProxy(c);
  if (c.proxy.mode === 'manual') {
    for (const [name, content] of Object.entries(rendered)) await privateWrite(path.join(directory, 'rendered', name), content);
    try { await publicGate(c); } catch { throw new Error('Services installed; waiting for manual reverse proxy installation and a passing HTTPS gate. Local snippets are in the deployment rendered directory.'); }
    return record;
  }
  // Retire removed origins (including an old configDir) without leaving active
  // listeners behind. Keep the owned tombstone and rollback hash for recovery.
  const desiredPaths = new Set(Object.keys(rendered).map(name => `${c.proxy.configDir}/${name}`));
  const retired = record.files.filter(f => /^[a-f0-9]{16}\.roc-wol\.conf$/.test(path.posix.basename(f.path)) && !desiredPaths.has(f.path));
  if (retired.length) record = await install(server, c, record, retired.map(f => ({ path: f.path, bytes: Buffer.from(`# Retired roc-wol origin; deployment ${c.deploymentId}\n`), mode: 0o644 })));
  if (c.tls.mode === 'acme-http01') {
    const bootstrap = Object.entries(renderProxy(c, true)).map(([name, bytes]) => ({ path: `${c.proxy.configDir}/${name}`, bytes: Buffer.from(bytes), mode: 0o644 }));
    record = await install(server, c, record, bootstrap);
    await server.exec(`${proxyCommand(c, '-t')} >/dev/null 2>&1\n${proxyCommand(c, '-s reload')}`, 'activate restricted HTTP-01 vhosts');
    await server.exec(`mkdir -p ${quote(`${c.proxy.webrootHost}/.well-known/acme-challenge`)}\nchmod 755 ${quote(c.proxy.webrootHost)} ${quote(`${c.proxy.webrootHost}/.well-known`)} ${quote(`${c.proxy.webrootHost}/.well-known/acme-challenge`)}`, 'prepare challenge-only webroot');
    const probe = nonce(), content = nonce(), filename = `${c.proxy.webrootHost}/.well-known/acme-challenge/${probe}`;
    await server.upload(filename, Buffer.from(content), 0o644);
    try { for (const origin of c.publicOrigins) { const response = await request(`http://${new URL(origin).hostname}/.well-known/acme-challenge/${probe}`); must(response.status === 200 && response.body === content, 'HTTP-01 webroot is not publicly reachable for every domain'); } }
    finally { await server.exec(`rm -f ${quote(filename)}`, 'remove temporary ACME reachability probe'); }
    const domains = c.publicOrigins.map(o => `-d ${quote(new URL(o).hostname)}`).join(' '), common = `certbot certonly --webroot -w ${quote(c.proxy.webrootHost)} --cert-name roc-wol --email ${quote(c.tls.email)} --agree-tos --non-interactive ${domains}`;
    await server.exec(`${common} --staging --config-dir /opt/roc-wol/acme/staging --work-dir /opt/roc-wol/acme/staging-work --logs-dir /opt/roc-wol/acme/staging-logs\n${common} --config-dir /opt/roc-wol/acme/etc --work-dir /opt/roc-wol/acme/work --logs-dir /opt/roc-wol/acme/logs`, 'issue staging then production SAN certificate');
    await server.exec('/opt/roc-wol/bin/renew-certificates.sh --install-only --no-reload', 'atomically install initial SAN certificate');
    await certificateCheck(server, c);
  }
  const resources = Object.entries(rendered).map(([name, bytes]) => ({ path: `${c.proxy.configDir}/${name}`, bytes: Buffer.from(bytes), mode: 0o644 }));
  const previous = []; for (const resource of resources) previous.push({ ...resource, bytes: await server.read(resource.path, true) });
  record = await install(server, c, record, resources);
  try { await server.exec(`${proxyCommand(c, '-t')} >/dev/null 2>&1\n${proxyCommand(c, '-s reload')}`, 'validate and reload configured proxy'); }
  catch (error) {
    for (const resource of previous) {
      const entry = record.files.find(f => f.path === resource.path);
      if (resource.bytes === null) { await server.exec(`rm -f ${quote(resource.path)}`, 'roll back newly installed vhost'); entry.installedSHA256 = null; }
      else { await server.upload(resource.path, resource.bytes, 0o644); entry.installedSHA256 = sha256(resource.bytes); }
    }
    await server.upload(OWNER, json(record)); await server.exec(`${proxyCommand(c, '-t')} >/dev/null 2>&1\n${proxyCommand(c, '-s reload')}`, 'reload rolled-back proxy').catch(() => {}); throw error;
  }
  if (c.tls.mode === 'acme-http01') await server.exec('systemctl daemon-reload\nsystemctl enable --now roc-wol-renew.timer', 'enable project-only certificate renewal');
  await publicGate(c); return record;
}
async function stage(server, router, c, info, directory) {
  const filename = path.join(directory, 'state.json'); let state;
  try { state = parse(await fs.readFile(filename)); } catch (e) { if (e.code !== 'ENOENT') throw e; }
  state ??= { version: 1, deploymentId: c.deploymentId, stcpKey: crypto.randomBytes(32).toString('base64url'), resources: {} };
  must(state.version === 1 && state.deploymentId === c.deploymentId && /^[A-Za-z0-9_-]{43}$/.test(state.stcpKey), 'Invalid local deployment state');
  const save = () => privateWrite(filename, json(state)); await save();
  const resources = await buildResources(c, info, directory, state, save);
  const proxyResources = c.proxy.mode === 'manual' ? [] : Object.entries(renderProxy(c, c.tls.mode === 'acme-http01')).map(([name, bytes]) => ({ path: `${c.proxy.configDir}/${name}`, bytes: Buffer.from(bytes) }));
  state.resources = { server: [...resources.server, ...proxyResources].map(r => ({ path: r.path, sha256: sha256(r.bytes) })), router: resources.router.map(r => ({ path: r.path, sha256: sha256(r.bytes) })) }; await save();
  // Both hosts, including future proxy paths, pass conflict checks before any remote write.
  await preflight(server, info.serverOwner, [...resources.server, ...proxyResources]); await preflight(router, info.routerOwner, resources.router);
  for (const [remote, record] of [[server, info.serverOwner], [router, info.routerOwner]]) {
    for (const f of record?.files ?? []) { const h = await currentHash(remote, f.path); f.installedSHA256 = h === 'absent' ? null : h; }
  }
  const installing = (old, items) => ({ version: 1, deploymentId: c.deploymentId, phase: 'installing', modes: deploymentModes(c), files: [...(old?.files ?? []).map(f => ({ ...f, desiredSHA256: sha256(items.find(r => r.path === f.path)?.bytes ?? Buffer.alloc(0)) || f.desiredSHA256 })), ...items.filter(r => !old?.files.some(f => f.path === r.path)).map(r => ({ path: r.path, installedSHA256: null, desiredSHA256: sha256(r.bytes) }))] });
  // Keep previous desired hashes for resources outside this phase.
  const startRecord = (old, items) => { const out = installing(old, items); for (const f of out.files) if (!items.some(r => r.path === f.path)) f.desiredSHA256 = old.files.find(r => r.path === f.path).desiredSHA256; return out; };
  let serverRecord = startRecord(info.serverOwner, [...resources.server, ...proxyResources]), routerRecord = startRecord(info.routerOwner, resources.router);
  await server.upload(OWNER, json(serverRecord)); await router.upload(OWNER, json(routerRecord));
  await server.exec('getent passwd roc-wol >/dev/null || useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin roc-wol\nmkdir -p /opt/roc-wol/bin /etc/roc-wol/pki /var/lib/roc-wol\nchmod 755 /opt/roc-wol /opt/roc-wol/bin\nchown root:roc-wol /etc/roc-wol /etc/roc-wol/pki\nchmod 750 /etc/roc-wol /etc/roc-wol/pki\nchown roc-wol:roc-wol /var/lib/roc-wol\nchmod 700 /var/lib/roc-wol', 'prepare isolated gateway account and data directory');
  await router.exec('mkdir -p /etc/roc-wol/pki /var/lib/roc-wol\nchmod 700 /etc/roc-wol /etc/roc-wol/pki /var/lib/roc-wol', 'prepare persistent agent data directory');
  serverRecord = await install(server, c, serverRecord, resources.server); routerRecord = await install(router, c, routerRecord, resources.router);
  if (c.transport.mode === 'frp') {
    await server.exec('/opt/roc-wol/bin/frpc verify -c /opt/roc-wol/frpc.toml >/dev/null', 'verify dedicated FRP visitor TOML');
    await router.exec(`${quote(c.transport.routerFrpcPath)} verify -c /etc/roc-wol/frpc.toml >/dev/null\n/etc/init.d/roc-wol-tunnel enable\n/etc/init.d/roc-wol-tunnel restart`, 'verify and start independent router STCP');
    await server.exec('systemctl daemon-reload\nsystemctl enable roc-wol-visitor.service\nsystemctl restart roc-wol-visitor.service', 'start independent server STCP');
  }
  await router.exec('/etc/init.d/roc-wol-agent enable\n/etc/init.d/roc-wol-agent restart', 'restart project agent');
  await server.exec('systemctl daemon-reload\nsystemctl enable roc-wol.service\nsystemctl restart roc-wol.service', 'restart project gateway');
  let last; for (let n = 0; n < 10; n++) { try { await privateStatus(server, c); last = null; break; } catch (e) { last = e; await new Promise(resolve => setTimeout(resolve, 1500)); } } if (last) throw last;
  serverRecord = await stageHTTPS(server, c, serverRecord, directory); await ready(router, routerRecord); await ready(server, serverRecord);
  state.stagedAt = new Date().toISOString(); state.configDigest = configDigest(c); await save();
  console.log('Deployment ready: private mTLS inventory and public read-only HTTPS gates passed. Run verify explicitly for temporary enrollment checks.');
}
async function api(origin, route, payload, token) {
  const response = await request(`${origin}/_wol/v2/${route}`, { method: payload === undefined ? 'GET' : 'POST', headers: { Origin: origin, ...(payload === undefined ? {} : { 'Content-Type': 'application/json' }), ...(token ? { Authorization: `Bearer ${token}` } : {}) }, body: payload });
  return { ...response, body: parse(response.body) };
}
const sign = (key, message) => crypto.sign('sha256', Buffer.from(message), key).toString('base64url');
async function protocolSmoke(server, c, origin) {
  const wake = crypto.generateKeyPairSync('ec', { namedCurve: 'prime256v1' }), manage = crypto.generateKeyPairSync('ec', { namedCurve: 'prime256v1' });
  const wakeDER = wake.publicKey.export({ type: 'spki', format: 'der' }), manageDER = manage.publicKey.export({ type: 'spki', format: 'der' }); let deviceId;
  const cli = '/opt/roc-wol/bin/roc-wol';
  try {
    const devices = parse(await server.exec(`${cli} devices list --socket /run/roc-wol/admin.sock`, 'check temporary enrollment capacity')); must(Array.isArray(devices) && devices.filter(d => !d.revoked).length < c.maxDevices, 'Verification not passed: no free device slot; existing devices are never revoked');
    const code = (await server.exec(`${cli} pair-code --socket /run/roc-wol/admin.sock --ttl 2m`, 'create temporary enrollment code')).trim();
    const challenge = await api(origin, 'pair/challenge', { code, deviceName: `deployment-check-${nonce().slice(0, 8)}`, wakePublicKey: wakeDER.toString('base64url'), managePublicKey: manageDER.toString('base64url') }); must(challenge.status === 200, 'Temporary pair challenge failed');
    const proof = ['ROCWOL-PAIR-V1', 'roc-wol', challenge.body.challengeId, challenge.body.nonce, sha256(wakeDER), sha256(manageDER)].join('\n');
    const paired = await api(origin, 'pair/complete', { challengeId: challenge.body.challengeId, wakeSignature: sign(wake.privateKey, proof), manageSignature: sign(manage.privateKey, proof) });
    if (/^[a-f0-9]{32}$/.test(paired.body.deviceId ?? '')) deviceId = paired.body.deviceId;
    must(paired.status >= 200 && paired.status < 300 && deviceId, 'Temporary pairing failed');
    const auth = await api(origin, 'auth/challenge', { deviceId }); must(auth.status === 200, 'Wake authentication challenge failed');
    const tokenReply = await api(origin, 'auth/complete', { deviceId, challengeId: auth.body.challengeId, signature: sign(wake.privateKey, ['ROCWOL-WAKE-AUTH-V1', 'roc-wol', deviceId, auth.body.challengeId, auth.body.nonce].join('\n')) }); must(tokenReply.status === 200, 'Wake authentication failed'); const token = tokenReply.body.accessToken;
    const targets = await api(origin, 'targets', undefined, token); must(targets.status === 200 && targets.body.targets.length === c.targets.length && targets.body.managementEnabled === !!c.luciOrigin, 'Public inventory contract failed');
    for (const target of c.targets) { const reply = await api(origin, `targets/${target.id}`, undefined, token); must(reply.status === 200 && reply.body.targetId === target.id && reply.body.router === 'available', 'Public target status failed'); }
    const denied = await api(origin, 'devices/challenge', { origin, action: 'list', targetDeviceId: '' }, token); must(denied.status === 403, 'Default temporary device unexpectedly has administration capability');
    if (!c.luciOrigin) {
      for (const [route, payload] of [['manage/status', undefined], ['manage/challenge', { origin, sessionMinutes: 10, allowAllDevices: false }], ['manage/authorize', {}], ['manage/browser', {}], ['manage/redeem', { ticket: nonce() }]]) { const reply = await api(origin, route, payload, token); must(reply.status === 403 && reply.body.code === 'management_disabled', 'Disabled management API did not reject access'); }
      const ended = await api(origin, 'manage/end', {}, token); must(ended.status === 200 && ended.body.ended, 'Disabled management cannot end existing authorization');
    } else {
      const challenge = await api(origin, 'manage/challenge', { origin, sessionMinutes: 10, allowAllDevices: false }, token); must(challenge.status === 200, 'Management challenge failed');
      const canonical = selectedOrigin => ['ROCWOL-MANAGE-V2', 'roc-wol', deviceId, challenge.body.challengeId, challenge.body.nonce, selectedOrigin, '10', 'browser-only'].join('\n');
      const granted = await api(origin, 'manage/authorize', { challengeId: challenge.body.challengeId, signature: sign(manage.privateKey, canonical(origin)) }, token); must(granted.status === 200 && granted.body.active && granted.body.origin === origin, 'Management authorization failed');
      const ticket = await api(origin, 'manage/browser', {}, token); must(ticket.status === 200, 'Browser ticket failed'); const url = new URL(ticket.body.url); must(url.origin === origin && url.pathname === '/_wol/manage' && !url.search, 'Browser ticket origin mismatch'); const opaque = new URLSearchParams(url.hash.slice(1)).get('t');
      const otherOrigin = c.publicOrigins.find(o => o !== origin) ?? 'https://scope-test.example';
      const wrongOrigin = await request(`${origin}/_wol/v2/manage/redeem`, { method: 'POST', headers: { Origin: otherOrigin, 'Content-Type': 'application/json' }, body: { ticket: opaque } }); must(wrongOrigin.status === 403, 'Ticket redemption bypassed origin binding');
      const redeemed = await api(origin, 'manage/redeem', { ticket: opaque }); must(redeemed.status === 200 && redeemed.body.redirect === '/cgi-bin/luci/', 'Browser ticket redemption failed');
      const cookie = redeemed.headers['set-cookie']?.find(v => v.startsWith('__Host-roc_session=')); must(cookie && /;\s*Secure/i.test(cookie) && /;\s*HttpOnly/i.test(cookie) && /;\s*SameSite=Strict/i.test(cookie) && !/;\s*Domain=/i.test(cookie), 'Browser cookie constraints failed');
      const panel = await request(`${origin}/cgi-bin/luci/`, { headers: { Cookie: cookie.split(';')[0] } }); must([200, 403].includes(panel.status) && /luci_username/.test(panel.body) && /luci_password/.test(panel.body), 'Authorized browser did not reach separate LuCI login');
      const replay = await api(origin, 'manage/redeem', { ticket: opaque }); must([401, 409].includes(replay.status), 'Browser ticket replay accepted');
      const end = await api(origin, 'manage/end', {}, token); must(end.status === 200 && end.body.ended, 'Management termination failed');
      must((await request(`${origin}/cgi-bin/luci/`, { headers: { Cookie: cookie.split(';')[0] } })).status === 401, 'Ended cookie still reaches proxy');
    }
    await server.exec(`${cli} devices admin --socket /run/roc-wol/admin.sock --id ${deviceId} --enabled=true >/dev/null`, 'grant only temporary device administration capability');
    async function deviceProof(action, targetDeviceId = '') {
      const challenge = await api(origin, 'devices/challenge', { origin, action, targetDeviceId }, token); must(challenge.status === 200, 'Device administration challenge failed');
      const signature = sign(manage.privateKey, ['ROCWOL-DEVICE-ADMIN-V1', 'roc-wol', deviceId, challenge.body.challengeId, challenge.body.nonce, origin, action, targetDeviceId].join('\n'));
      return { challengeId: challenge.body.challengeId, signature, ...(action === 'revoke' ? { targetDeviceId } : {}) };
    }
    const listing = await api(origin, 'devices/list', await deviceProof('list'), token); must(listing.status === 200 && listing.body.capacity === c.maxDevices && listing.body.devices.some(d => d.deviceId === deviceId && d.current), 'Signed device listing failed');
    const revoke = await api(origin, 'devices/revoke', await deviceProof('revoke', deviceId), token); must(revoke.status === 200 && revoke.body.revoked && revoke.body.current && revoke.body.deviceId === deviceId, 'Signed self-revocation failed'); deviceId = null;
    must((await api(origin, 'targets', undefined, token)).status === 401, 'Revoked bearer remains usable');
  } finally {
    // Pair completion can commit even if its HTTP response is lost. Recover
    // exclusively by both freshly generated public keys, never by user names.
    const devices = parse(await server.exec(`${cli} devices list --socket /run/roc-wol/admin.sock`, 'locate only temporary verification identity'));
    must(Array.isArray(devices), 'Cannot confirm temporary identity cleanup');
    const temporary = devices.filter(d => d.deviceId === deviceId || d.wakeKeySHA256 === sha256(wakeDER) && d.manageKeySHA256 === sha256(manageDER));
    for (const d of temporary) {
      must(/^[a-f0-9]{32}$/.test(d.deviceId), 'Invalid temporary identity cleanup identifier');
      if (!d.revoked) await server.exec(`${cli} devices revoke --socket /run/roc-wol/admin.sock --id ${d.deviceId} >/dev/null`, 'clean up only temporary verification identity');
    }
  }
}
async function main() {
  const options = args(process.argv.slice(2)); if (options.command === 'help') { console.log(USAGE); return; }
  if (options.command === 'init') { await privateWrite(options.config, json(deploymentTemplate()), true); console.log('Empty protected v2 template created. Fill all required fields before inspect.'); return; }
  const input = parse(await fs.readFile(options.config));
  if (options.command === 'migrate-config') { await privateWrite(`${options.config}.v2.draft.json`, json(migrateConfig(input)), true); console.log('Adjacent v2 draft created without changing the source. Review ownership and fill missing fields manually.'); return; }
  const c = validateConfig(input, options.config, { allowMissingPins: options.command === 'inspect' }); const directory = path.join(ROOT, 'private/deployments', c.deploymentId), inspectionFile = path.join(directory, 'inspection.json');
  let server, router;
  try {
    server = await connect(c.server, 'server'); router = await connect(c.router, 'router');
    if (options.command === 'verify') { await publicGate(c); await privateStatus(server, c); for (const origin of c.publicOrigins) await protocolSmoke(server, c, origin); console.log('Explicit verification passed; all temporary identities revoked; no wake POST submitted.'); return; }
    if (options.command === 'stage') { const inspection = parse(await fs.readFile(inspectionFile)); must(inspection.success && inspection.configDigest === configDigest(c) && inspection.serverPin === c.server.hostKeySHA256 && inspection.routerPin === c.router.hostKeySHA256, 'Run a successful inspect for this unchanged configuration before stage'); }
    const info = await inspectHosts(server, router, c);
    if (options.command === 'inspect') { await privateWrite(inspectionFile, json({ version: 1, success: true, inspectedAt: new Date().toISOString(), configDigest: configDigest(c), serverPin: c.server.hostKeySHA256, routerPin: c.router.hostKeySHA256, machines: info.machines })); console.log('Read-only inspection passed; pins and normalized configuration digest saved locally.'); }
    else await stage(server, router, c, info, directory);
  } finally { router?.close(); server?.close(); }
}
main().catch(error => { console.error(`Stopped: ${error.message}`); process.exitCode = 1; });

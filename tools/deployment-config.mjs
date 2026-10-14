import path from 'node:path';
import net from 'node:net';
import crypto from 'node:crypto';

export const sha256 = value => crypto.createHash('sha256').update(value).digest('hex');
export function requireValue(ok, message) { if (!ok) throw new Error(message); }
const fail = field => { throw new Error(`Invalid deployment field: ${field}`); };
const control = /[\x00-\x20\x7f]/;
const digest = /^[a-f0-9]{64}$/;
const id = /^[a-f0-9]{32}$/;
function object(value, fields, field) {
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).some(key => !fields.includes(key))) fail(field);
  return value;
}
function string(value, field) { if (typeof value !== 'string' || !value || control.test(value)) fail(field); return value; }
export function remotePath(value, field = 'remote path') {
  string(value, field);
  if (!value.startsWith('/') || value === '/' || value.includes('\\') || value.split('/').some(part => part === '..' || part === '.') || path.posix.normalize(value) !== value) fail(field);
  return value;
}
function localPath(value, base, field) { string(value, field); return path.resolve(base, value); }
function port(value, fallback, field) { value ??= fallback; if (!Number.isInteger(value) || value < 1 || value > 65535) fail(field); return value; }
function host(value, field) {
  string(value, field);
  if (net.isIP(value)) return value.toLowerCase();
  if (value.length > 253 || !/^[A-Za-z0-9.-]+$/.test(value) || value.split('.').some(label => !/^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/.test(label))) fail(field);
  return value.toLowerCase();
}
export function normalizeOrigin(value) {
  if (typeof value !== 'string' || value.length > 512 || control.test(value) || /[%\\]/.test(value) || !/^https:\/\//.test(value)) fail('publicOrigins');
  const raw = value.slice(8);
  if (!raw || /[?#@]/.test(raw) || (raw.includes('/') && !raw.endsWith('/')) || raw.split('/').length > 2) fail('publicOrigins');
  let url; try { url = new URL(value); } catch { fail('publicOrigins'); }
  if (url.username || url.password || url.search || url.hash || !['', '/'].includes(url.pathname)) fail('publicOrigins');
  const hostname = url.hostname.replace(/^\[|\]$/g, '');
  host(hostname, 'publicOrigins');
  // URL parsing must not silently normalize numeric, encoded or Unicode hosts.
  const authority = raw.replace(/\/$/, '');
  const expectedHost = net.isIP(hostname) === 6 ? `[${hostname}]` : hostname;
  if (!new RegExp(`^${expectedHost.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(?::[0-9]+)?$`, 'i').test(authority)) fail('publicOrigins');
  const explicit = authority.startsWith('[') ? authority.slice(authority.indexOf(']') + 1) : authority.slice(expectedHost.length);
  if (explicit && (!/^:[0-9]+$/.test(explicit) || Number(explicit.slice(1)) < 1 || Number(explicit.slice(1)) > 65535)) fail('publicOrigins');
  return `https://${expectedHost.toLowerCase()}${url.port ? `:${url.port}` : ''}`;
}
function endpoint(value, base, field, allowMissingPins) {
  object(value, ['host', 'port', 'user', 'hostKeySHA256', 'auth'], field);
  if (value.user !== 'root') fail(`${field}.user (automatic deployment requires root)`);
  if (!(allowMissingPins && value.hostKeySHA256 === '') && !digest.test(value.hostKeySHA256 ?? '')) fail(`${field}.hostKeySHA256`);
  object(value.auth, ['privateKeyFile', 'passwordFile'], `${field}.auth`);
  if (Object.keys(value.auth).length !== 1) fail(`${field}.auth`);
  const auth = {};
  for (const key of Object.keys(value.auth)) auth[key] = localPath(value.auth[key], base, `${field}.auth.${key}`);
  return { host: host(value.host, `${field}.host`), port: port(value.port, 22, `${field}.port`), user: value.user, hostKeySHA256: value.hostKeySHA256, auth };
}
function target(value) {
  object(value, ['id', 'name', 'mac', 'interface', 'broadcast', 'ip'], 'targets');
  if (typeof value.id !== 'string' || !/^[a-z0-9][a-z0-9_-]{0,63}$/.test(value.id)) fail('targets.id');
  if (typeof value.name !== 'string' || [...value.name].length < 1 || [...value.name].length > 64 || /[\p{Cc}\p{Cf}]/u.test(value.name)) fail('targets.name');
  if (!/^([0-9a-f]{2}:){5}[0-9a-f]{2}$/i.test(value.mac ?? '') || parseInt(value.mac.slice(0, 2), 16) & 1 || /^00(:00){5}$/.test(value.mac)) fail('targets.mac');
  if (!/^[A-Za-z0-9_.:-]{1,15}$/.test(value.interface ?? '') || typeof value.broadcast !== 'boolean') fail('targets.interface/broadcast');
  if (net.isIP(value.ip) !== 4) fail('targets.ip');
  const bytes = value.ip.split('.').map(Number);
  if (bytes[0] === 0 || bytes[0] === 127 || bytes[0] >= 224 || value.ip === '255.255.255.255') fail('targets.ip');
  return { ...value, mac: value.mac.toLowerCase() };
}
export function validateConfig(input, filename, { allowMissingPins = false } = {}) {
  object(input, ['version', 'deploymentId', 'server', 'router', 'publicOrigins', 'gatewayPort', 'visitorPort', 'agentPort', 'targets', 'pingPath', 'etherwakePath', 'luciOrigin', 'maxDevices', 'transport', 'proxy', 'tls'], 'root');
  if (input.version !== 2 || !id.test(input.deploymentId ?? '')) fail('version/deploymentId');
  const base = path.dirname(path.resolve(filename));
  const c = { version: 2, deploymentId: input.deploymentId, server: endpoint(input.server, base, 'server', allowMissingPins), router: endpoint(input.router, base, 'router', allowMissingPins) };
  if (c.server.host === c.router.host && c.server.port === c.router.port) fail('server/router must be separate hosts');
  if (!Array.isArray(input.publicOrigins) || input.publicOrigins.length < 1 || input.publicOrigins.length > 8) fail('publicOrigins');
  c.publicOrigins = input.publicOrigins.map(normalizeOrigin);
  if (new Set(c.publicOrigins).size !== c.publicOrigins.length) fail('publicOrigins duplicates');
  for (const [key, fallback] of Object.entries({ gatewayPort: 18081, visitorPort: 18082, agentPort: 19991 })) c[key] = port(input[key], fallback, key);
  if (new Set([c.gatewayPort, c.visitorPort, c.agentPort]).size !== 3) fail('project ports must differ');
  if (!Array.isArray(input.targets) || input.targets.length < 1 || input.targets.length > 64) fail('targets');
  c.targets = input.targets.map(target);
  if (new Set(c.targets.map(t => t.id)).size !== c.targets.length) fail('targets duplicate ID');
  c.pingPath = remotePath(input.pingPath, 'pingPath');
  c.etherwakePath = remotePath(input.etherwakePath, 'etherwakePath');
  c.luciOrigin = input.luciOrigin ?? '';
  if (c.luciOrigin !== '') {
    if (typeof c.luciOrigin !== 'string' || control.test(c.luciOrigin) || /[%\\?#@]/.test(c.luciOrigin)) fail('luciOrigin');
    let u; try { u = new URL(c.luciOrigin); } catch { fail('luciOrigin'); }
    if (u.protocol !== 'http:' || !['127.0.0.1', '[::1]'].includes(u.hostname) || !['', '/'].includes(u.pathname) || c.luciOrigin.endsWith('/')) fail('luciOrigin');
    c.luciOrigin = u.origin;
  }
  c.maxDevices = input.maxDevices ?? 5;
  if (!Number.isInteger(c.maxDevices) || c.maxDevices < 1 || c.maxDevices > 100) fail('maxDevices');
  object(input.transport, ['mode', 'frpsHost', 'frpsPort', 'tokenFile', 'routerFrpcPath'], 'transport');
  if (!['frp', 'external'].includes(input.transport.mode)) fail('transport.mode');
  c.transport = { mode: input.transport.mode };
  if (c.transport.mode === 'external') { if (Object.keys(input.transport).length !== 1) fail('transport.external fields'); }
  else {
    c.transport.frpsHost = host(input.transport.frpsHost, 'transport.frpsHost');
    c.transport.frpsPort = port(input.transport.frpsPort, undefined, 'transport.frpsPort');
    c.transport.tokenFile = localPath(input.transport.tokenFile, base, 'transport.tokenFile');
    c.transport.routerFrpcPath = remotePath(input.transport.routerFrpcPath, 'transport.routerFrpcPath');
  }
  object(input.proxy, ['mode', 'configDir', 'container', 'webrootHost', 'webrootContainer', 'tlsDirHost', 'tlsDirContainer'], 'proxy');
  if (!['manual', 'nginx', 'onepanel'].includes(input.proxy.mode)) fail('proxy.mode');
  c.proxy = { mode: input.proxy.mode };
  if (c.proxy.mode === 'manual') { if (Object.keys(input.proxy).length !== 1) fail('proxy.manual fields'); }
  else {
    c.proxy.configDir = remotePath(input.proxy.configDir, 'proxy.configDir');
    for (const key of ['webrootHost', 'tlsDirHost']) if (input.proxy[key] !== undefined) c.proxy[key] = remotePath(input.proxy[key], `proxy.${key}`);
    if (c.proxy.mode === 'nginx') { if (['container', 'webrootContainer', 'tlsDirContainer'].some(key => input.proxy[key] !== undefined)) fail('proxy.nginx container fields'); }
    else {
      if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/.test(input.proxy.container ?? '')) fail('proxy.container');
      c.proxy.container = input.proxy.container;
      c.proxy.tlsDirHost = remotePath(input.proxy.tlsDirHost, 'proxy.tlsDirHost');
      c.proxy.tlsDirContainer = remotePath(input.proxy.tlsDirContainer, 'proxy.tlsDirContainer');
      if (input.proxy.webrootContainer !== undefined) c.proxy.webrootContainer = remotePath(input.proxy.webrootContainer, 'proxy.webrootContainer');
    }
  }
  object(input.tls, ['mode', 'email', 'fullchainFile', 'privateKeyFile'], 'tls');
  if (!['external', 'acme-http01'].includes(input.tls.mode)) fail('tls.mode');
  c.tls = { mode: input.tls.mode };
  if (c.tls.mode === 'external') {
    if (input.tls.email !== undefined) fail('tls.external email');
    c.tls.fullchainFile = remotePath(input.tls.fullchainFile, 'tls.fullchainFile');
    c.tls.privateKeyFile = remotePath(input.tls.privateKeyFile, 'tls.privateKeyFile');
    if (c.proxy.mode === 'onepanel') for (const key of ['fullchainFile', 'privateKeyFile']) mapContainerPath(c.tls[key], c.proxy.tlsDirHost, c.proxy.tlsDirContainer);
  } else {
    if (input.tls.fullchainFile !== undefined || input.tls.privateKeyFile !== undefined || c.proxy.mode === 'manual') fail('tls.acme fields/proxy');
    if (typeof input.tls.email !== 'string' || !/^[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$/.test(input.tls.email)) fail('tls.email');
    c.tls.email = input.tls.email;
    c.proxy.webrootHost = remotePath(c.proxy.webrootHost, 'proxy.webrootHost');
    c.proxy.tlsDirHost = remotePath(c.proxy.tlsDirHost, 'proxy.tlsDirHost');
    if (c.proxy.mode === 'onepanel') c.proxy.webrootContainer = remotePath(c.proxy.webrootContainer, 'proxy.webrootContainer');
  }
  for (const origin of c.publicOrigins) {
    const u = new URL(origin); const isIP = net.isIP(u.hostname.replace(/^\[|\]$/g, '')) !== 0;
    if (isIP && c.proxy.mode !== 'manual') fail('automatic proxy requires DNS origins');
    if (c.tls.mode === 'acme-http01' && (isIP || u.port)) fail('acme requires DNS origins on 443');
  }
  return c;
}
export function mapContainerPath(source, hostRoot, containerRoot) {
  if (!source.startsWith(`${hostRoot}/`)) fail('certificate outside tlsDirHost');
  return containerRoot + source.slice(hostRoot.length);
}
export function configDigest(c) { return sha256(JSON.stringify(c)); }
export function deploymentModes(c) { return { transport: c.transport.mode, proxy: c.proxy.mode, tls: c.tls.mode }; }
export function deploymentTemplate() {
  return { version: 2, deploymentId: crypto.randomBytes(16).toString('hex'), server: { host: '', port: 22, user: 'root', hostKeySHA256: '', auth: { privateKeyFile: '' } }, router: { host: '', port: 22, user: 'root', hostKeySHA256: '', auth: { passwordFile: '' } }, publicOrigins: [], gatewayPort: 18081, visitorPort: 18082, agentPort: 19991, targets: [], pingPath: '/bin/ping', etherwakePath: '/usr/bin/etherwake', luciOrigin: '', maxDevices: 5, transport: { mode: 'external' }, proxy: { mode: 'manual' }, tls: { mode: 'external', fullchainFile: '', privateKeyFile: '' } };
}
export function migrateConfig(old) {
  requireValue(old?.version === 1, 'Migration requires schema version 1');
  const draft = deploymentTemplate();
  if (id.test(old.deploymentId ?? '')) draft.deploymentId = old.deploymentId;
  for (const key of ['server', 'router']) {
    if (typeof old[key] === 'string') draft[key].host = old[key];
    else if (old[key] && typeof old[key] === 'object') draft[key] = { ...draft[key], ...old[key] };
  }
  draft.server.hostKeySHA256 = old.serverKey ?? draft.server.hostKeySHA256;
  draft.router.hostKeySHA256 = old.routerKey ?? draft.router.hostKeySHA256;
  draft.publicOrigins = old.publicOrigins ?? [old.directHost, old.fallbackHost].filter(Boolean).map(h => `https://${h}`);
  for (const key of ['gatewayPort', 'visitorPort', 'agentPort', 'targets', 'pingPath', 'etherwakePath', 'luciOrigin', 'maxDevices', 'transport', 'proxy', 'tls']) if (old[key] !== undefined) draft[key] = old[key];
  return draft;
}
export function runtimeConfigs(c) {
  return {
    gateway: { listen: `127.0.0.1:${c.gatewayPort}`, publicOrigins: c.publicOrigins, agentURL: `https://127.0.0.1:${c.visitorPort}`, agentServerName: 'router.roc-wol.internal', caFile: '/etc/roc-wol/pki/ca.pem', clientCertFile: '/etc/roc-wol/pki/client.pem', clientKeyFile: '/etc/roc-wol/pki/client.key', dataFile: '/var/lib/roc-wol/gateway.db', adminSocket: '/run/roc-wol/admin.sock', maxDevices: c.maxDevices, managementEnabled: !!c.luciOrigin },
    agent: { listen: `127.0.0.1:${c.agentPort}`, serverCertFile: '/etc/roc-wol/pki/server.pem', serverKeyFile: '/etc/roc-wol/pki/server.key', clientCAFile: '/etc/roc-wol/pki/ca.pem', clientURI: 'urn:roc-wol:gateway', dataFile: '/var/lib/roc-wol/agent.db', luciOrigin: c.luciOrigin, pingPath: c.pingPath, etherwakePath: c.etherwakePath, targets: c.targets },
  };
}
export function renderFrp(c, stcpKey, token) {
  const name = `roc-wol-${c.deploymentId}`;
  const common = `serverAddr = ${JSON.stringify(c.transport.frpsHost)}\nserverPort = ${c.transport.frpsPort}\nauth.method = "token"\nauth.token = ${JSON.stringify(token)}\nauth.additionalScopes = ["HeartBeats", "NewWorkConns"]\ntransport.tls.enable = true\n`;
  return { visitor: `${common}\n[[visitors]]\nname = "${name}-visitor"\ntype = "stcp"\nserverName = "${name}"\nsecretKey = ${JSON.stringify(stcpKey)}\nbindAddr = "127.0.0.1"\nbindPort = ${c.visitorPort}\n`, router: `${common}\n[[proxies]]\nname = "${name}"\ntype = "stcp"\nsecretKey = ${JSON.stringify(stcpKey)}\nlocalIP = "127.0.0.1"\nlocalPort = ${c.agentPort}\n` };
}
export function certificatePaths(c, container = c.proxy.mode === 'onepanel') {
  const files = c.tls.mode === 'external' ? { cert: c.tls.fullchainFile, key: c.tls.privateKeyFile } : { cert: `${c.proxy.tlsDirHost}/${c.deploymentId}/fullchain.pem`, key: `${c.proxy.tlsDirHost}/${c.deploymentId}/privkey.pem` };
  if (container) for (const key of Object.keys(files)) files[key] = mapContainerPath(files[key], c.proxy.tlsDirHost, c.proxy.tlsDirContainer);
  return files;
}
function nginxPath(value) { return JSON.stringify(value); }
export function renderProxy(c, bootstrap = false) {
  const files = certificatePaths(c); const result = {};
  for (const origin of c.publicOrigins) {
    const u = new URL(origin), hostname = u.hostname, port = u.port || '443';
    const acme = c.tls.mode === 'acme-http01' ? `    location ^~ /.well-known/acme-challenge/ { root ${nginxPath(c.proxy.mode === 'onepanel' ? c.proxy.webrootContainer : c.proxy.webrootHost)}; }\n` : '';
    const http = c.tls.mode === 'acme-http01' ? `server {\n    listen 80;\n    listen [::]:80;\n    server_name ${hostname};\n${acme}    location / { return 403; }\n}\n` : '';
    result[`${sha256(origin).slice(0, 16)}.roc-wol.conf`] = `# roc-wol deployment ${c.deploymentId}\n${http}${bootstrap ? '' : `server {\n    listen ${port} ssl;\n    listen [::]:${port} ssl;\n    server_name ${hostname};\n    ssl_certificate ${nginxPath(files.cert)};\n    ssl_certificate_key ${nginxPath(files.key)};\n    ssl_protocols TLSv1.2 TLSv1.3;\n    client_max_body_size 128m;\n    access_log off;\n    location / {\n        proxy_pass http://127.0.0.1:${c.gatewayPort};\n        proxy_http_version 1.1;\n        proxy_set_header Host $http_host;\n        proxy_set_header X-Forwarded-Proto https;\n        proxy_set_header X-Forwarded-For "";\n        proxy_set_header X-Real-IP "";\n        proxy_set_header Forwarded "";\n        proxy_set_header CF-Connecting-IP "";\n        proxy_set_header X-Forwarded-User "";\n        proxy_set_header X-Auth-Request-User "";\n        proxy_set_header X-Remote-User "";\n        proxy_set_header Upgrade $http_upgrade;\n        proxy_set_header Connection "upgrade";\n        proxy_buffering off;\n        proxy_request_buffering off;\n        proxy_cache off;\n        proxy_read_timeout 960s;\n        proxy_send_timeout 960s;\n    }\n}\n`}`;
  }
  return result;
}
export function validateOwner(owner, c) {
  if (owner === null) return;
  object(owner, ['version', 'deploymentId', 'phase', 'files', 'modes'], 'remote owner');
  requireValue(owner.version === 1 && owner.deploymentId === c.deploymentId && ['installing', 'ready'].includes(owner.phase), 'Remote deployment belongs to another instance or is invalid');
  requireValue(owner.modes && Object.keys(owner.modes).length === 3 && Object.entries(deploymentModes(c)).every(([key, value]) => owner.modes[key] === value), 'In-place transport/proxy/tls mode changes are unsupported; manually retire owned resources first');
  requireValue(Array.isArray(owner.files), 'Invalid remote ownership file list');
  const seen = new Set();
  for (const file of owner.files) {
    object(file, ['path', 'installedSHA256', 'desiredSHA256'], 'owner.files'); remotePath(file.path);
    requireValue(!seen.has(file.path) && (file.installedSHA256 === null || digest.test(file.installedSHA256)) && digest.test(file.desiredSHA256), 'Invalid remote ownership entry'); seen.add(file.path);
  }
}
export function checkResource(owner, filename, currentSHA256) {
  const record = owner?.files.find(f => f.path === filename);
  if (!record) requireValue(currentSHA256 === null, 'Unregistered existing resource; manual ownership review required');
  else requireValue(currentSHA256 === null ? record.installedSHA256 === null : [record.installedSHA256, record.desiredSHA256].includes(currentSHA256), 'Owned resource was externally modified or removed; refusing overwrite');
}
export function selectArtifact(manifest, role, machine) {
  const arch = { x86_64: 'amd64', aarch64: 'arm64', arm64: 'arm64' }[machine];
  requireValue(arch, 'Unsupported remote architecture');
  object(manifest, ['version', 'apiVersion', 'builtAt', 'apkCertificateSHA256', 'sha256', 'platforms'], 'manifest');
  requireValue(manifest.version === 2 && manifest.apiVersion === 2 && typeof manifest.builtAt === 'string' && digest.test(manifest.apkCertificateSHA256 ?? ''), 'Unsupported artifact manifest');
  requireValue(manifest.platforms && manifest.sha256, 'Missing artifact manifest maps');
  const matches = Object.entries(manifest.platforms).filter(([, p]) => p.os === 'linux' && p.arch === arch && p.role === role);
  requireValue(matches.length === 1, 'Missing or ambiguous platform artifact');
  const [filename, platform] = matches[0]; object(platform, ['os', 'arch', 'role'], 'manifest platform');
  requireValue(/^[A-Za-z0-9_.-]+$/.test(filename) && digest.test(manifest.sha256[filename] ?? ''), 'Invalid artifact filename/checksum');
  return filename;
}
export const FRP_RELEASE = { version: '0.69.1', checksums: { amd64: '7be257b72dbbc60bcb3e0e25a5afd1dfac7b63f897084864d3c956dd3d5674e1', arm64: 'bbc0c75e896af3f292fb46ba09c844a04fa9b5ea3530c039c7af20637f836355' } };
export function checkVhosts(source, c, owned) {
  // nginx -T names every included file. Reject matching server blocks outside registered project files.
  let filename = ''; const chunks = source.split(/(?=^# configuration file )/m);
  for (const chunk of chunks) {
    const match = chunk.match(/^# configuration file (.+):\s*$/m); if (match) filename = match[1];
    const text = chunk.replace(/^\s*#.*$/gm, ''); const tokens = text.match(/"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[{};]|[^\s{};]+/g) ?? [];
    for (let i = 0; i < tokens.length; i++) if (tokens[i] === 'server' && tokens[i + 1] === '{') {
      let depth = 1, block = [], j = i + 2; for (; j < tokens.length && depth; j++) { if (tokens[j] === '{') depth++; if (tokens[j] === '}') depth--; if (depth === 1) block.push(tokens[j]); }
      const names = [], ports = []; for (let k = 0; k < block.length; k++) {
        if (block[k] === 'server_name') for (++k; block[k] !== ';' && k < block.length; k++) names.push(block[k].replace(/^['"]|['"]$/g, ''));
        if (block[k] === 'listen') { const value = block[++k]; ports.push(value?.match(/(?:^|:)(\d+)$/)?.[1] ?? (value === '*' ? '80' : '')); }
      }
      const conflict = c.publicOrigins.some(origin => { const u = new URL(origin); return names.some(n => n === u.hostname || n === '_' || (n.startsWith('*.') && u.hostname.endsWith(n.slice(1))) || n.startsWith('~')) && ports.includes(u.port || '443'); }) || c.tls.mode === 'acme-http01' && c.publicOrigins.some(origin => names.includes(new URL(origin).hostname) && ports.includes('80'));
      if (conflict) {
        const approved = owned?.files.some(f => f.path === filename);
        requireValue(approved, 'Unowned vhost conflicts with configured server_name/listen; refusing replacement');
      }
      i = j - 1;
    }
  }
  if (c.proxy.mode === 'nginx') requireValue(source.includes(c.proxy.configDir), 'Configured configDir is not included by nginx');
}

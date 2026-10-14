import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { deploymentTemplate, validateConfig, normalizeOrigin, remotePath, renderProxy, renderFrp, certificatePaths, mapContainerPath, validateOwner, checkResource, checkVhosts, selectArtifact, sha256, deploymentModes } from './deployment-config.mjs';
const filename = path.resolve('fixture/deployment.json');
function fixture() {
  const c = deploymentTemplate();
  c.deploymentId = 'a'.repeat(32);
  c.server = { host: '192.0.2.10', user: 'root', hostKeySHA256: 'b'.repeat(64), auth: { privateKeyFile: './server.key' } };
  c.router = { host: '192.0.2.20', user: 'root', hostKeySHA256: 'c'.repeat(64), auth: { passwordFile: './router.password' } };
  c.publicOrigins = ['https://wol.example'];
  c.targets = [{ id: 'pc-a', name: 'Computer A', mac: '02:00:00:00:00:01', ip: '192.0.2.30', interface: 'br-lan', broadcast: true }];
  c.tls = { mode: 'external', fullchainFile: '/etc/tls/fullchain.pem', privateKeyFile: '/etc/tls/key.pem' };
  return c;
}
const config = () => validateConfig(fixture(), filename);
test('strict schema, defaults and relative secret references', () => {
  const c = config();
  assert.equal(c.server.auth.privateKeyFile, path.resolve('fixture/server.key'));
  const invalid = fixture(); invalid.unknown = 'secret-never-print';
  assert.throws(() => validateConfig(invalid, filename), e => !e.message.includes('secret-never-print'));
  assert.throws(() => validateConfig(deploymentTemplate(), filename));
});
test('origins reject unsafe input and normalize HTTPS', () => {
  assert.equal(normalizeOrigin('https://WOL.example:443/'), 'https://wol.example');
  assert.equal(normalizeOrigin('https://wol.example:8443'), 'https://wol.example:8443');
  assert.equal(normalizeOrigin('https://[::1]'), 'https://[::1]');
  for (const value of ['http://wol.example', 'https://user@wol.example', 'https://wol.example/path', 'https://wol.example?x', 'https://wol.example#x', 'https://%77ol.example', 'https://wol.example:0', 'https://127.1', 'https://wol.example\\other']) assert.throws(() => normalizeOrigin(value));
});
test('paths, pins, ports and target safety', () => {
  for (const value of ['/etc/../secret', '/etc/./secret', '/etc//secret', 'relative', '/etc/secret\n']) assert.throws(() => remotePath(value));
  for (const mutate of [c => c.server.hostKeySHA256 = '', c => c.gatewayPort = c.agentPort, c => c.targets[0].mac = 'ff:ff:ff:ff:ff:ff', c => c.targets[0].ip = '127.0.0.1', c => c.targets.push({ ...c.targets[0] }), c => c.maxDevices = 101, c => c.server.auth.passwordFile = './extra']) {
    const c = fixture(); mutate(c); assert.throws(() => validateConfig(c, filename));
  }
});
test('proxy rendering preserves external certificates and emits required FRP authentication scopes', () => {
  const c = config();
  assert.deepEqual(certificatePaths(c), { cert: '/etc/tls/fullchain.pem', key: '/etc/tls/key.pem' });
  const snippet = Object.values(renderProxy(c))[0];
  assert.match(snippet, /proxy_set_header Host \$http_host/);
  assert.match(snippet, /\/etc\/tls\/fullchain.pem/);
  assert.doesNotMatch(snippet, /acme-challenge/);
  const raw = fixture(); raw.proxy = { mode: 'nginx', configDir: '/etc/nginx/conf.d' };
  assert.equal(validateConfig(raw, filename).proxy.mode, 'nginx');
  raw.transport = { mode: 'frp', frpsHost: 'frp.example', frpsPort: 7000, tokenFile: './token', routerFrpcPath: '/usr/bin/frpc' };
  const frp = renderFrp(validateConfig(raw, filename), 'key', 'token');
  for (const value of Object.values(frp)) { assert.match(value, /additionalScopes = \["HeartBeats", "NewWorkConns"\]/); assert.match(value, /transport.tls.enable = true/); }
  assert.match(frp.router, new RegExp(`roc-wol-${raw.deploymentId}`));
});
test('ACME and container certificate mappings are explicit', () => {
  const raw = fixture(); raw.proxy = { mode: 'onepanel', configDir: '/etc/proxy', container: 'openresty', tlsDirHost: '/etc/tls', tlsDirContainer: '/tls' };
  let c = validateConfig(raw, filename);
  assert.equal(certificatePaths(c).cert, '/tls/fullchain.pem');
  assert.throws(() => mapContainerPath('/etc/tls-other/key', '/etc/tls', '/tls'));
  raw.tls = { mode: 'acme-http01', email: 'admin@example.com' };
  assert.throws(() => validateConfig(raw, filename));
  raw.proxy.webrootHost = '/srv/challenge'; raw.proxy.webrootContainer = '/challenge';
  c = validateConfig(raw, filename);
  assert.match(certificatePaths(c).cert, new RegExp(`/tls/${raw.deploymentId}/fullchain.pem`));
  assert.doesNotMatch(Object.values(renderProxy(c, true))[0], /ssl_certificate/);
  raw.publicOrigins = ['https://wol.example:8443']; assert.throws(() => validateConfig(raw, filename));
});
test('ownership rejects foreign instances, mode changes and external edits; permits interrupted replay', () => {
  const c = config(), installed = sha256('old'), desired = sha256('new');
  const owner = { version: 1, deploymentId: c.deploymentId, phase: 'installing', modes: deploymentModes(c), files: [{ path: '/etc/roc-wol/agent.json', installedSHA256: installed, desiredSHA256: desired }] };
  validateOwner(owner, c);
  checkResource(owner, owner.files[0].path, installed);
  checkResource(owner, owner.files[0].path, desired);
  assert.throws(() => checkResource(owner, owner.files[0].path, sha256('external')));
  assert.throws(() => checkResource(owner, owner.files[0].path, null));
  checkResource(null, '/etc/roc-wol/new', null);
  assert.throws(() => checkResource(null, '/etc/roc-wol/new', installed));
  const pending = structuredClone(owner); pending.files[0].installedSHA256 = null;
  checkResource(pending, pending.files[0].path, null);
  checkResource(pending, pending.files[0].path, desired);
  assert.throws(() => validateOwner({ ...owner, deploymentId: 'd'.repeat(32) }, c));
  for (const axis of ['transport', 'proxy', 'tls']) assert.throws(() => validateOwner({ ...owner, modes: { ...owner.modes, [axis]: 'different' } }, c));
});
test('vhost conflict requires registered ownership', () => {
  const c = config(); c.proxy = { mode: 'nginx', configDir: '/etc/nginx/conf.d' };
  const file = '/etc/nginx/conf.d/wol.conf';
  const source = `# configuration file ${file}:\nserver { listen 443 ssl; server_name wol.example; location / { return 401; } }`;
  assert.throws(() => checkVhosts(source, c, null), /Unowned vhost/);
  checkVhosts(source, c, { files: [{ path: file }] });
  assert.throws(() => checkVhosts(source.replace('wol.example', '*.example'), c, null));
});
test('artifact selection rejects unsupported architecture and untrusted names', () => {
  const manifest = { version: 2, apiVersion: 2, builtAt: '2026-01-01', apkCertificateSHA256: 'a'.repeat(64), platforms: { 'gateway-arm64': { os: 'linux', arch: 'arm64', role: 'gateway' } }, sha256: { 'gateway-arm64': 'b'.repeat(64) } };
  assert.equal(selectArtifact(manifest, 'gateway', 'aarch64'), 'gateway-arm64');
  assert.throws(() => selectArtifact(manifest, 'gateway', 'mips'));
  assert.throws(() => selectArtifact(manifest, 'agent', 'aarch64'));
  assert.throws(() => selectArtifact({ ...manifest, sha256: {} }, 'gateway', 'aarch64'));
});

#!/usr/bin/env node
import fs from 'node:fs/promises';
import crypto from 'node:crypto';
import path from 'node:path';

const [url, destination, algorithm, expected] = process.argv.slice(2);
const official = new Set(['api.adoptium.net', 'github.com', 'release-assets.githubusercontent.com', 'objects.githubusercontent.com', 'raw.githubusercontent.com', 'dl.google.com', 'services.gradle.org', 'downloads.gradle.org']);
async function main() {
  if (!url || !destination || !['metadata', 'sha256', 'sha1'].includes(algorithm)) throw new Error('Usage: download.mjs HTTPS_URL DEST metadata|sha256|sha1 [EXPECTED_HASH]');
  let current = new URL(url);
  let response;
  for (let redirects = 0; redirects <= 8; redirects++) {
    if (current.protocol !== 'https:' || !official.has(current.hostname) || current.username || current.password) throw new Error('Refusing non-official download origin');
    response = await fetch(current, { redirect: 'manual', signal: AbortSignal.timeout(300000), headers: { 'User-Agent': 'RocWol-build/1.0' } });
    if ([301, 302, 303, 307, 308].includes(response.status)) { await response.body?.cancel(); current = new URL(response.headers.get('location'), current); continue; }
    break;
  }
  if (!response?.ok) throw new Error('Official download failed');
  const content = Buffer.from(await response.arrayBuffer());
  if (algorithm === 'metadata') {
    if (content.length > 32 * 1024 * 1024) throw new Error('Unexpected metadata size');
  } else {
    const pattern = algorithm === 'sha256' ? /^[a-fA-F0-9]{64}$/ : /^[a-fA-F0-9]{40}$/;
    if (!pattern.test(expected || '') || crypto.createHash(algorithm).update(content).digest('hex') !== expected.toLowerCase()) throw new Error('Official checksum mismatch; nothing installed');
  }
  await fs.mkdir(path.dirname(path.resolve(destination)), { recursive: true });
  const temporary = `${destination}.${crypto.randomBytes(8).toString('hex')}.new`;
  await fs.writeFile(temporary, content, { flag: 'wx' });
  await fs.rename(temporary, destination);
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });

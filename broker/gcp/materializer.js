const crypto = require('crypto');
const zlib = require('zlib');
const {Storage} = require('@google-cloud/storage');
const {GoogleAuth} = require('google-auth-library');
const yaml = require('js-yaml');

const storage = new Storage();
const auth = new GoogleAuth({scopes: ['https://www.googleapis.com/auth/cloud-platform']});
const brokerVersion = process.env.BROKER_VERSION || '{{BROKER_VERSION}}';

function gcpProjectID() {
  return String(process.env.BGIT_GCP_PROJECT || process.env.GOOGLE_CLOUD_PROJECT || process.env.GCP_PROJECT || '').trim();
}

function response(res, status, body) {
  res.status(status).type('application/json').send(JSON.stringify(body));
}

function cleanPath(path) {
  const out = String(path || '').replace(/^\/+/, '');
  if (!out || out.includes('\\') || out.includes('\0') || out.split('/').includes('..')) throw new Error('invalid path');
  return out;
}

async function secretManagerRequest(path) {
  const client = await auth.getClient();
  const accessToken = await client.getAccessToken();
  const token = typeof accessToken === 'string' ? accessToken : accessToken && accessToken.token;
  const resp = await fetch('https://secretmanager.googleapis.com/v1/' + path, {
    headers: {authorization: 'Bearer ' + token},
  });
  const text = await resp.text();
  if (!resp.ok) throw new Error(text || ('Secret Manager returned HTTP ' + resp.status));
  return text ? JSON.parse(text) : {};
}

async function materializerToken() {
  const secret = String(process.env.BGIT_CI_MATERIALIZER_SECRET || '').trim();
  if (!secret) return '';
  const data = await secretManagerRequest(secret + '/versions/latest:access');
  return Buffer.from(data.payload && data.payload.data || '', 'base64').toString('utf8').trim();
}

async function verifyToken(req) {
  const want = await materializerToken();
  if (!want) return;
  const got = String(req.get('x-bgit-ci-token') || '').trim();
  const a = Buffer.from(want);
  const b = Buffer.from(got);
  if (a.length !== b.length || !crypto.timingSafeEqual(a, b)) throw Object.assign(new Error('invalid CI materializer token'), {status: 403});
}

async function readObject(repo, path) {
  const bucket = String(repo.bucket || '').trim();
  const prefix = String(repo.prefix || '').replace(/^\/+|\/+$/g, '');
  if (!bucket || !prefix) throw new Error('repo bucket/prefix is required');
  const key = prefix + '/' + cleanPath(path);
  const [data] = await storage.bucket(bucket).file(key).download();
  return data;
}

async function listObjects(repo, path) {
  const bucket = String(repo.bucket || '').trim();
  const prefix = String(repo.prefix || '').replace(/^\/+|\/+$/g, '');
  if (!bucket || !prefix) throw new Error('repo bucket/prefix is required');
  const base = prefix + '/' + cleanPath(path).replace(/\/?$/, '/');
  const [files] = await storage.bucket(bucket).getFiles({prefix: base});
  return files.map((file) => file.name.slice(prefix.length + 1));
}

const packIndexCache = new Map();
const packDataCache = new Map();
const packObjectCache = new Map();

async function readLooseGitObject(repo, hash) {
  if (!/^[0-9a-f]{40}$/i.test(hash)) throw new Error('invalid git object hash');
  let compressed;
  try {
    compressed = await readObject(repo, 'objects/' + hash.slice(0, 2) + '/' + hash.slice(2));
  } catch (err) {
    return readPackedGitObject(repo, hash);
  }
  const raw = zlib.inflateSync(compressed);
  const nul = raw.indexOf(0);
  if (nul < 0) throw new Error('invalid git object');
  const header = raw.subarray(0, nul).toString('utf8');
  const space = header.indexOf(' ');
  return {type: header.slice(0, space), data: raw.subarray(nul + 1)};
}

function repoCacheKey(repo) {
  return String(repo.bucket || '') + '/' + String(repo.prefix || '').replace(/^\/+|\/+$/g, '');
}

async function readPackedGitObject(repo, hash) {
  const indexes = await loadPackIndexes(repo);
  for (const index of indexes) {
    const pos = binarySearch(index.hashes, hash);
    if (pos >= 0) return objectAtPackOffset(repo, index, index.offsets[pos]);
  }
  throw new Error('git object not found: ' + hash);
}

async function loadPackIndexes(repo) {
  const key = repoCacheKey(repo);
  if (packIndexCache.has(key)) return packIndexCache.get(key);
  const paths = (await listObjects(repo, 'objects/pack')).filter((path) => path.endsWith('.idx')).sort();
  const indexes = [];
  for (const path of paths) {
    const data = await readObject(repo, path);
    indexes.push(parsePackIndex(path, data));
  }
  packIndexCache.set(key, indexes);
  return indexes;
}

function parsePackIndex(path, data) {
  if (data.length < 8 || data.readUInt32BE(0) !== 0xff744f63) throw new Error('unsupported pack index format');
  const version = data.readUInt32BE(4);
  if (version !== 2) throw new Error('unsupported pack index version ' + version);
  const fanoutStart = 8;
  const total = data.readUInt32BE(fanoutStart + 255 * 4);
  const hashStart = fanoutStart + 256 * 4;
  const crcStart = hashStart + total * 20;
  const offsetStart = crcStart + total * 4;
  if (data.length < offsetStart + total * 4) throw new Error('truncated pack index');
  const hashes = [];
  const offsets = [];
  const largeRefs = [];
  for (let i = 0; i < total; i++) hashes.push(data.subarray(hashStart + i * 20, hashStart + (i + 1) * 20).toString('hex'));
  for (let i = 0; i < total; i++) {
    const raw = data.readUInt32BE(offsetStart + i * 4);
    if (raw & 0x80000000) {
      largeRefs.push({entry: i, index: raw & 0x7fffffff});
      offsets[i] = 0;
    } else {
      offsets[i] = raw;
    }
  }
  const largeStart = offsetStart + total * 4;
  for (const ref of largeRefs) {
    const pos = largeStart + ref.index * 8;
    if (data.length < pos + 8) throw new Error('truncated large pack index offsets');
    offsets[ref.entry] = Number(data.readBigUInt64BE(pos));
  }
  return {idxPath: path, packPath: path.replace(/\.idx$/, '.pack'), hashes, offsets};
}

function binarySearch(values, target) {
  let low = 0;
  let high = values.length - 1;
  while (low <= high) {
    const mid = (low + high) >> 1;
    if (values[mid] === target) return mid;
    if (values[mid] < target) low = mid + 1;
    else high = mid - 1;
  }
  return -1;
}

async function packData(repo, path) {
  const key = repoCacheKey(repo) + '/' + path;
  if (!packDataCache.has(key)) packDataCache.set(key, await readObject(repo, path));
  return packDataCache.get(key);
}

async function objectAtPackOffset(repo, index, offset) {
  const key = repoCacheKey(repo) + '/' + index.packPath + ':' + offset;
  if (packObjectCache.has(key)) return packObjectCache.get(key);
  const pack = await packData(repo, index.packPath);
  const obj = await decodePackedObject(repo, index, pack, offset);
  packObjectCache.set(key, obj);
  return obj;
}

async function decodePackedObject(repo, index, pack, offset) {
  if (pack.length < 12 || !pack.subarray(0, 4).equals(Buffer.from('PACK'))) throw new Error('invalid pack file');
  let pos = Number(offset);
  const header = parsePackObjectHeader(pack, pos);
  pos += header.bytes;
  if (header.type >= 1 && header.type <= 4) {
    return {type: packTypeName(header.type), data: zlib.inflateSync(pack.subarray(pos))};
  }
  if (header.type === 6) {
    const parsed = parseOFSDeltaBase(pack, pos, offset);
    pos += parsed.bytes;
    const delta = zlib.inflateSync(pack.subarray(pos));
    const base = await objectAtPackOffset(repo, index, parsed.baseOffset);
    return {type: base.type, data: applyPackDelta(base.data, delta)};
  }
  if (header.type === 7) {
    if (pack.length < pos + 20) throw new Error('truncated ref delta object');
    const baseHash = pack.subarray(pos, pos + 20).toString('hex');
    const delta = zlib.inflateSync(pack.subarray(pos + 20));
    const base = await readLooseGitObject(repo, baseHash);
    return {type: base.type, data: applyPackDelta(base.data, delta)};
  }
  throw new Error('unsupported pack object type ' + header.type);
}

function parsePackObjectHeader(pack, pos) {
  let byte = pack[pos++];
  if (byte === undefined) throw new Error('truncated pack object header');
  const type = (byte >> 4) & 7;
  let size = byte & 0x0f;
  let shift = 4;
  let bytes = 1;
  while (byte & 0x80) {
    byte = pack[pos++];
    if (byte === undefined) throw new Error('truncated pack object header');
    size |= (byte & 0x7f) << shift;
    shift += 7;
    bytes++;
  }
  return {type, size, bytes};
}

function parseOFSDeltaBase(pack, pos, currentOffset) {
  let byte = pack[pos++];
  if (byte === undefined) throw new Error('truncated ofs-delta header');
  let value = byte & 0x7f;
  let bytes = 1;
  while (byte & 0x80) {
    byte = pack[pos++];
    if (byte === undefined) throw new Error('truncated ofs-delta header');
    value = ((value + 1) << 7) | (byte & 0x7f);
    bytes++;
  }
  return {baseOffset: Number(currentOffset) - value, bytes};
}

function packTypeName(type) {
  if (type === 1) return 'commit';
  if (type === 2) return 'tree';
  if (type === 3) return 'blob';
  if (type === 4) return 'tag';
  return 'unknown';
}

function readDeltaSize(delta, state) {
  let size = 0;
  let shift = 0;
  while (state.pos < delta.length) {
    const byte = delta[state.pos++];
    size |= (byte & 0x7f) << shift;
    if (!(byte & 0x80)) return size;
    shift += 7;
  }
  throw new Error('truncated delta size');
}

function applyPackDelta(base, delta) {
  const state = {pos: 0};
  readDeltaSize(delta, state);
  const targetSize = readDeltaSize(delta, state);
  const out = [];
  let written = 0;
  while (state.pos < delta.length) {
    const op = delta[state.pos++];
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      if (op & 0x01) offset |= delta[state.pos++];
      if (op & 0x02) offset |= delta[state.pos++] << 8;
      if (op & 0x04) offset |= delta[state.pos++] << 16;
      if (op & 0x08) offset |= delta[state.pos++] << 24;
      if (op & 0x10) size |= delta[state.pos++];
      if (op & 0x20) size |= delta[state.pos++] << 8;
      if (op & 0x40) size |= delta[state.pos++] << 16;
      if (size === 0) size = 0x10000;
      if (offset < 0 || size < 0 || offset + size > base.length) throw new Error('invalid delta copy');
      out.push(base.subarray(offset, offset + size));
      written += size;
    } else if (op) {
      if (state.pos + op > delta.length) throw new Error('invalid delta insert');
      out.push(delta.subarray(state.pos, state.pos + op));
      state.pos += op;
      written += op;
    } else {
      throw new Error('invalid delta opcode');
    }
  }
  if (written !== targetSize) throw new Error('delta target size mismatch');
  return Buffer.concat(out, written);
}

async function treeForCommit(repo, commitHash) {
  const obj = await readLooseGitObject(repo, commitHash);
  if (obj.type !== 'commit') throw new Error('CI commit is not a commit object');
  const match = /^tree ([0-9a-f]{40})$/m.exec(obj.data.toString('utf8'));
  if (!match) throw new Error('commit has no tree');
  return match[1];
}

async function collectTree(repo, treeHash, base, files) {
  const obj = await readLooseGitObject(repo, treeHash);
  if (obj.type !== 'tree') throw new Error('tree object expected');
  let pos = 0;
  while (pos < obj.data.length) {
    const space = obj.data.indexOf(0x20, pos);
    const nul = obj.data.indexOf(0, space + 1);
    if (space < 0 || nul < 0 || nul + 21 > obj.data.length) throw new Error('invalid tree object');
    const mode = obj.data.subarray(pos, space).toString('utf8');
    const name = obj.data.subarray(space + 1, nul).toString('utf8');
    const hash = obj.data.subarray(nul + 1, nul + 21).toString('hex');
    const path = base ? base + '/' + name : name;
    pos = nul + 21;
    if (mode === '40000') {
      await collectTree(repo, hash, path, files);
      continue;
    }
    const child = await readLooseGitObject(repo, hash);
    if (child.type !== 'blob') continue;
    files.push({path, mode: mode === '100755' ? 0o755 : 0o644, data: child.data});
  }
}

function tarOctal(value, width) {
  return value.toString(8).padStart(width - 1, '0') + '\0';
}

function tarHeader(file) {
  const header = Buffer.alloc(512);
  const name = Buffer.from(file.path);
  if (name.length > 100) throw new Error('CI source path is too long for tar header: ' + file.path);
  name.copy(header, 0);
  Buffer.from(tarOctal(file.mode, 8)).copy(header, 100);
  Buffer.from(tarOctal(0, 8)).copy(header, 108);
  Buffer.from(tarOctal(0, 8)).copy(header, 116);
  Buffer.from(tarOctal(file.data.length, 12)).copy(header, 124);
  Buffer.from(tarOctal(Math.floor(Date.now() / 1000), 12)).copy(header, 136);
  header.fill(0x20, 148, 156);
  header[156] = '0'.charCodeAt(0);
  Buffer.from('ustar\0').copy(header, 257);
  Buffer.from('00').copy(header, 263);
  let sum = 0;
  for (const byte of header) sum += byte;
  Buffer.from(tarOctal(sum, 8)).copy(header, 148);
  return header;
}

function tarGz(files) {
  const chunks = [];
  for (const file of files) {
    chunks.push(tarHeader(file), file.data);
    const pad = (512 - (file.data.length % 512)) % 512;
    if (pad) chunks.push(Buffer.alloc(pad));
  }
  chunks.push(Buffer.alloc(1024));
  return zlib.gzipSync(Buffer.concat(chunks));
}

async function startCloudBuild(repo, run, sourceBucket, sourceObject, configText) {
  const project = gcpProjectID();
  if (!project) throw new Error('GCP project is not configured');
  const client = await auth.getClient();
  const accessToken = await client.getAccessToken();
  const token = typeof accessToken === 'string' ? accessToken : accessToken && accessToken.token;
  const parsed = yaml.load(configText);
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw Object.assign(new Error('CI config must contain a Cloud Build object'), {status: 400});
  const build = {
    ...parsed,
    source: {storageSource: {bucket: sourceBucket, object: sourceObject}},
    substitutions: {
      ...(parsed.substitutions || {}),
      _BGIT_REPO: String(repo.logical || repo.prefix || ''),
      _BGIT_REF: run.ref,
      _BGIT_COMMIT: run.commit,
      _BGIT_BROKER_VERSION: brokerVersion,
    },
    options: {...(parsed.options || {}), logging: 'CLOUD_LOGGING_ONLY', substitutionOption: 'ALLOW_LOOSE'},
  };
  const serviceAccount = String(process.env.BGIT_CI_BUILD_SERVICE_ACCOUNT || '').trim();
  if (serviceAccount) build.serviceAccount = 'projects/' + project + '/serviceAccounts/' + serviceAccount;
  const resp = await fetch('https://cloudbuild.googleapis.com/v1/projects/' + project + '/builds', {
    method: 'POST',
    headers: {authorization: 'Bearer ' + token, 'content-type': 'application/json'},
    body: JSON.stringify(build),
  });
  const text = await resp.text();
  if (!resp.ok) throw new Error(text || ('Cloud Build returned HTTP ' + resp.status));
  return text ? JSON.parse(text) : {};
}

exports.materializer = async (req, res) => {
  try {
    await verifyToken(req);
    const body = req.body && typeof req.body === 'object' ? req.body : {};
    const repo = body.repo || {};
    const run = body.run || {};
    if (run.provider !== 'gcp') throw Object.assign(new Error('GCP materializer only accepts gcp runs'), {status: 400});
    if (!/^[0-9a-f]{40}$/i.test(String(run.commit || ''))) throw Object.assign(new Error('invalid CI commit'), {status: 400});
    const files = [];
    await collectTree(repo, await treeForCommit(repo, run.commit), '', files);
    const configFile = files.find((file) => file.path === run.config);
    if (!configFile) throw Object.assign(new Error('CI config not found in commit: ' + run.config), {status: 400});
    const bucket = repo.bucket;
    const object = String(repo.prefix || '').replace(/^\/+|\/+$/g, '') + '/_bgit-ci/run-' + run.id + '-' + run.commit + '.tar.gz';
    await storage.bucket(bucket).file(object).save(tarGz(files), {contentType: 'application/gzip'});
    const build = await startCloudBuild(repo, run, bucket, object, configFile.data.toString('utf8'));
    const operationName = String(build.name || '');
    const operationID = operationName.split('/').pop() || '';
    let buildID = build.id || build.metadata && build.metadata.build && build.metadata.build.id || '';
    if (!buildID && operationID) {
      try { buildID = Buffer.from(operationID, 'base64url').toString('utf8'); } catch (_) {}
    }
    const buildName = build.name && String(build.name).startsWith('projects/') && String(build.name).includes('/builds/')
      ? build.name
      : (buildID ? 'projects/' + gcpProjectID() + '/builds/' + buildID : '');
    response(res, 200, {
      status: 'queued',
      url: build.logUrl || build.metadata && build.metadata.build && build.metadata.build.logUrl || '',
      message: operationName || buildID || 'Cloud Build queued',
      provider_build_id: buildID,
      provider_build_name: buildName,
    });
  } catch (err) {
    response(res, err.status || 500, {error: err.message || String(err)});
  }
};

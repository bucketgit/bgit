const crypto = require('crypto');
const zlib = require('zlib');
const {Storage} = require('@google-cloud/storage');
const {GoogleAuth} = require('google-auth-library');

const storage = new Storage();
const auth = new GoogleAuth({scopes: ['https://www.googleapis.com/auth/cloud-platform']});
const brokerVersion = process.env.BROKER_VERSION || '{{BROKER_VERSION}}';

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

async function readLooseGitObject(repo, hash) {
  if (!/^[0-9a-f]{40}$/i.test(hash)) throw new Error('invalid git object hash');
  const compressed = await readObject(repo, 'objects/' + hash.slice(0, 2) + '/' + hash.slice(2));
  const raw = zlib.inflateSync(compressed);
  const nul = raw.indexOf(0);
  if (nul < 0) throw new Error('invalid git object');
  const header = raw.subarray(0, nul).toString('utf8');
  const space = header.indexOf(' ');
  return {type: header.slice(0, space), data: raw.subarray(nul + 1)};
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

async function startCloudBuild(repo, run, sourceBucket, sourceObject) {
  const project = String(process.env.BGIT_GCP_PROJECT || process.env.GOOGLE_CLOUD_PROJECT || process.env.GCP_PROJECT || '').trim();
  if (!project) throw new Error('GCP project is not configured');
  const client = await auth.getClient();
  const accessToken = await client.getAccessToken();
  const token = typeof accessToken === 'string' ? accessToken : accessToken && accessToken.token;
  const build = {
    source: {storageSource: {bucket: sourceBucket, object: sourceObject}},
    steps: [{
      name: 'gcr.io/google.com/cloudsdktool/cloud-sdk:slim',
      entrypoint: 'bash',
      args: ['-lc', 'gcloud builds submit --project "$PROJECT_ID" --config "$_BGIT_CONFIG" .'],
    }],
    substitutions: {
      _BGIT_REPO: String(repo.logical || repo.prefix || ''),
      _BGIT_REF: run.ref,
      _BGIT_COMMIT: run.commit,
      _BGIT_BROKER_VERSION: brokerVersion,
      _BGIT_CONFIG: run.config,
    },
    options: {logging: 'CLOUD_LOGGING_ONLY', substitutionOption: 'ALLOW_LOOSE'},
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
    if (!files.some((file) => file.path === run.config)) throw Object.assign(new Error('CI config not found in commit: ' + run.config), {status: 400});
    const bucket = repo.bucket;
    const object = String(repo.prefix || '').replace(/^\/+|\/+$/g, '') + '/_bgit-ci/run-' + run.id + '-' + run.commit + '.tar.gz';
    await storage.bucket(bucket).file(object).save(tarGz(files), {contentType: 'application/gzip'});
    const build = await startCloudBuild(repo, run, bucket, object);
    response(res, 200, {status: 'queued', url: build.logUrl || '', message: build.name || build.id || 'Cloud Build queued'});
  } catch (err) {
    response(res, err.status || 500, {error: err.message || String(err)});
  }
};

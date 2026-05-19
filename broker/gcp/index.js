'use strict';

const crypto = require('crypto');
const {Firestore} = require('@google-cloud/firestore');
const {Storage} = require('@google-cloud/storage');
const {GoogleAuth} = require('google-auth-library');

const db = new Firestore({databaseId: process.env.FIRESTORE_DATABASE || 'bgit'});
const repos = db.collection('bgit_broker_repos');
const members = db.collection('bgit_broker_members');
const storage = new Storage();
const auth = new GoogleAuth({scopes: ['https://www.googleapis.com/auth/cloud-platform']});
const brokerVersion = process.env.BROKER_VERSION || '{{BROKER_VERSION}}';
const zero = '0000000000000000000000000000000000000000';

function repoID(repo) {
  if (repo && repo.logical) return ['logical', repo.logical].join(':');
  return [repo.provider || 'gcs', repo.bucket, repo.prefix].join(':');
}

function docID(repo) {
  return Buffer.from(repoID(repo)).toString('base64url');
}

function cleanName(value) {
  return String(value || 'repo').toLowerCase().replace(/[^a-z0-9.-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40) || 'repo';
}

function randomSuffix() {
  return crypto.randomBytes(5).toString('hex');
}

async function loadRepo(repo) {
  const ref = repos.doc(docID(repo));
  const snap = await ref.get();
  if (!snap.exists) return {ref, data: {repo, keys: [], audit: []}};
  const data = snap.data() || {};
  data.repo = data.repo || repo;
  data.keys = data.keys || [];
  data.audit = data.audit || [];
  return {ref, data};
}

async function saveRepo(entry) {
  await entry.ref.set(entry.data, {merge: true});
  await syncMembershipIndex(entry);
}

async function syncMembershipIndex(entry) {
  const repo = entry.data.repo || {};
  if (!repo.logical && (!repo.bucket || !repo.prefix)) return;
  const repoIDValue = repoID(repo);
  const logical = repo.logical || repo.prefix || repoIDValue;
  const writes = [];
  for (const key of entry.data.keys || []) {
    if (!key.public_key) continue;
    const fingerprint = keyFingerprint(key.public_key);
    writes.push(members.doc(memberDocID(fingerprint)).collection('repos').doc(docID(repo)).set({
      repo_id: repoIDValue,
      logical,
      repo,
      user: key.user || '',
      role: key.role || '',
      source: key.source || '',
      suspended: !!key.suspended,
      updated_at: new Date().toISOString(),
    }, {merge: true}));
  }
  await Promise.all(writes);
}

async function syncAllMembershipIndexes() {
  const snap = await repos.get();
  const writes = [];
  let count = 0;
  snap.forEach((doc) => {
    if (doc.id === '_owners') return;
    const data = doc.data() || {};
    const repo = data.repo || {};
    if (!repo.logical && (!repo.bucket || !repo.prefix)) return;
    count++;
    writes.push(syncMembershipIndex({ref: doc.ref, data}));
  });
  await Promise.all(writes);
  return count;
}

function prDoc(entry, id) {
  return entry.ref.collection('prs').doc(String(id).padStart(10, '0'));
}

async function savePR(entry, pr) {
  await prDoc(entry, pr.id).set(pr, {merge: false});
}

async function loadPR(entry, id) {
  const snap = await prDoc(entry, id).get();
  if (!snap.exists) return null;
  return snap.data() || null;
}

async function listPRs(entry) {
  const snap = await entry.ref.collection('prs').orderBy('id', 'desc').get();
  return snap.docs.map((doc) => doc.data() || {});
}

async function syncPRRecords(entry, known) {
  const knownMap = known && typeof known === 'object' ? known : {};
  const prs = await listPRs(entry);
  const present = new Set(prs.map((pr) => String(pr.id)));
  const deleted = Object.keys(knownMap).filter((id) => !present.has(String(id))).map((id) => Number(id)).filter((id) => Number.isFinite(id));
  return {
    prs: prs.filter((pr) => String(pr.version || '') !== String(knownMap[String(pr.id)] || '')),
    deleted,
  };
}

async function loadOwners() {
  const ref = repos.doc('_owners');
  const snap = await ref.get();
  if (!snap.exists) return {ref, data: {keys: [], audit: []}};
  const data = snap.data() || {};
  data.keys = data.keys || [];
  data.audit = data.audit || [];
  return {ref, data};
}

function audit(entry, event) {
  entry.data.audit = entry.data.audit || [];
  entry.data.audit.push({...event, at: new Date().toISOString()});
  if (entry.data.audit.length > 500) entry.data.audit = entry.data.audit.slice(-500);
}

function readSSHString(buf, offset) {
  const len = buf.readUInt32BE(offset);
  const start = offset + 4;
  return {value: buf.subarray(start, start + len), offset: start + len};
}

function rawBody(req) {
  if (req.rawBody) return Buffer.from(req.rawBody);
  return Buffer.from(JSON.stringify(req.body || {}));
}

function expectedMessage(req) {
  const digest = crypto.createHash('sha256').update(rawBody(req)).digest('base64');
  return Buffer.from('bgit-broker-v1\n' + digest).toString('base64');
}

function normalizeKey(key) {
  return String(key || '').trim().split(/\s+/).slice(0, 2).join(' ');
}

function publicKeyObject(publicKey) {
  const parts = normalizeKey(publicKey).split(/\s+/);
  if (parts[0] !== 'ssh-ed25519') return crypto.createPublicKey(publicKey);
  const blob = Buffer.from(parts[1], 'base64');
  let parsed = readSSHString(blob, 0);
  const alg = parsed.value.toString();
  if (alg !== 'ssh-ed25519') throw new Error('unsupported SSH key algorithm');
  parsed = readSSHString(blob, parsed.offset);
  const derPrefix = Buffer.from('302a300506032b6570032100', 'hex');
  return crypto.createPublicKey({key: Buffer.concat([derPrefix, parsed.value]), format: 'der', type: 'spki'});
}

function signedKey(req, entry) {
  const keys = (entry.data.keys || []).filter((k) => !k.suspended);
  const publicKey = normalizeKey(req.get('x-bgit-key'));
  const message = String(req.get('x-bgit-signature-message') || '');
  const signature = String(req.get('x-bgit-signature') || '');
  if (!publicKey || !message || !signature || message !== expectedMessage(req)) return null;
  const key = keys.find((k) => normalizeKey(k.public_key) === publicKey);
  if (!key) return null;
  const parsed = readSSHString(Buffer.from(signature, 'base64'), 0);
  const alg = parsed.value.toString();
  const sig = readSSHString(Buffer.from(signature, 'base64'), parsed.offset).value;
  const verifyAlg = alg === 'ssh-ed25519' ? null : 'sha256';
  if (!crypto.verify(verifyAlg, Buffer.from(message, 'base64'), publicKeyObject(publicKey), sig)) return null;
  return key;
}

function submittedSignedKey(req) {
  const publicKey = normalizeKey(req.get('x-bgit-key'));
  const message = String(req.get('x-bgit-signature-message') || '');
  const signature = String(req.get('x-bgit-signature') || '');
  if (!publicKey || !message || !signature || message !== expectedMessage(req)) return null;
  const parsed = readSSHString(Buffer.from(signature, 'base64'), 0);
  const alg = parsed.value.toString();
  const sig = readSSHString(Buffer.from(signature, 'base64'), parsed.offset).value;
  const verifyAlg = alg === 'ssh-ed25519' ? null : 'sha256';
  if (!crypto.verify(verifyAlg, Buffer.from(message, 'base64'), publicKeyObject(publicKey), sig)) return null;
  return {public_key: publicKey, fingerprint: keyFingerprint(publicKey)};
}

function ownershipTransferCode(brokerURL, repo, token) {
  const payload = Buffer.from(JSON.stringify({broker_url: brokerURL, repo, token})).toString('base64url');
  return 'bgitot_' + payload;
}

function ownershipTransferTokenHash(token) {
  return crypto.createHash('sha256').update(String(token || '')).digest('hex');
}

function ownershipTransferExpired(transfer) {
  return !transfer || !transfer.expires_at || Date.parse(transfer.expires_at) <= Date.now();
}

function memberInviteCode(brokerURL, repo, token) {
  const payload = Buffer.from(JSON.stringify({broker_url: brokerURL, repo, token})).toString('base64url');
  return 'bgitinv_' + payload;
}

function verifySignature(req, entry) {
  const adminKeys = (entry.data.keys || []).filter((k) => (k.role === 'admin' || k.role === 'owner') && !k.suspended);
  if (adminKeys.length === 0) return true;
  const key = signedKey(req, entry);
  return !!key && (key.role === 'admin' || key.role === 'owner');
}

function roleAllows(role, operation) {
  if (role === 'owner') return true;
  if (role === 'admin') return true;
  if (operation === 'read') return ['read', 'triage', 'developer', 'maintainer'].includes(role);
  if (operation === 'write') return ['developer', 'maintainer'].includes(role);
  if (operation === 'merge') return ['maintainer'].includes(role);
  return false;
}

function keyFingerprint(publicKey) {
  const parts = String(publicKey || '').trim().split(/\s+/);
  const data = parts.length >= 2 ? Buffer.from(parts[1], 'base64') : Buffer.from(normalizeKey(publicKey));
  return 'SHA256:' + crypto.createHash('sha256').update(data).digest('base64').replace(/=+$/g, '');
}

function keyMatches(item, key) {
  const value = String(key || '').trim();
  if (!value) return false;
  const normalized = normalizeKey(value);
  return normalizeKey(item.public_key) === normalized ||
    item.public_key === value ||
    item.public_key.includes(value) ||
    keyFingerprint(item.public_key) === value;
}

function memberDocID(fingerprint) {
  return Buffer.from(String(fingerprint || '')).toString('base64url');
}

function roleCapabilities(role) {
  return {
    read: roleAllows(role, 'read'),
    push: roleAllows(role, 'write'),
    comment: ['owner', 'admin', 'maintainer', 'developer', 'triage'].includes(role),
    review: ['owner', 'admin', 'maintainer', 'developer', 'triage'].includes(role),
    approve: ['owner', 'admin', 'maintainer', 'triage'].includes(role),
    merge: roleAllows(role, 'merge'),
    admin_keys: role === 'owner' || role === 'admin',
    manage_protection: role === 'owner' || role === 'admin',
    reopen_pr: ['owner', 'admin', 'maintainer'].includes(role),
    owner_transfer: role === 'owner',
    broker_upgrade: role === 'owner' || role === 'admin',
  };
}

function anonymousKey() {
  return {user: 'anonymous', role: 'read', public_key: '', source: 'public', anonymous: true};
}

function repoIsPublic(entry) {
  return (entry.data.visibility || 'private') === 'public';
}

function repoIsReadOnly(entry) {
  return !!entry.data.read_only;
}

function validRole(role) {
  return ['owner', 'admin', 'maintainer', 'developer', 'triage', 'read'].includes(role);
}

function normalizeRole(role) {
  return role === 'write' ? 'developer' : role;
}

function requireAdmin(req, entry) {
  if (!verifySignature(req, entry)) {
    const err = new Error('admin SSH signature required');
    err.status = 403;
    throw err;
  }
}

function requireRead(req, entry) {
  const key = signedKey(req, entry);
  if (!key && repoIsPublic(entry)) return anonymousKey();
  if (!key || !roleAllows(key.role, 'read')) {
    const err = new Error('read SSH signature required');
    err.status = 403;
    throw err;
  }
  return key;
}

function requireWrite(req, entry) {
  if (repoIsReadOnly(entry)) {
    const err = new Error('repository is read-only');
    err.status = 403;
    throw err;
  }
  const key = signedKey(req, entry);
  if (!key || !roleAllows(key.role, 'write')) {
    const err = new Error('write SSH signature required');
    err.status = 403;
    throw err;
  }
  return key;
}

function requireIssueCreate(req, entry) {
  if (repoIsReadOnly(entry)) throw Object.assign(new Error('repository is read-only'), {status: 403});
  if (repoIsPublic(entry)) return signedKey(req, entry) || anonymousKey();
  return requireRead(req, entry);
}

function cleanObjectPath(value) {
  const path = String(value || '').replace(/^\/+/, '');
  if (path.includes('\0') || path.includes('..')) throw new Error('invalid object path');
  return path;
}

function objectName(repo, objectPath) {
  const prefix = String(repo.prefix || '').replace(/^\/+|\/+$/g, '');
  const path = cleanObjectPath(objectPath);
  return prefix ? prefix + '/' + path : path;
}

async function ensurePhysicalRepo(entry) {
  const repo = entry.data.repo || {};
  if (repo.bucket && repo.prefix) return repo;
  const logical = cleanName(repo.logical || repo.prefix || 'repo.git');
  const suffix = entry.data.bucket_suffix || randomSuffix();
  const bucket = `bgit-${logical.replace(/\.git$/, '')}-${suffix}`.slice(0, 63).replace(/\.+$/g, '');
  const prefix = 'repo.git';
  try {
    await storage.bucket(bucket).get({autoCreate: true});
  } catch (err) {
    await storage.createBucket(bucket);
  }
  entry.data.bucket_suffix = suffix;
  entry.data.repo = {...repo, provider: 'gcs', bucket, prefix};
  await saveRepo(entry);
  return entry.data.repo;
}

async function readObject(repo, objectPath) {
  const [data] = await storage.bucket(repo.bucket).file(objectName(repo, objectPath)).download();
  return data.toString('base64');
}

async function writeTextObject(repo, objectPath, value) {
  await storage.bucket(repo.bucket).file(objectName(repo, objectPath)).save(value);
}

async function deleteObject(repo, objectPath) {
  await storage.bucket(repo.bucket).file(objectName(repo, objectPath)).delete({ignoreNotFound: true});
}

async function deletePhysicalRepo(repo) {
  if (!repo.bucket) return;
  const bucket = storage.bucket(repo.bucket);
  const [files] = await bucket.getFiles();
  await Promise.all(files.map((file) => file.delete({ignoreNotFound: true})));
  try {
    await bucket.delete();
  } catch (err) {
    if (err && err.code !== 404) throw err;
  }
}

async function listObjects(repo, prefix) {
  const repoPrefix = String(repo.prefix || '').replace(/^\/+|\/+$/g, '');
  const queryPrefix = objectName(repo, prefix);
  const [files] = await storage.bucket(repo.bucket).getFiles({prefix: queryPrefix});
  const strip = repoPrefix ? repoPrefix + '/' : '';
  return files.map((file) => file.name.startsWith(strip) ? file.name.slice(strip.length) : file.name);
}

async function serviceAccountEmail() {
  if (process.env.BGIT_SIGNING_SERVICE_ACCOUNT) return process.env.BGIT_SIGNING_SERVICE_ACCOUNT;
  const client = await auth.getClient();
  const projectId = await auth.getProjectId();
  return `${projectId}@appspot.gserviceaccount.com`;
}

async function signedURL(repo, objectPath, operation) {
  const action = operation === 'write' ? 'write' : operation === 'delete' ? 'delete' : 'read';
  const method = action === 'write' ? 'PUT' : action === 'delete' ? 'DELETE' : 'GET';
  const file = storage.bucket(repo.bucket).file(objectName(repo, objectPath));
  const [url] = await file.getSignedUrl({
    version: 'v4',
    action,
    expires: Date.now() + 10 * 60 * 1000,
    method,
    virtualHostedStyle: false,
    extensionHeaders: action === 'write' ? {'content-type': 'application/octet-stream'} : undefined,
    cname: undefined,
    accessibleAt: undefined,
    signingEndpoint: 'https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/' + await serviceAccountEmail() + ':signBlob',
  });
  return {provider: 'gcs', mode: 'signed_url', method, url, headers: action === 'write' ? {'content-type': 'application/octet-stream'} : {}, expires_in: 600};
}

async function resumableUpload(repo, objectPath) {
  const [uri] = await storage.bucket(repo.bucket).file(objectName(repo, objectPath)).createResumableUpload({metadata: {contentType: 'application/octet-stream'}});
  return {provider: 'gcs', mode: 'resumable_upload', method: 'PUT', url: uri, headers: {}, expires_in: 600};
}

function protectionFor(data, ref) {
  return (data.protections || []).find((p) => p.ref === ref);
}

function assertRefAllowed(data, ref, key, opts) {
  const protection = protectionFor(data, ref);
  if (!protection || !protection.require_pr) return;
  if (opts && opts.fromPR) return;
  if (protection.allow_overrides && key && (key.role === 'owner' || key.role === 'admin')) return;
  const err = new Error(`protected branch ${ref} requires a pull request`);
  err.status = 403;
  throw err;
}

async function updateRefCAS(repo, ref, oldHash, newHash, key, opts = {}) {
  const id = docID(repo);
  const refDoc = repos.doc(id);
  await db.runTransaction(async (tx) => {
    const snap = await tx.get(refDoc);
    const data = snap.exists ? (snap.data() || {}) : {repo, keys: [], refs: {}, audit: []};
    data.repo = data.repo || repo;
    data.keys = data.keys || [];
    data.refs = data.refs || {};
    data.protections = data.protections || [];
    assertRefAllowed(data, ref, key, opts);
    const current = Object.prototype.hasOwnProperty.call(data.refs, ref) ? data.refs[ref] : oldHash;
    if (current !== oldHash) {
      const err = new Error('stale ref');
      err.status = 409;
      throw err;
    }
    if (newHash === zero) delete data.refs[ref];
    else data.refs[ref] = newHash;
    data.audit = (data.audit || []).concat([{type: 'ref_update', ref, old: oldHash, new: newHash, at: new Date().toISOString()}]).slice(-500);
    tx.set(refDoc, data, {merge: true});
  });
}

function nextPRID(data) {
  data.next_pr_id = Number(data.next_pr_id || 1);
  return data.next_pr_id++;
}

function nextIssueID(data) {
  data.next_issue_id = Number(data.next_issue_id || 1);
  return data.next_issue_id++;
}

function issueDoc(entry, id) {
  return entry.ref.collection('issues').doc(String(id).padStart(10, '0'));
}

async function saveIssue(entry, issue) {
  await issueDoc(entry, issue.id).set(issue, {merge: false});
}

async function loadIssue(entry, id) {
  const snap = await issueDoc(entry, id).get();
  if (!snap.exists) return null;
  return snap.data() || null;
}

async function listIssues(entry) {
  const snap = await entry.ref.collection('issues').orderBy('id', 'desc').get();
  return snap.docs.map((doc) => doc.data() || {});
}

async function deleteRepoMetadata(entry) {
  const prSnap = await entry.ref.collection('prs').get();
  const issueSnap = await entry.ref.collection('issues').get();
  const deletes = [];
  prSnap.forEach((doc) => deletes.push(doc.ref.delete()));
  issueSnap.forEach((doc) => deletes.push(doc.ref.delete()));
  const repo = entry.data.repo || {};
  const oldRepoDocID = docID(repo);
  for (const key of entry.data.keys || []) {
    if (!key.public_key) continue;
    deletes.push(members.doc(memberDocID(keyFingerprint(key.public_key))).collection('repos').doc(oldRepoDocID).delete());
  }
  deletes.push(entry.ref.delete());
  await Promise.all(deletes);
}

async function moveRepoSubcollections(oldEntry, newRef) {
  const copies = [];
  for (const collectionName of ['prs', 'issues']) {
    const snap = await oldEntry.ref.collection(collectionName).get();
    snap.forEach((doc) => {
      copies.push(newRef.collection(collectionName).doc(doc.id).set(doc.data() || {}, {merge: false}));
    });
  }
  await Promise.all(copies);
}

async function deleteMembershipIndex(entry) {
  const repoDocID = docID(entry.data.repo || {});
  const deletes = [];
  for (const key of entry.data.keys || []) {
    if (!key.public_key) continue;
    deletes.push(members.doc(memberDocID(keyFingerprint(key.public_key))).collection('repos').doc(repoDocID).delete());
  }
  await Promise.all(deletes);
}

function findPR(data, id) {
  return (data.prs || []).find((pr) => Number(pr.id) === Number(id));
}

function nextPRNoteID(pr) {
  pr.next_note_id = Number(pr.next_note_id || 1);
  return pr.next_note_id++;
}

function nextPRCommentID(pr) {
  pr.next_comment_id = Number(pr.next_comment_id || 1);
  return pr.next_comment_id++;
}

function hashLineText(value) {
  return crypto.createHash('sha1').update(String(value || '')).digest('hex');
}

function normalizeReviewComments(pr, comments, key, head) {
  if (!Array.isArray(comments)) return [];
  const now = new Date().toISOString();
  return comments.map((comment) => {
    const body = String(comment.body || '').trim();
    if (!body) return null;
    const lineText = String(comment.line_text || '');
    return {
      id: nextPRCommentID(pr),
      user: key.user,
      body,
      file: String(comment.file || '').trim(),
      kind: String(comment.kind || 'line').trim(),
      side: String(comment.side || 'new').trim(),
      hunk: String(comment.hunk || '').trim(),
      hunk_index: Number(comment.hunk_index || 0),
      old_start: Number(comment.old_start || 0),
      new_start: Number(comment.new_start || 0),
      offset: Number(comment.offset || 0),
      line: Number(comment.line || 0),
      line_text: lineText,
      line_hash: String(comment.line_hash || hashLineText(lineText)),
      head: String(comment.head || head || pr.head || ''),
      at: now,
    };
  }).filter(Boolean);
}

function findPRComment(comments, id) {
  if (!Array.isArray(comments) || !id) return null;
  for (const comment of comments) {
    if (Number(comment.id) === Number(id)) return comment;
    const nested = findPRComment(comment.replies || [], id);
    if (nested) return nested;
  }
  return null;
}

function findPRReplyTarget(pr, noteID, commentID) {
  const notes = [...(pr.comments || []), ...(pr.reviews || [])];
  for (const note of notes) {
    if (commentID) {
      const inline = findPRComment(note.comments || [], commentID);
      if (inline) return inline;
      const reply = findPRComment(note.replies || [], commentID);
      if (reply) return reply;
    }
    if (noteID && Number(note.id) === Number(noteID)) return note;
  }
  return null;
}

function bumpPRVersion(data, pr) {
  const now = new Date().toISOString();
  data.next_pr_version = Number(data.next_pr_version || 1);
  pr.version = `${data.next_pr_version++}-${crypto.randomBytes(4).toString('hex')}`;
  pr.updated_at = now;
  return pr;
}

function ensurePRVersions(data) {
  let changed = false;
  for (const pr of data.prs || []) {
    if (!pr.version) {
      bumpPRVersion(data, pr);
      changed = true;
    }
  }
  return changed;
}

function syncPRs(data, known) {
  const knownMap = known && typeof known === 'object' ? known : {};
  return (data.prs || []).filter((pr) => String(pr.version || '') !== String(knownMap[String(pr.id)] || ''));
}

function countApprovals(pr) {
  const latest = new Map();
  for (const review of pr.reviews || []) {
    if (review.user) latest.set(review.user, review.state);
  }
  return Array.from(latest.values()).filter((state) => state === 'approved').length;
}

async function ensureRepo(repo) {
  if (!repo || (!repo.logical && (!repo.bucket || !repo.prefix))) throw new Error('repo is required');
  const entry = await loadRepo(repo);
  const owners = await loadOwners();
  for (const owner of owners.data.keys || []) {
    if (owner.role === 'owner' && !entry.data.keys.find((k) => normalizeKey(k.public_key) === normalizeKey(owner.public_key))) {
      entry.data.keys.push(owner);
    }
  }
  return entry;
}

exports.broker = async (req, res) => {
  res.set('content-type', 'application/json');
  if (req.path === '/health' || req.path === '/') {
    res.status(200).send(JSON.stringify({ok: true, service: 'bgit-broker', version: brokerVersion}));
    return;
  }
  try {
    const body = req.body || {};
    if (req.path === '/owners/upsert' && req.method === 'POST') {
      const entry = await loadOwners();
      if (!verifySignature(req, entry)) throw Object.assign(new Error('owner SSH signature required'), {status: 403});
      const user = body.user || 'owner';
      const role = normalizeRole(body.role || 'owner');
      if (role !== 'owner') throw new Error('owner bootstrap only accepts owner role');
      for (const publicKey of body.public_keys || []) {
        if (!entry.data.keys.find((k) => normalizeKey(k.public_key) === normalizeKey(publicKey))) entry.data.keys.push({user, role, public_key: publicKey, source: body.source || '', suspended: false});
      }
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/repos/upsert' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      const user = body.admin_user || 'admin';
      const role = normalizeRole(body.role || 'admin');
      if (!validRole(role)) throw new Error('invalid role');
      entry.data.repo = {...(entry.data.repo || {}), ...(body.repo || {})};
      if (body.repo && body.repo.logical && !entry.data.repo.bucket) await ensurePhysicalRepo(entry);
      for (const publicKey of body.public_keys || []) {
        if (!entry.data.keys.find((k) => normalizeKey(k.public_key) === normalizeKey(publicKey))) entry.data.keys.push({user, role, public_key: publicKey, source: body.source || '', suspended: false});
      }
      audit(entry, {type: 'repo_upsert', user});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, repo: entry.data.repo, bucket_suffix: entry.data.bucket_suffix}));
      return;
    }
    if (req.path === '/repo/info' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      res.status(200).send(JSON.stringify({
        repo: entry.data.repo || body.repo,
        description: entry.data.description || '',
        default_branch: entry.data.default_branch || 'main',
        visibility: entry.data.visibility || 'private',
        read_only: !!entry.data.read_only,
        issues_enabled: entry.data.issues_enabled !== false,
      }));
      return;
    }
    if (req.path === '/repo/update' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      if (Object.prototype.hasOwnProperty.call(body, 'description')) entry.data.description = String(body.description || '').trim();
      if (Object.prototype.hasOwnProperty.call(body, 'default_branch')) entry.data.default_branch = String(body.default_branch || '').trim() || 'main';
      if (Object.prototype.hasOwnProperty.call(body, 'visibility')) entry.data.visibility = body.visibility === 'public' ? 'public' : 'private';
      if (Object.prototype.hasOwnProperty.call(body, 'read_only')) entry.data.read_only = !!body.read_only;
      if (Object.prototype.hasOwnProperty.call(body, 'issues_enabled')) entry.data.issues_enabled = body.issues_enabled !== false;
      audit(entry, {type: 'repo_update'});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({
        ok: true,
        repo: entry.data.repo || body.repo,
        description: entry.data.description,
        default_branch: entry.data.default_branch,
        visibility: entry.data.visibility,
        read_only: !!entry.data.read_only,
        issues_enabled: entry.data.issues_enabled !== false,
      }));
      return;
    }
    if (req.path === '/repo/rename' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = signedKey(req, entry);
      if (!key || key.role !== 'owner') throw Object.assign(new Error('owner SSH signature required'), {status: 403});
      const logical = String(body.logical || '').trim().replace(/^\/+|\/+$/g, '');
      if (!logical) throw new Error('logical repo name is required');
      const newRepo = {...(entry.data.repo || body.repo), logical};
      const newRef = repos.doc(docID(newRepo));
      const oldID = docID(entry.data.repo || body.repo);
      const newID = docID(newRepo);
      if (oldID !== newID && (await newRef.get()).exists) throw Object.assign(new Error('target logical repo already exists'), {status: 409});
      entry.data.repo = newRepo;
      audit(entry, {type: 'repo_rename', logical, user: key.user});
      await newRef.set(entry.data, {merge: false});
      if (oldID !== newID) {
        await moveRepoSubcollections({ref: entry.ref, data: {...entry.data, repo: body.repo}}, newRef);
        await deleteMembershipIndex({data: {...entry.data, repo: body.repo}});
        await entry.ref.delete();
      }
      await syncMembershipIndex({ref: newRef, data: entry.data});
      res.status(200).send(JSON.stringify({ok: true, repo: newRepo}));
      return;
    }
    if (req.path === '/repo/delete' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = signedKey(req, entry);
      if (!key || key.role !== 'owner') throw Object.assign(new Error('owner SSH signature required'), {status: 403});
      const repo = await ensurePhysicalRepo(entry);
      await deletePhysicalRepo(repo);
      await deleteRepoMetadata(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/keys/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      res.status(200).send(JSON.stringify({keys: entry.data.keys}));
      return;
    }
    if (req.path === '/keys/add' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      const user = body.user || 'admin';
      const role = normalizeRole(body.role || 'read');
      if (!validRole(role) || role === 'owner') throw new Error('invalid role');
      for (const publicKey of body.public_keys || []) {
        if (!entry.data.keys.find((k) => normalizeKey(k.public_key) === normalizeKey(publicKey))) entry.data.keys.push({user, role, public_key: publicKey, source: body.source || '', suspended: false});
      }
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/keys/invite/create' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      const user = String(body.user || '').trim();
      const role = normalizeRole(body.role || 'read');
      if (!user) throw new Error('user is required');
      if (!validRole(role) || role === 'owner') throw new Error('invalid role');
      const token = crypto.randomBytes(24).toString('base64url');
      const brokerURL = String(body.broker_url || '').trim();
      const expires = new Date(Date.now() + 7 * 24 * 60 * 60 * 1000).toISOString();
      entry.data.invites = (entry.data.invites || []).filter((invite) => Date.parse(invite.expires_at || '') > Date.now());
      entry.data.invites.push({token_hash: ownershipTransferTokenHash(token), user, role, broker_url: brokerURL, expires_at: expires});
      const code = memberInviteCode(brokerURL, entry.data.repo || body.repo, token);
      audit(entry, {type: 'member_invite_create', user, role});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, code, accept_command: 'bgit admin accept-invite ' + code}));
      return;
    }
    if (req.path === '/keys/invite/accept' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const signed = submittedSignedKey(req);
      if (!signed) throw Object.assign(new Error('SSH signature required'), {status: 403});
      const tokenHash = ownershipTransferTokenHash(body.token);
      const invites = entry.data.invites || [];
      const invite = invites.find((item) => item.token_hash === tokenHash && Date.parse(item.expires_at || '') > Date.now());
      if (!invite) throw Object.assign(new Error('invite is not pending or has expired'), {status: 404});
      const existing = (entry.data.keys || []).find((item) => normalizeKey(item.public_key) === normalizeKey(signed.public_key));
      if (existing) {
        existing.user = invite.user;
        existing.role = invite.role;
        existing.suspended = false;
      } else {
        entry.data.keys = entry.data.keys || [];
        entry.data.keys.push({user: invite.user, role: invite.role, public_key: signed.public_key, source: 'invite', suspended: false});
      }
      entry.data.invites = invites.filter((item) => item.token_hash !== tokenHash);
      audit(entry, {type: 'member_invite_accept', user: invite.user, role: invite.role, fingerprint: signed.fingerprint});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, user: invite.user, role: invite.role, fingerprint: signed.fingerprint}));
      return;
    }
    if ((req.path === '/keys/remove' || req.path === '/keys/suspend') && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      const key = String(body.key || '').trim();
      const match = (k) => keyMatches(k, key);
      if (entry.data.keys.some((k) => match(k) && k.role === 'owner')) throw Object.assign(new Error('owners cannot be removed or suspended'), {status: 403});
      let changed = false;
      if (req.path === '/keys/remove') {
        const before = entry.data.keys.length;
        entry.data.keys = entry.data.keys.filter((k) => !match(k));
        changed = entry.data.keys.length !== before;
      } else {
        for (const item of entry.data.keys) {
          if (match(item)) {
            item.suspended = true;
            changed = true;
          }
        }
      }
      if (!changed) throw Object.assign(new Error('key not found'), {status: 404});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/keys/unsuspend' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      const key = String(body.key || '').trim();
      const match = (k) => keyMatches(k, key);
      let changed = false;
      for (const item of entry.data.keys || []) {
        if (match(item)) {
          item.suspended = false;
          changed = true;
        }
      }
      if (!changed) throw Object.assign(new Error('key not found'), {status: 404});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/owners/transfer/confirm' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = signedKey(req, entry);
      if (!key || key.role !== 'owner') throw Object.assign(new Error('owner SSH signature required'), {status: 403});
      if (entry.data.owner_transfer && !ownershipTransferExpired(entry.data.owner_transfer)) {
        throw Object.assign(new Error('ownership transfer already pending; run bgit admin cancel-ownership-transfer to cancel it'), {status: 409});
      }
      const token = crypto.randomBytes(24).toString('base64url');
      const brokerURL = String(body.broker_url || '').trim();
      const expires = new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString();
      entry.data.owner_transfer = {
        token_hash: ownershipTransferTokenHash(token),
        requested_by: key.user || '',
        requested_by_fingerprint: keyFingerprint(key.public_key),
        broker_url: brokerURL,
        expires_at: expires,
      };
      const code = ownershipTransferCode(brokerURL, entry.data.repo || body.repo, token);
      audit(entry, {type: 'owner_transfer_confirm', user: key.user || '', expires_at: expires});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, code, accept_command: 'bgit admin accept-ownership-transfer ' + code, cancel_command: 'bgit admin cancel-ownership-transfer --broker ' + brokerURL + ' ' + ((entry.data.repo || body.repo).logical || '')}));
      return;
    }
    if (req.path === '/owners/transfer/cancel' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = signedKey(req, entry);
      if (!key || key.role !== 'owner') throw Object.assign(new Error('owner SSH signature required'), {status: 403});
      delete entry.data.owner_transfer;
      audit(entry, {type: 'owner_transfer_cancel', user: key.user || ''});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/owners/transfer/accept' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const transfer = entry.data.owner_transfer;
      if (!transfer || ownershipTransferExpired(transfer)) throw Object.assign(new Error('ownership transfer is not pending or has expired'), {status: 404});
      if (ownershipTransferTokenHash(body.token) !== transfer.token_hash) throw Object.assign(new Error('ownership transfer code is invalid'), {status: 403});
      const accepted = submittedSignedKey(req);
      if (!accepted) throw Object.assign(new Error('SSH signature required'), {status: 403});
      const user = String(body.user || 'owner').trim() || 'owner';
      const ownerFingerprint = transfer.requested_by_fingerprint || '';
      for (const item of entry.data.keys || []) {
        if (item.role === 'owner' && keyFingerprint(item.public_key) === ownerFingerprint) item.role = 'admin';
      }
      const existing = (entry.data.keys || []).find((item) => normalizeKey(item.public_key) === normalizeKey(accepted.public_key));
      if (existing) {
        existing.role = 'owner';
        existing.user = user;
        existing.suspended = false;
      } else {
        entry.data.keys = entry.data.keys || [];
        entry.data.keys.push({user, role: 'owner', public_key: accepted.public_key, source: 'ownership-transfer', suspended: false});
      }
      delete entry.data.owner_transfer;
      audit(entry, {type: 'owner_transfer_accept', user, fingerprint: accepted.fingerprint});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, user, fingerprint: accepted.fingerprint}));
      return;
    }
    if (req.path === '/protection/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      res.status(200).send(JSON.stringify({protections: entry.data.protections || []}));
      return;
    }
    if (req.path === '/protection/upsert' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      entry.data.protections = (entry.data.protections || []).filter((p) => p.ref !== body.ref);
      entry.data.protections.push({ref: body.ref, require_pr: body.require_pr !== false, allow_overrides: !!body.allow_overrides});
      audit(entry, {type: 'protection_upsert', ref: body.ref});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/protection/remove' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireAdmin(req, entry);
      entry.data.protections = (entry.data.protections || []).filter((p) => p.ref !== body.ref);
      audit(entry, {type: 'protection_remove', ref: body.ref});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/issues/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      if (entry.data.issues_enabled === false) throw Object.assign(new Error('issues are disabled'), {status: 403});
      const issues = await listIssues(entry);
      res.status(200).send(JSON.stringify({issues}));
      return;
    }
    if (req.path === '/issues/view' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      if (entry.data.issues_enabled === false) throw Object.assign(new Error('issues are disabled'), {status: 403});
      const issue = await loadIssue(entry, body.id);
      if (!issue) throw Object.assign(new Error('issue not found'), {status: 404});
      res.status(200).send(JSON.stringify({issue}));
      return;
    }
    if (req.path === '/issues/create' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      if (entry.data.issues_enabled === false) throw Object.assign(new Error('issues are disabled'), {status: 403});
      const key = requireIssueCreate(req, entry);
      const title = String(body.title || '').trim();
      const issueBody = String(body.body || '').trim();
      if (!title) throw new Error('issue title is required');
      const issue = {id: nextIssueID(entry.data), title, body: issueBody, status: 'open', author: key.user || 'anonymous', comments: [], created_at: new Date().toISOString(), updated_at: new Date().toISOString()};
      await saveRepo(entry);
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/issues/comment' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      if (entry.data.issues_enabled === false) throw Object.assign(new Error('issues are disabled'), {status: 403});
      const key = requireIssueCreate(req, entry);
      const issue = await loadIssue(entry, body.id);
      if (!issue) throw Object.assign(new Error('issue not found'), {status: 404});
      const comment = String(body.comment || '').trim();
      if (!comment) throw new Error('comment is required');
      issue.comments = issue.comments || [];
      issue.comments.push({user: key.user || 'anonymous', body: comment, at: new Date().toISOString()});
      issue.updated_at = new Date().toISOString();
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/issues/close' || req.path === '/issues/reopen') {
      const entry = await ensureRepo(body.repo);
      requireWrite(req, entry);
      const issue = await loadIssue(entry, body.id);
      if (!issue) throw Object.assign(new Error('issue not found'), {status: 404});
      issue.status = req.path === '/issues/reopen' ? 'open' : 'closed';
      issue.updated_at = new Date().toISOString();
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/prs/create' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = requireWrite(req, entry);
      entry.data.refs = entry.data.refs || {};
      const pr = {...(body.pr || {})};
      pr.id = nextPRID(entry.data);
      pr.status = 'open';
      pr.author = key.user;
      pr.approvals = pr.approvals || 0;
      pr.checks = pr.checks || [];
      pr.head = entry.data.refs[pr.source] || '';
      bumpPRVersion(entry.data, pr);
      audit(entry, {type: 'pr_create', id: pr.id, source: pr.source, target: pr.target, user: key.user});
      await saveRepo(entry);
      await savePR(entry, pr);
      res.status(200).send(JSON.stringify({pr}));
      return;
    }
    if (req.path === '/prs/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      res.status(200).send(JSON.stringify({prs: await listPRs(entry)}));
      return;
    }
    if (req.path === '/prs/sync' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      res.status(200).send(JSON.stringify(await syncPRRecords(entry, body.known || {})));
      return;
    }
    if (req.path === '/prs/view' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      const pr = await loadPR(entry, body.id);
      if (!pr) throw Object.assign(new Error('pull request not found'), {status: 404});
      res.status(200).send(JSON.stringify({pr}));
      return;
    }
    if (req.path === '/prs/close' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = requireWrite(req, entry);
      const pr = await loadPR(entry, body.id);
      if (!pr) throw Object.assign(new Error('pull request not found'), {status: 404});
      pr.status = 'closed';
      pr.closed_by = key.user;
      pr.closed_at = new Date().toISOString();
      bumpPRVersion(entry.data, pr);
      audit(entry, {type: 'pr_close', id: pr.id, user: key.user});
      await saveRepo(entry);
      await savePR(entry, pr);
      res.status(200).send(JSON.stringify({ok: true, pr}));
      return;
    }
    if (req.path === '/prs/reopen' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = requireWrite(req, entry);
      const pr = await loadPR(entry, body.id);
      if (!pr) throw Object.assign(new Error('pull request not found'), {status: 404});
      pr.status = 'open';
      delete pr.closed_by;
      delete pr.closed_at;
      bumpPRVersion(entry.data, pr);
      audit(entry, {type: 'pr_reopen', id: pr.id, user: key.user});
      await saveRepo(entry);
      await savePR(entry, pr);
      res.status(200).send(JSON.stringify({ok: true, pr}));
      return;
    }
    if (req.path === '/prs/comment' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      if (repoIsReadOnly(entry)) throw Object.assign(new Error('repository is read-only'), {status: 403});
      const key = requireRead(req, entry);
      const pr = await loadPR(entry, body.id);
      if (!pr) throw Object.assign(new Error('pull request not found'), {status: 404});
      const comment = String(body.comment || '').trim();
      if (!comment) throw Object.assign(new Error('comment is required'), {status: 400});
      pr.comments = pr.comments || [];
      pr.comments.push({id: nextPRNoteID(pr), user: key.user, body: comment, at: new Date().toISOString()});
      bumpPRVersion(entry.data, pr);
      audit(entry, {type: 'pr_comment', id: pr.id, user: key.user});
      await saveRepo(entry);
      await savePR(entry, pr);
      res.status(200).send(JSON.stringify({ok: true, pr}));
      return;
    }
    if (req.path === '/prs/reply' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      if (repoIsReadOnly(entry)) throw Object.assign(new Error('repository is read-only'), {status: 403});
      const key = requireRead(req, entry);
      const pr = await loadPR(entry, body.id);
      if (!pr) throw Object.assign(new Error('pull request not found'), {status: 404});
      const comment = String(body.comment || '').trim();
      if (!comment) throw Object.assign(new Error('comment is required'), {status: 400});
      const target = findPRReplyTarget(pr, Number(body.target_note_id || 0), Number(body.target_comment_id || 0));
      if (!target) throw Object.assign(new Error('reply target not found'), {status: 404});
      target.replies = target.replies || [];
      target.replies.push({id: nextPRCommentID(pr), user: key.user, body: comment, kind: 'reply', at: new Date().toISOString()});
      bumpPRVersion(entry.data, pr);
      audit(entry, {type: 'pr_reply', id: pr.id, user: key.user});
      await saveRepo(entry);
      await savePR(entry, pr);
      res.status(200).send(JSON.stringify({ok: true, pr}));
      return;
    }
    if (req.path === '/prs/review' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = requireWrite(req, entry);
      const pr = await loadPR(entry, body.id);
      if (!pr) throw Object.assign(new Error('pull request not found'), {status: 404});
      const state = String(body.review || '').trim();
      if (!['commented', 'approved', 'changes_requested'].includes(state)) throw Object.assign(new Error('unsupported review state'), {status: 400});
      pr.reviews = pr.reviews || [];
      const comments = normalizeReviewComments(pr, body.comments, key, pr.head);
      pr.reviews.push({id: nextPRNoteID(pr), user: key.user, body: String(body.comment || '').trim(), state, comments, head: String(pr.head || ''), at: new Date().toISOString()});
      pr.approvals = countApprovals(pr);
      bumpPRVersion(entry.data, pr);
      audit(entry, {type: 'pr_review', id: pr.id, user: key.user, state});
      await saveRepo(entry);
      await savePR(entry, pr);
      res.status(200).send(JSON.stringify({ok: true, pr}));
      return;
    }
    if (req.path === '/prs/merge' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      if (repoIsReadOnly(entry)) throw Object.assign(new Error('repository is read-only'), {status: 403});
      const key = signedKey(req, entry);
      if (!key || !roleAllows(key.role, 'merge')) throw Object.assign(new Error('merge SSH signature required'), {status: 403});
      const pr = await loadPR(entry, body.id);
      if (!pr) throw Object.assign(new Error('pull request not found'), {status: 404});
      if (pr.status !== 'open') throw new Error('pull request is not open');
      const newHash = (entry.data.refs || {})[pr.source] || pr.head;
      if (!newHash) throw new Error('pull request source ref has no head');
      const oldHash = (entry.data.refs || {})[pr.target] || zero;
      await updateRefCAS(body.repo, pr.target, oldHash, newHash, key, {fromPR: true});
      const repo = await ensurePhysicalRepo(entry);
      await writeTextObject(repo, pr.target, newHash + '\n');
      entry.data.refs = entry.data.refs || {};
      entry.data.refs[pr.target] = newHash;
      pr.status = 'merged';
      pr.merged_by = key.user;
      pr.merged_at = new Date().toISOString();
      bumpPRVersion(entry.data, pr);
      if (body.delete_branch && pr.source && pr.source !== pr.target) {
        delete entry.data.refs[pr.source];
        await deleteObject(repo, pr.source);
        audit(entry, {type: 'branch_delete', ref: pr.source, from_pr: pr.id, user: key.user});
      }
      await saveRepo(entry);
      await savePR(entry, pr);
      res.status(200).send(JSON.stringify({ok: true, pr}));
      return;
    }
    if (req.path === '/auth/check' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = signedKey(req, entry);
      const operation = body.operation || '';
      const allowed = (operation === 'read' && repoIsPublic(entry)) || (!!key && roleAllows(key.role, operation));
      res.status(200).send(JSON.stringify({allowed, user: key && key.user || (allowed ? 'anonymous' : ''), role: key && key.role || (allowed ? 'read' : '')}));
      return;
    }
    if (req.path === '/auth/status' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = signedKey(req, entry) || (repoIsPublic(entry) ? anonymousKey() : null);
      if (!key) throw Object.assign(new Error('SSH signature required'), {status: 403});
      res.status(200).send(JSON.stringify({
        broker_version: brokerVersion,
        repo: entry.data.repo || body.repo,
        identity: {user: key.user || '', source: key.source || '', key_fingerprint: key.public_key ? keyFingerprint(key.public_key) : '', public_key: key.public_key || ''},
        user: key.user || '',
        role: key.role || '',
        capabilities: roleCapabilities(key.role || ''),
        resolved_at: new Date().toISOString(),
      }));
      return;
    }
    if (req.path === '/repos/mine' && req.method === 'POST') {
      const fingerprint = req.get('x-bgit-key-fingerprint') || '';
      if (!fingerprint) throw Object.assign(new Error('SSH signature required'), {status: 403});
      const snap = await members.doc(memberDocID(fingerprint)).collection('repos').get();
      const out = [];
      snap.forEach((doc) => {
        const item = doc.data() || {};
        if (!item.suspended && item.repo && (item.repo.logical || (item.repo.bucket && item.repo.prefix))) out.push({...item, key_fingerprint: fingerprint});
      });
      out.sort((a, b) => String(a.logical || a.repo_id || '').localeCompare(String(b.logical || b.repo_id || '')));
      res.status(200).send(JSON.stringify({repos: out}));
      return;
    }
    if (req.path === '/members/reindex' && req.method === 'POST') {
      if (body.repo && (body.repo.logical || body.repo.bucket || body.repo.prefix)) {
        const entry = await ensureRepo(body.repo);
        requireAdmin(req, entry);
        await syncMembershipIndex(entry);
        res.status(200).send(JSON.stringify({ok: true, repositories: 1}));
        return;
      }
      const owners = await loadOwners();
      if (!verifySignature(req, owners)) throw Object.assign(new Error('owner SSH signature required'), {status: 403});
      const count = await syncAllMembershipIndexes();
      res.status(200).send(JSON.stringify({ok: true, repositories: count}));
      return;
    }
    if (req.path === '/objects/capability' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const operation = body.operation || 'read';
      const key = operation === 'read' ? requireRead(req, entry) : requireWrite(req, entry);
      const repo = await ensurePhysicalRepo(entry);
      const capability = body.resumable ? await resumableUpload(repo, body.path) : await signedURL(repo, body.path, operation);
      audit(entry, {type: 'capability_issued', operation, path: body.path, user: key.user, role: key.role});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({...capability, bucket: repo.bucket, prefix: repo.prefix, object: objectName(repo, body.path)}));
      return;
    }
    if (req.path === '/objects/read' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      const repo = await ensurePhysicalRepo(entry);
      const data = await readObject(repo, body.path);
      res.status(200).send(JSON.stringify({data}));
      return;
    }
    if (req.path === '/objects/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      requireRead(req, entry);
      const repo = await ensurePhysicalRepo(entry);
      const paths = await listObjects(repo, body.prefix);
      res.status(200).send(JSON.stringify({paths}));
      return;
    }
    if (req.path === '/refs/update' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = requireWrite(req, entry);
      await updateRefCAS(body.repo, body.ref, body.old, body.new, key, {override: !!body.override});
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    res.status(404).send(JSON.stringify({error: 'unknown broker endpoint'}));
  } catch (err) {
    res.status(err.status || 500).send(JSON.stringify({error: err.message || String(err)}));
  }
};

'use strict';

const crypto = require('crypto');
const {Firestore} = require('@google-cloud/firestore');
const {Storage} = require('@google-cloud/storage');
const {GoogleAuth} = require('google-auth-library');

const db = new Firestore({databaseId: process.env.FIRESTORE_DATABASE || 'bgit'});
const repos = db.collection('bgit_broker_repos');
const members = db.collection('bgit_broker_members');
const nonces = db.collection('bgit_broker_nonces');
const storage = new Storage();
const auth = new GoogleAuth({scopes: ['https://www.googleapis.com/auth/cloud-platform']});
const brokerVersion = process.env.BROKER_VERSION || '{{BROKER_VERSION}}';
const zero = '0000000000000000000000000000000000000000';
const coreTeamID = 't_core';
const coreTeamName = 'core';

function repoID(repo) {
  if (repo && repo.logical) return ['logical', repo.team_id || coreTeamID, repo.logical].join(':');
  return [repo.provider || 'gcs', repo.bucket, repo.prefix].join(':');
}

function legacyRepoID(repo) {
  if (repo && repo.logical) return ['logical', repo.logical].join(':');
  return [repo.provider || 'gcs', repo.bucket, repo.prefix].join(':');
}

function docID(repo) {
  return Buffer.from(repoID(repo)).toString('base64url');
}

function legacyDocID(repo) {
  return Buffer.from(legacyRepoID(repo)).toString('base64url');
}

function cleanName(value) {
  return String(value || 'repo').toLowerCase().replace(/[^a-z0-9.-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40) || 'repo';
}

function normalizeLogicalRepo(value) {
  const base = String(value || '').trim().replace(/\.git$/, '');
  if (!base) throw new Error('logical repo name is required');
  if (base.includes('/') || base.includes('\\')) throw new Error('logical repo names must be flat');
  if (base === '.' || base === '..') throw new Error('logical repo name is invalid');
  return `${base}.git`;
}

function validateRepo(repo) {
  if (!repo || (!repo.logical && (!repo.bucket || !repo.prefix))) throw new Error('repo is required');
  if (repo.logical) repo.logical = normalizeLogicalRepo(repo.logical);
  if (repo.logical && !repo.team_id) repo.team_id = coreTeamID;
  return repo;
}

function randomSuffix() {
  return crypto.randomBytes(5).toString('hex');
}

async function loadRepo(repo) {
  const hadLogicalNoTeam = !!(repo && repo.logical && !repo.team_id);
  repo = validateRepo(repo);
  const ref = repos.doc(docID(repo));
  const snap = await ref.get();
  if (!snap.exists && (hadLogicalNoTeam || repo.team_id === coreTeamID)) {
    const legacyRef = repos.doc(legacyDocID(repo));
    const legacySnap = await legacyRef.get();
    if (legacySnap.exists) {
      const legacyData = legacySnap.data() || {};
      legacyData.repo = {...(legacyData.repo || repo), team_id: coreTeamID};
      legacyData.keys = legacyData.keys || [];
      legacyData.audit = legacyData.audit || [];
      return {ref: legacyRef, data: legacyData};
    }
  }
  if (!snap.exists) return {ref, data: {repo, keys: [], audit: []}};
  const data = snap.data() || {};
  data.repo = data.repo || repo;
  if (data.repo.logical && !data.repo.team_id) data.repo.team_id = coreTeamID;
  data.keys = data.keys || [];
  data.audit = data.audit || [];
  return {ref, data};
}

async function loadExistingRepo(repo) {
  const hadLogicalNoTeam = !!(repo && repo.logical && !repo.team_id);
  repo = validateRepo(repo);
  const ref = repos.doc(docID(repo));
  const snap = await ref.get();
  if (snap.exists) {
    const data = snap.data() || {};
    data.repo = data.repo || repo;
    if (data.repo.logical && !data.repo.team_id) data.repo.team_id = coreTeamID;
    data.keys = data.keys || [];
    data.audit = data.audit || [];
    return {ref, data};
  }
  if (hadLogicalNoTeam || repo.team_id === coreTeamID) {
    const legacyRef = repos.doc(legacyDocID(repo));
    const legacySnap = await legacyRef.get();
    if (legacySnap.exists) {
      const legacyData = legacySnap.data() || {};
      legacyData.repo = {...(legacyData.repo || repo), team_id: coreTeamID};
      legacyData.keys = legacyData.keys || [];
      legacyData.audit = legacyData.audit || [];
      return {ref: legacyRef, data: legacyData};
    }
  }
  return null;
}

function mergeCIState(current, next) {
  current = current || {};
  next = next || {};
  const byID = new Map();
  function add(run) {
    if (!run || run.id === undefined || run.id === null) return;
    const id = Number(run.id);
    const existing = byID.get(id);
    if (!existing) {
      byID.set(id, run);
      return;
    }
    const existingTime = Date.parse(existing.updated_at || existing.created_at || '') || 0;
    const runTime = Date.parse(run.updated_at || run.created_at || '') || 0;
    if (runTime >= existingTime) byID.set(id, run);
  }
  for (const run of current.ci_runs || []) add(run);
  for (const run of next.ci_runs || []) add(run);
  next.ci_runs = [...byID.values()].sort((a, b) => Number(b.id || 0) - Number(a.id || 0)).slice(0, 100);
  next.next_ci_id = Math.max(Number(current.next_ci_id || 1), Number(next.next_ci_id || 1));
  return next;
}

async function saveRepo(entry) {
  const snap = await entry.ref.get();
  if (snap.exists) entry.data = mergeCIState(snap.data() || {}, entry.data);
  await entry.ref.set(entry.data, {merge: false});
  await syncMembershipIndex(entry);
}

async function syncMembershipIndex(entry) {
 const repo = entry.data.repo || {};
 if (!repo.logical && (!repo.bucket || !repo.prefix)) return;
 const repoIDValue = repoID(repo);
 const logical = repo.logical || repo.prefix || repoIDValue;
  const users = await loadBrokerUsers();
  const usersByID = new Map((users.data.users || []).map((user) => [user.id, user]));
  const memberships = new Map();
  function addMembership(publicKey, user, role, source, suspended) {
    if (!publicKey) return;
    const fingerprint = keyFingerprint(publicKey);
    const existing = memberships.get(fingerprint);
    const nextRole = existing ? strongerRole(existing.role, role || '') : role || '';
    memberships.set(fingerprint, {public_key: publicKey, user: user || '', role: nextRole, source: source || '', suspended: !!suspended});
  }
  for (const key of entry.data.keys || []) {
    addMembership(key.public_key, key.user, key.role, key.source, key.suspended);
  }
  for (const grant of entry.data.teams || []) {
    const team = await loadTeam(grant.id || grant.team_id);
    if (!team) continue;
    for (const member of team.data.members || []) {
      if (member.suspended) continue;
      const user = usersByID.get(member.user_id);
      if (!user || user.suspended || user.pending) continue;
      const role = weakerRole(normalizeRole(member.role || 'read'), normalizeRole(grant.role || 'read'));
      for (const key of user.keys || []) {
        addMembership(key.public_key || key, user.username || member.username || member.user_id, role, 'team', false);
      }
    }
  }
  for (const grant of entry.data.user_grants || []) {
    const user = usersByID.get(grant.user_id) || (users.data.users || []).find((item) => String(item.username || '').trim().toLowerCase() === String(grant.user || grant.username || '').trim().toLowerCase());
    if (!user || user.suspended || user.pending) continue;
    for (const key of user.keys || []) {
      addMembership(key.public_key || key, user.username || grant.username || grant.user_id, normalizeRole(grant.role || 'read'), 'repo-user', false);
    }
  }
  const writes = [];
  for (const membership of memberships.values()) {
    const fingerprint = keyFingerprint(membership.public_key);
    writes.push(members.doc(memberDocID(fingerprint)).collection('repos').doc(docID(repo)).set({
      repo_id: repoIDValue,
      logical,
      repo,
      user: membership.user || '',
      role: membership.role || '',
      source: membership.source || '',
      suspended: !!membership.suspended,
      updated_at: new Date().toISOString(),
    }, {merge: true}));
  }
  await Promise.all(writes);
}

async function syncReposForTeam(teamID) {
  const snap = await repos.get();
  const writes = [];
  snap.forEach((doc) => {
    if (String(doc.id || '').startsWith('_') || String(doc.id || '').startsWith('team:')) return;
    const data = doc.data() || {};
    if ((data.teams || []).some((grant) => (grant.id || grant.team_id) === teamID)) writes.push(syncMembershipIndex({ref: doc.ref, data}));
  });
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

async function loadBrokerUsers() {
  const ref = repos.doc('_users');
  const snap = await ref.get();
  const data = snap.exists ? (snap.data() || {}) : {};
  data.users = data.users || [];
  const owners = await loadOwners();
  for (const key of owners.data.keys || []) {
    const user = key.user || 'owner';
    let existing = data.users.find((item) => String(item.username || '').toLowerCase() === String(user).toLowerCase());
    if (!existing) {
      existing = {id: 'u_' + cleanName(user), username: user, broker_role: key.role === 'admin' ? 'admin' : 'owner', keys: [], suspended: false};
      data.users.push(existing);
    }
    if (key.public_key && !existing.keys.find((k) => normalizeKey(k.public_key || k) === normalizeKey(key.public_key))) {
      existing.keys.push({public_key: key.public_key, source: key.source || 'owner'});
    }
  }
  return {ref, data};
}

async function saveBrokerUsers(entry) {
  await entry.ref.set(entry.data, {merge: false});
}

function publicProfileForUser(user) {
  const profile = user && user.profile || {};
  return {bio: String(profile.bio || ''), avatar: String(profile.avatar || '')};
}

function normalizeProfileAvatar(value) {
  value = String(value || '').trim();
  if (!value) return '';
  if (value.length > 700000) throw Object.assign(new Error('avatar is too large'), {status: 400});
  if (!/^data:image\/(png|jpeg|webp);base64,[A-Za-z0-9+/=]+$/.test(value)) throw Object.assign(new Error('avatar must be a cropped image data URL'), {status: 400});
  return value;
}

function profileKeysForUser(entry, users, actor) {
  const keys = [];
  const seen = new Set();
  const addKey = (item) => {
    const publicKey = normalizeKey(item && (item.public_key || item));
    if (!publicKey || seen.has(publicKey)) return;
    seen.add(publicKey);
    keys.push({public_key: publicKey, fingerprint: keyFingerprint(publicKey), source: item.source || ''});
  };
  const username = String(actor.user || '').trim().toLowerCase();
  const brokerUser = (users.data.users || []).find((item) => String(item.username || item.id || '').trim().toLowerCase() === username);
  for (const key of brokerUser && brokerUser.keys || []) addKey(key);
  for (const key of entry.data.keys || []) {
    if (String(key.user || '').trim().toLowerCase() === username) addKey(key);
  }
  if (actor.public_key) addKey({public_key: actor.public_key, source: actor.source || ''});
  return keys;
}

function ensureProfileUser(users, actor) {
  const username = String(actor.user || '').trim();
  if (!username) throw Object.assign(new Error('user is required'), {status: 403});
  let user = (users.data.users || []).find((item) => String(item.username || '').trim().toLowerCase() === username.toLowerCase());
  if (!user) {
    user = {id: actor.user_id || 'u_' + cleanName(username), username, broker_role: 'user', keys: [], suspended: false};
    users.data.users.push(user);
  }
  if (actor.public_key && !user.keys.find((key) => normalizeKey(key.public_key || key) === normalizeKey(actor.public_key))) {
    user.keys.push({public_key: actor.public_key, source: actor.source || 'profile'});
  }
  user.profile = user.profile || {};
  return user;
}

function validBrokerRole(role) {
  return ['owner', 'admin', 'user'].includes(role);
}

function normalizeBrokerRole(role) {
  return validBrokerRole(role) ? role : 'user';
}

async function signedBrokerUser(req) {
  const users = await loadBrokerUsers();
  const publicKey = normalizeKey(req.get('x-bgit-key'));
  const message = String(req.get('x-bgit-signature-message') || '');
  const signature = String(req.get('x-bgit-signature') || '');
  if (!publicKey || !message || !signature) return null;
  for (const user of users.data.users || []) {
    if (user.suspended) continue;
    for (const key of user.keys || []) {
      const keyValue = normalizeKey(key.public_key || key);
      if (keyValue === publicKey && await verifySignedRequest(req, publicKey, message, signature)) {
        return {user: user.username || user.id, user_id: user.id, broker_role: user.broker_role || 'user', public_key: publicKey, source: 'broker-user'};
      }
    }
  }
  return null;
}

async function requireBrokerAdmin(req) {
  const user = await signedBrokerUser(req);
  if (user && (user.broker_role === 'owner' || user.broker_role === 'admin')) return user;
  const owners = await loadOwners();
  const key = await signedKey(req, owners);
  if (key && (key.role === 'owner' || key.role === 'admin')) return {user: key.user || 'owner', broker_role: key.role, public_key: key.public_key};
  throw Object.assign(new Error('broker admin SSH signature required'), {status: 403});
}

function teamDocID(teamID) {
  return 'team:' + String(teamID || '').trim();
}

async function loadTeam(teamID) {
  const ref = repos.doc(teamDocID(teamID));
  const snap = await ref.get();
  if (!snap.exists) return null;
  const data = snap.data() || {};
  data.members = data.members || [];
  return {ref, data};
}

async function ensureCoreTeam(actor) {
  const existing = await loadTeam(coreTeamID);
  if (existing) return existing;
  const ref = repos.doc(teamDocID(coreTeamID));
  const team = {id: coreTeamID, name: coreTeamName, members: [], created_by: actor || 'setup', created_at: new Date().toISOString()};
  await ref.set(team, {merge: false});
  return {ref, data: team};
}

async function loadTeamByName(name) {
  const want = String(name || '').trim().toLowerCase();
  if (!want) return null;
  if (want === coreTeamName || want === coreTeamID) return ensureCoreTeam('lookup');
  const snap = await repos.get();
  const matches = snap.docs
    .filter((doc) => String(doc.id || '').startsWith('team:'))
    .map((doc) => ({ref: doc.ref, data: doc.data() || {}}))
    .filter((team) => String(team.data.name || '').trim().toLowerCase() === want || String(team.data.id || '').trim().toLowerCase() === want);
  if (matches.length === 0) return null;
  if (matches.length > 1) throw Object.assign(new Error('team name is ambiguous'), {status: 409});
  matches[0].data.members = matches[0].data.members || [];
  return matches[0];
}

async function teamRoleForUser(teamID, userID) {
  if (!teamID || !userID) return '';
  const team = await loadTeam(teamID);
  if (!team) return '';
  const member = (team.data.members || []).find((item) => item.user_id === userID);
  return member && !member.suspended ? normalizeRole(member.role || 'read') : '';
}

function strongerRole(a, b) {
  const rank = {read: 1, triage: 2, developer: 3, maintainer: 4, admin: 5, owner: 6};
  return (rank[b] || 0) > (rank[a] || 0) ? b : a;
}

function weakerRole(a, b) {
  const rank = {read: 1, triage: 2, developer: 3, maintainer: 4, admin: 5, owner: 6};
  if (!a) return b || '';
  if (!b) return a || '';
  return (rank[b] || 0) < (rank[a] || 0) ? b : a;
}

async function effectiveSignedKey(req, entry) {
  let direct = await signedKey(req, entry);
  let role = direct ? direct.role : '';
  const brokerUser = await signedBrokerUser(req);
  if (brokerUser) {
    for (const grant of entry.data.user_grants || []) {
      const grantUser = String(grant.user || grant.username || '').trim().toLowerCase();
      if ((grant.user_id && grant.user_id === brokerUser.user_id) || (grantUser && grantUser === String(brokerUser.user || '').trim().toLowerCase())) {
        role = strongerRole(role, normalizeRole(grant.role || 'read'));
      }
    }
    for (const grant of entry.data.teams || []) {
      const teamRole = await teamRoleForUser(grant.id || grant.team_id, brokerUser.user_id);
      if (teamRole) role = strongerRole(role, weakerRole(teamRole, grant.role || teamRole));
    }
  }
  if (!direct && brokerUser && role) {
    direct = {user: brokerUser.user, role, public_key: brokerUser.public_key, source: 'team', user_id: brokerUser.user_id};
  } else if (direct && role && role !== direct.role) {
    direct = {...direct, role};
  }
  return direct;
}

function audit(entry, event) {
  entry.data.audit = entry.data.audit || [];
  entry.data.audit.push({...event, at: new Date().toISOString()});
  if (entry.data.audit.length > 500) entry.data.audit = entry.data.audit.slice(-500);
}

function readSSHString(buf, offset) {
  if (offset + 4 > buf.length) throw new Error('invalid SSH wire string');
  const len = buf.readUInt32BE(offset);
  const start = offset + 4;
  if (start + len > buf.length) throw new Error('invalid SSH wire string');
  return {value: buf.subarray(start, start + len), offset: start + len};
}

function rawBody(req) {
  if (req.rawBody) return Buffer.from(req.rawBody);
  return Buffer.from(JSON.stringify(req.body || {}));
}

function expectedMessage(req) {
  const timestamp = String(req.get('x-bgit-timestamp') || '').trim();
  const nonce = String(req.get('x-bgit-nonce') || '').trim();
  const signedHost = String(req.get('x-bgit-signed-host') || '').trim().toLowerCase();
  const digest = crypto.createHash('sha256').update(rawBody(req)).digest('hex');
  return Buffer.from([
    'bgit-broker-v2',
    String(req.method || '').toUpperCase(),
    requestPathWithQuery(req),
    signedHost,
    timestamp,
    nonce,
    digest,
  ].join('\n')).toString('base64');
}

function requestPathWithQuery(req) {
  const original = String(req.originalUrl || req.url || req.path || '/');
  if (!original) return '/';
  if (original.startsWith('/')) return original;
  try {
    return new URL(original).pathname + new URL(original).search;
  } catch (_) {
    return req.path || '/';
  }
}

function observedHost(req) {
  return String(req.get('x-forwarded-host') || req.get('host') || '').split(',')[0].trim().toLowerCase();
}

function validateSignatureHeaders(req, message) {
  if (String(req.get('x-bgit-signature-version') || '') !== '2') return false;
  const timestamp = String(req.get('x-bgit-timestamp') || '').trim();
  const nonce = String(req.get('x-bgit-nonce') || '').trim();
  const signedHost = String(req.get('x-bgit-signed-host') || '').trim().toLowerCase();
  if (!timestamp || !nonce || !signedHost || !message) return false;
  if (message !== expectedMessage(req)) return false;
  const ts = Number(timestamp);
  if (!Number.isInteger(ts)) return false;
  const now = Math.floor(Date.now() / 1000);
  if (Math.abs(now - ts) > 300) return false;
  const host = observedHost(req);
  if (!host || signedHost !== host) return false;
  return true;
}

async function consumeSignatureNonce(fingerprint, nonce, timestamp) {
  const nonceID = Buffer.from(fingerprint + '\n' + nonce).toString('base64url');
  const ref = nonces.doc(nonceID);
  const expiresAt = new Date((Number(timestamp) + 300) * 1000).toISOString();
  await db.runTransaction(async (tx) => {
    const snap = await tx.get(ref);
    if (snap.exists) {
      const data = snap.data() || {};
      if (!data.expires_at || Date.parse(data.expires_at) > Date.now()) {
        throw Object.assign(new Error('broker signature replay detected'), {status: 403});
      }
    }
    tx.set(ref, {fingerprint, nonce, expires_at: expiresAt, created_at: new Date().toISOString()}, {merge: false});
  });
}

async function verifySignedRequest(req, publicKey, message, signature) {
  const fingerprint = keyFingerprint(publicKey);
  const verified = req.bgitVerifiedSignature || {};
  if (verified[fingerprint]) return true;
  if (!validateSignatureHeaders(req, message)) return false;
  if (!verifySSHSignature(publicKey, message, signature)) return false;
  await consumeSignatureNonce(fingerprint, String(req.get('x-bgit-nonce') || '').trim(), String(req.get('x-bgit-timestamp') || '').trim());
  req.bgitVerifiedSignature = {...verified, [fingerprint]: true};
  return true;
}

function normalizeKey(key) {
  return String(key || '').trim().split(/\s+/).slice(0, 2).join(' ');
}

function base64URL(buf) {
  return Buffer.from(buf).toString('base64url');
}

function sshMPIntToBuffer(buf) {
  let out = Buffer.from(buf);
  while (out.length > 1 && out[0] === 0) out = out.subarray(1);
  return out;
}

function ecdsaSignatureToDER(blob) {
  let parsed = readSSHString(blob, 0);
  const r = sshMPIntToBuffer(parsed.value);
  parsed = readSSHString(blob, parsed.offset);
  const s = sshMPIntToBuffer(parsed.value);
  const encodeInt = (value) => {
    let out = Buffer.from(value);
    if (out.length === 0) out = Buffer.from([0]);
    if (out[0] & 0x80) out = Buffer.concat([Buffer.from([0]), out]);
    return Buffer.concat([Buffer.from([0x02, out.length]), out]);
  };
  const body = Buffer.concat([encodeInt(r), encodeInt(s)]);
  if (body.length > 127) throw new Error('ECDSA signature too large');
  return Buffer.concat([Buffer.from([0x30, body.length]), body]);
}

function publicKeyObject(publicKey) {
  const parts = normalizeKey(publicKey).split(/\s+/);
  if (parts.length < 2) throw new Error('invalid SSH public key');
  const blob = Buffer.from(parts[1], 'base64');
  let parsed = readSSHString(blob, 0);
  const alg = parsed.value.toString();
  if (alg !== parts[0]) throw new Error('SSH key algorithm mismatch');
  if (alg === 'ssh-ed25519') {
    parsed = readSSHString(blob, parsed.offset);
    return crypto.createPublicKey({key: {kty: 'OKP', crv: 'Ed25519', x: base64URL(parsed.value)}, format: 'jwk'});
  }
  if (alg === 'ssh-rsa') {
    parsed = readSSHString(blob, parsed.offset);
    const e = sshMPIntToBuffer(parsed.value);
    parsed = readSSHString(blob, parsed.offset);
    const n = sshMPIntToBuffer(parsed.value);
    return crypto.createPublicKey({key: {kty: 'RSA', n: base64URL(n), e: base64URL(e)}, format: 'jwk'});
  }
  if (alg.startsWith('ecdsa-sha2-')) {
    parsed = readSSHString(blob, parsed.offset);
    const sshCurve = parsed.value.toString();
    parsed = readSSHString(blob, parsed.offset);
    const point = parsed.value;
    const curves = {'nistp256': 'P-256', 'nistp384': 'P-384', 'nistp521': 'P-521'};
    const crv = curves[sshCurve];
    if (!crv || !point.length || point[0] !== 4) throw new Error('unsupported ECDSA SSH key');
    const coordinateLength = Math.ceil(Number(sshCurve.replace('nistp', '')) / 8);
    const x = point.subarray(1, 1 + coordinateLength);
    const y = point.subarray(1 + coordinateLength, 1 + 2 * coordinateLength);
    if (x.length !== coordinateLength || y.length !== coordinateLength) throw new Error('invalid ECDSA SSH key');
    return crypto.createPublicKey({key: {kty: 'EC', crv, x: base64URL(x), y: base64URL(y)}, format: 'jwk'});
  }
  throw new Error('unsupported SSH key algorithm');
}

function signatureVerifyAlgorithm(alg) {
  if (alg === 'ssh-ed25519') return null;
  if (alg === 'ssh-rsa') return 'sha1';
  if (alg === 'rsa-sha2-256') return 'sha256';
  if (alg === 'rsa-sha2-512') return 'sha512';
  if (alg === 'ecdsa-sha2-nistp256') return 'sha256';
  if (alg === 'ecdsa-sha2-nistp384') return 'sha384';
  if (alg === 'ecdsa-sha2-nistp521') return 'sha512';
  throw new Error('unsupported SSH signature algorithm');
}

function signatureBlobForVerify(alg, sig) {
  if (alg.startsWith('ecdsa-sha2-')) return ecdsaSignatureToDER(sig);
  return sig;
}

function verifySSHSignature(publicKey, message, signature) {
  const parsed = readSSHString(Buffer.from(signature, 'base64'), 0);
  const alg = parsed.value.toString();
  const sig = readSSHString(Buffer.from(signature, 'base64'), parsed.offset).value;
  return crypto.verify(signatureVerifyAlgorithm(alg), Buffer.from(message, 'base64'), publicKeyObject(publicKey), signatureBlobForVerify(alg, sig));
}

async function signedKey(req, entry) {
  const keys = (entry.data.keys || []).filter((k) => !k.suspended);
  const publicKey = normalizeKey(req.get('x-bgit-key'));
  const message = String(req.get('x-bgit-signature-message') || '');
  const signature = String(req.get('x-bgit-signature') || '');
  if (!publicKey || !message || !signature) return null;
  const key = keys.find((k) => normalizeKey(k.public_key) === publicKey);
  if (!key) return null;
  if (!await verifySignedRequest(req, publicKey, message, signature)) return null;
  return key;
}

async function submittedSignedKey(req) {
  const publicKey = normalizeKey(req.get('x-bgit-key'));
  const message = String(req.get('x-bgit-signature-message') || '');
  const signature = String(req.get('x-bgit-signature') || '');
  if (!publicKey || !message || !signature) return null;
  if (!await verifySignedRequest(req, publicKey, message, signature)) return null;
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

function brokerUserInviteCode(brokerURL, user, role, token) {
  const payload = Buffer.from(JSON.stringify({broker_url: brokerURL, user, role, token})).toString('base64url');
  return 'bgituser_' + payload;
}

async function verifySignature(req, entry) {
  const adminKeys = (entry.data.keys || []).filter((k) => (k.role === 'admin' || k.role === 'owner') && !k.suspended);
  if (adminKeys.length === 0) return verifyBootstrapToken(req, entry);
  const key = await signedKey(req, entry);
  return !!key && (key.role === 'admin' || key.role === 'owner');
}

function bootstrapTokenHash(token) {
  return crypto.createHash('sha256').update(String(token || '')).digest('hex');
}

function verifyBootstrapToken(req, entry) {
  const expected = String(process.env.BGIT_OWNER_BOOTSTRAP_HASH || '').trim();
  if (!expected || entry.data.bootstrap_used_at) return false;
  const token = String(req.get('x-bgit-bootstrap-token') || '').trim();
  return !!token && bootstrapTokenHash(token) === expected;
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

function normalizedUsername(user) {
  return String(user || '').trim().toLowerCase();
}

function assertUniqueRepoKey(entry, publicKey, user) {
  const normalized = normalizeKey(publicKey);
  if (!normalized) return null;
  const existing = (entry.data.keys || []).find((item) => normalizeKey(item.public_key) === normalized);
  if (existing && normalizedUsername(existing.user) !== normalizedUsername(user)) {
    throw Object.assign(new Error('SSH key already belongs to user ' + (existing.user || 'unknown')), {status: 409});
  }
  return existing || null;
}

function assertUniqueBrokerUserKey(users, publicKey, username) {
  const normalized = normalizeKey(publicKey);
  if (!normalized) return null;
  const target = normalizedUsername(username);
  for (const user of users.data.users || []) {
    for (const key of user.keys || []) {
      if (normalizeKey(key.public_key || key) === normalized) {
        if (normalizedUsername(user.username) !== target) {
          throw Object.assign(new Error('SSH key already belongs to broker user ' + (user.username || 'unknown')), {status: 409});
        }
        return key;
      }
    }
  }
  return null;
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
    ci_run: roleAllows(role, 'write'),
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

async function requireAdmin(req, entry) {
  const key = await effectiveSignedKey(req, entry);
  if (!key || (key.role !== 'owner' && key.role !== 'admin')) {
    const brokerUser = await signedBrokerUser(req);
    if (brokerUser && (brokerUser.broker_role === 'owner' || brokerUser.broker_role === 'admin')) {
      return {user: brokerUser.user || '', role: brokerUser.broker_role, broker_role: brokerUser.broker_role, public_key: brokerUser.public_key || '', source: 'broker-user'};
    }
    const err = new Error('admin SSH signature required');
    err.status = 403;
    throw err;
  }
  return key;
}

async function requireOwner(req, entry) {
  const key = await effectiveSignedKey(req, entry);
  if (key && key.role === 'owner') return key;
  const brokerUser = await signedBrokerUser(req);
  if (brokerUser && brokerUser.broker_role === 'owner') {
    return {user: brokerUser.user || '', role: 'owner', broker_role: 'owner', public_key: brokerUser.public_key || '', source: 'broker-user'};
  }
  throw Object.assign(new Error('owner SSH signature required'), {status: 403});
}

async function requireRead(req, entry) {
  const key = await effectiveSignedKey(req, entry);
  if (!key && repoIsPublic(entry)) return anonymousKey();
  if (!key || !roleAllows(key.role, 'read')) {
    const err = new Error('read SSH signature required');
    err.status = 403;
    throw err;
  }
  return key;
}

async function requireWrite(req, entry) {
  if (repoIsReadOnly(entry)) {
    const err = new Error('repository is read-only');
    err.status = 403;
    throw err;
  }
  const key = await effectiveSignedKey(req, entry);
  if (!key || !roleAllows(key.role, 'write')) {
    const err = new Error('write SSH signature required');
    err.status = 403;
    throw err;
  }
  return key;
}

async function requireIssueCreate(req, entry) {
  if (repoIsReadOnly(entry)) throw Object.assign(new Error('repository is read-only'), {status: 403});
  if (repoIsPublic(entry)) return await effectiveSignedKey(req, entry) || anonymousKey();
  return requireRead(req, entry);
}

async function requireBoardMutation(req, entry) {
  return requireWrite(req, entry);
}

function normalizeBoardLane(value) {
  const lane = String(value || '').trim().toLowerCase();
  if (!lane || lane === 'backlog') return 'backlog';
  if (lane === 'ready' || lane === 'todo' || lane === 'to-do') return 'ready';
  if (lane === 'doing' || lane === 'in-progress' || lane === 'in_progress' || lane === 'progress') return 'doing';
  if (lane === 'review' || lane === 'in-review' || lane === 'in_review') return 'review';
  if (lane === 'done' || lane === 'closed') return 'done';
  throw Object.assign(new Error('unknown board lane'), {status: 400});
}

async function boardAssignableUsers(entry) {
  const seen = new Set();
  const users = [];
  const addUser = (user, role, suspended) => {
    user = String(user || '').trim();
    if (!user || suspended || !roleAllows(normalizeRole(role || 'read'), 'write')) return;
    const normalized = user.toLowerCase();
    if (seen.has(normalized)) return;
    seen.add(normalized);
    users.push(user);
  };
  for (const key of entry.data.keys || []) {
    addUser(key.user, key.role, key.suspended);
  }
  const brokerUsers = await loadBrokerUsers();
  const usersByID = new Map((brokerUsers.data.users || []).map((user) => [user.id, user]));
  for (const grant of entry.data.teams || []) {
    const team = await loadTeam(grant.id || grant.team_id);
    if (!team) continue;
    for (const member of team.data.members || []) {
      if (member.suspended) continue;
      const user = usersByID.get(member.user_id);
      if (!user || user.suspended || user.pending) continue;
      const role = weakerRole(normalizeRole(member.role || 'read'), normalizeRole(grant.role || 'read'));
      addUser(user.username || member.username || member.user_id, role, false);
    }
  }
  users.sort((a, b) => a.toLowerCase().localeCompare(b.toLowerCase()));
  return users;
}

function cleanObjectPath(value) {
  const path = String(value || '').replace(/^\/+/, '');
  if (path.includes('\0') || path.includes('\\') || path.split('/').some((part) => part === '.' || part === '..' || (part === '' && path !== ''))) throw new Error('invalid object path');
  return path;
}

function isGitObjectDatabasePath(path) {
  return path === 'objects' || path.startsWith('objects/');
}

function validateCapabilityPath(operation, objectPath) {
  const path = cleanObjectPath(objectPath);
  if (!path) throw Object.assign(new Error('invalid object path'), {status: 400});
  if (operation === 'delete') throw Object.assign(new Error('delete capabilities are not supported'), {status: 400});
  if (operation === 'write') {
    if (!isGitObjectDatabasePath(path)) throw Object.assign(new Error('write capabilities are restricted to git object paths'), {status: 403});
    return path;
  }
  if (operation === 'read') {
    if (path === 'HEAD' || path === 'packed-refs' || path.startsWith('refs/') || isGitObjectDatabasePath(path)) return path;
    throw Object.assign(new Error('read capabilities are restricted to git repository paths'), {status: 403});
  }
  throw Object.assign(new Error('unsupported object capability operation'), {status: 400});
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

async function readTextObject(repo, objectPath) {
  return Buffer.from(await readObject(repo, objectPath), 'base64').toString('utf8');
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
  let physicalRepo = null;
  await db.runTransaction(async (tx) => {
    const snap = await tx.get(refDoc);
    const data = snap.exists ? (snap.data() || {}) : {repo, keys: [], refs: {}, audit: []};
    data.repo = data.repo || repo;
    physicalRepo = data.repo;
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
  if (physicalRepo && physicalRepo.bucket) {
    if (newHash === zero) await deleteObject(physicalRepo, ref);
    else await writeTextObject(physicalRepo, ref, newHash + '\n');
  }
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

function issueHistory(issue, user, action, fields = {}) {
  issue.history = Array.isArray(issue.history) ? issue.history : [];
  issue.history.push({user: user || 'anonymous', action, at: new Date().toISOString(), ...fields});
  if (issue.history.length > 100) issue.history = issue.history.slice(-100);
}

function storyPosition(issue) {
  const value = Number(issue && issue.position || 0);
  return Number.isFinite(value) && value > 0 ? value : 0;
}

function sortStoriesForLane(stories) {
  return stories.sort((a, b) => {
    const ap = storyPosition(a);
    const bp = storyPosition(b);
    if (ap && bp && ap !== bp) return ap - bp;
    if (ap && !bp) return -1;
    if (!ap && bp) return 1;
    return Number(a.id || 0) - Number(b.id || 0);
  });
}

async function nextStoryPosition(entry, lane, excludeID = 0) {
  const stories = (await listIssues(entry)).filter((issue) =>
    issue.type === 'story' &&
    !issue.archived &&
    Number(issue.id || 0) !== Number(excludeID || 0) &&
    normalizeBoardLane(issue.lane) === lane
  );
  const max = stories.reduce((value, issue) => Math.max(value, storyPosition(issue)), 0);
  return max + 1000;
}

async function storyPositionAfter(entry, lane, afterID, selfID) {
  const stories = sortStoriesForLane((await listIssues(entry)).filter((issue) =>
    issue.type === 'story' &&
    !issue.archived &&
    Number(issue.id || 0) !== Number(selfID || 0) &&
    normalizeBoardLane(issue.lane) === lane
  ));
  if (!stories.length) return 1000;
  const after = Number(afterID || 0);
  if (!after) {
    const first = storyPosition(stories[0]);
    return first > 1 ? first / 2 : 500;
  }
  const index = stories.findIndex((issue) => Number(issue.id || 0) === after);
  if (index < 0) return nextStoryPosition(entry, lane, selfID);
  const current = storyPosition(stories[index]) || ((index + 1) * 1000);
  const next = stories[index + 1] ? storyPosition(stories[index + 1]) : 0;
  if (!next) return current + 1000;
  return current + ((next - current) / 2);
}

function nextCIID(data) {
  data.next_ci_id = Number(data.next_ci_id || 1);
  return data.next_ci_id++;
}

function listCIRuns(data) {
  const runs = Array.isArray(data.ci_runs) ? data.ci_runs : [];
  return [...runs].sort((a, b) => Number(b.id || 0) - Number(a.id || 0));
}

function findCIRun(data, id) {
  return (data.ci_runs || []).find((run) => Number(run.id) === Number(id));
}

function ciTerminal(status) {
  return ['passed', 'failed', 'cancelled', 'timed_out'].includes(String(status || '').toLowerCase());
}

function mapGCPBuildStatus(status) {
  switch (String(status || '').toUpperCase()) {
    case 'SUCCESS': return 'passed';
    case 'FAILURE':
    case 'INTERNAL_ERROR': return 'failed';
    case 'TIMEOUT':
    case 'EXPIRED': return 'timed_out';
    case 'CANCELLED': return 'cancelled';
    case 'WORKING': return 'building';
    case 'QUEUED':
    case 'PENDING': return 'queued';
    default: return 'queued';
  }
}

async function googleAPI(method, url, body) {
  const client = await auth.getClient();
  const accessToken = await client.getAccessToken();
  const token = typeof accessToken === 'string' ? accessToken : accessToken && accessToken.token;
  const resp = await fetch(url, {
    method,
    headers: {authorization: 'Bearer ' + token, 'content-type': 'application/json'},
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await resp.text();
  if (!resp.ok) throw Object.assign(new Error(text || ('Google API returned HTTP ' + resp.status)), {status: 502});
  try { return text ? JSON.parse(text) : {}; } catch (_) { return {}; }
}

function gcpProjectID() {
  return String(process.env.BGIT_GCP_PROJECT || process.env.GOOGLE_CLOUD_PROJECT || process.env.GCP_PROJECT || '').trim();
}

async function refreshCIRun(run) {
  if (!run || run.provider !== 'gcp' || ciTerminal(run.status)) return false;
  const project = gcpProjectID();
  const buildID = String(run.provider_build_id || '').trim();
  if (!project || !buildID) return false;
  const before = JSON.stringify(run);
  const buildName = String(run.provider_build_name || '').trim() || 'projects/' + project + '/builds/' + buildID;
  const build = await googleAPI('GET', 'https://cloudbuild.googleapis.com/v1/' + buildName);
  run.status = mapGCPBuildStatus(build.status);
  run.result = run.status;
  run.url = build.logUrl || run.url || '';
  run.provider_build_id = build.id || run.provider_build_id || '';
  run.provider_build_name = build.name || run.provider_build_name || '';
  run.started_at = build.startTime || run.started_at || '';
  run.finished_at = build.finishTime || run.finished_at || '';
  run.message = build.statusDetail || build.failureInfo && build.failureInfo.detail || run.message || '';
  run.updated_at = new Date().toISOString();
  return JSON.stringify(run) !== before;
}

async function refreshCIRuns(entry) {
  let changed = false;
  for (const run of entry.data.ci_runs || []) {
    if (await refreshCIRun(run)) changed = true;
  }
  if (changed) await saveRepo(entry);
  return changed;
}

function logEntryText(entry) {
  if (!entry) return '';
  if (entry.textPayload) return String(entry.textPayload);
  if (entry.jsonPayload) {
    if (entry.jsonPayload.message) return String(entry.jsonPayload.message);
    return JSON.stringify(entry.jsonPayload);
  }
  if (entry.protoPayload && entry.protoPayload.status && entry.protoPayload.status.message) return String(entry.protoPayload.status.message);
  return '';
}

async function ciRunLogs(run) {
  const project = gcpProjectID();
  const buildID = String(run.provider_build_id || '').trim();
  if (!project || !buildID) return '';
  const filter = 'resource.type="build" AND resource.labels.build_id="' + buildID + '"';
  let data = await googleAPI('POST', 'https://logging.googleapis.com/v2/entries:list', {
    resourceNames: ['projects/' + project],
    filter,
    orderBy: 'timestamp asc',
    pageSize: 1000,
  });
  if (!Array.isArray(data.entries) || data.entries.length === 0) {
    data = await googleAPI('POST', 'https://logging.googleapis.com/v2/entries:list', {
      resourceNames: ['projects/' + project],
      filter: 'resource.type="build"',
      orderBy: 'timestamp desc',
      pageSize: 1000,
    });
    data.entries = (data.entries || []).filter((entry) => entry && entry.resource && entry.resource.labels && entry.resource.labels.build_id === buildID).reverse();
  }
  return (data.entries || []).map(logEntryText).filter(Boolean).join('\n');
}

function normalizeCIProvider(value, repo) {
  const raw = String(value || '').trim().toLowerCase();
  if (raw === 'aws' || raw === 'codebuild' || (raw === '' && repo.provider === 's3')) return 'aws';
  return 'gcp';
}

function normalizeCIRef(value) {
  const ref = String(value || '').trim();
  if (!ref) throw new Error('CI ref is required');
  if (ref.startsWith('refs/')) return ref;
  return 'refs/heads/' + ref.replace(/^heads\//, '');
}

function cleanCIConfig(value, provider) {
  const config = String(value || '').trim();
  const defaults = provider === 'aws' ? ['buildspec.yml', 'buildspec.yaml'] : ['cloudbuild.yaml', 'cloudbuild.yml'];
  const out = config || defaults[0];
  if (out.startsWith('/') || out.includes('\\') || out.includes('\0') || out.split('/').includes('..')) {
    throw Object.assign(new Error('invalid CI config path'), {status: 400});
  }
  if (!/\.(ya?ml)$/i.test(out)) throw Object.assign(new Error('CI config must be a YAML file'), {status: 400});
  return out;
}

async function triggerCIRun(entry, run) {
  const url = String(process.env.BGIT_CI_MATERIALIZER_URL || '').trim();
  if (!url) {
    run.status = 'queued';
    run.message = 'CI materializer is not configured for this broker yet.';
    return run;
  }
  const payload = {
    repo: entry.data.repo,
    run: {id: run.id, provider: run.provider, ref: run.ref, commit: run.commit, config: run.config},
    broker_version: brokerVersion,
  };
  const headers = {'content-type': 'application/json'};
  const token = await ciMaterializerToken();
  if (token) headers['x-bgit-ci-token'] = token;
  const client = await auth.getIdTokenClient(url);
  const resp = await client.request({url, method: 'POST', headers, data: payload});
  const data = resp.data && typeof resp.data === 'object' ? resp.data : {};
  run.status = data.status || 'queued';
  run.url = data.url || run.url || '';
  run.message = data.message || run.message || '';
  run.provider_build_id = data.provider_build_id || run.provider_build_id || '';
  run.provider_build_name = data.provider_build_name || run.provider_build_name || '';
  return run;
}

function randomCIToken() {
  return crypto.randomBytes(32).toString('base64url');
}

async function secretManagerRequest(path, opts = {}) {
  const client = await auth.getClient();
  const accessToken = await client.getAccessToken();
  const token = typeof accessToken === 'string' ? accessToken : accessToken && accessToken.token;
  const resp = await fetch('https://secretmanager.googleapis.com/v1/' + path, {
    method: opts.method || 'GET',
    headers: {
      authorization: 'Bearer ' + token,
      'content-type': 'application/json',
    },
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  const text = await resp.text();
  if (!resp.ok) throw Object.assign(new Error(text || ('Secret Manager returned HTTP ' + resp.status)), {status: 502});
  try { return text ? JSON.parse(text) : {}; } catch (_) { return {}; }
}

async function ciMaterializerToken() {
  const secret = String(process.env.BGIT_CI_MATERIALIZER_SECRET || '').trim();
  if (!secret) return '';
  const data = await secretManagerRequest(secret + '/versions/latest:access');
  return Buffer.from(data.payload && data.payload.data || '', 'base64').toString('utf8').trim();
}

async function rotateCIMaterializerToken() {
  const secret = String(process.env.BGIT_CI_MATERIALIZER_SECRET || '').trim();
  if (!secret) throw Object.assign(new Error('CI materializer secret is not configured'), {status: 500});
  const token = randomCIToken();
  await secretManagerRequest(secret + ':addVersion', {
    method: 'POST',
    body: {payload: {data: Buffer.from(token).toString('base64')}},
  });
  return {rotated: true};
}

async function deleteRepoMetadata(entry) {
  const prSnap = await entry.ref.collection('prs').get();
  const issueSnap = await entry.ref.collection('issues').get();
  const deletes = [];
  prSnap.forEach((doc) => deletes.push(doc.ref.delete()));
  issueSnap.forEach((doc) => deletes.push(doc.ref.delete()));
  const repo = entry.data.repo || {};
  const oldRepoDocID = docID(repo);
  const users = await loadBrokerUsers();
  for (const key of entry.data.keys || []) {
    if (!key.public_key) continue;
    deletes.push(members.doc(memberDocID(keyFingerprint(key.public_key))).collection('repos').doc(oldRepoDocID).delete());
  }
  for (const grant of entry.data.user_grants || []) {
    const user = (users.data.users || []).find((item) => item.id === grant.user_id || String(item.username || '').trim().toLowerCase() === String(grant.user || grant.username || '').trim().toLowerCase());
    for (const key of user && user.keys || []) {
      const publicKey = key.public_key || key;
      if (!publicKey) continue;
      deletes.push(members.doc(memberDocID(keyFingerprint(publicKey))).collection('repos').doc(oldRepoDocID).delete());
    }
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
  const users = await loadBrokerUsers();
  for (const key of entry.data.keys || []) {
    if (!key.public_key) continue;
    deletes.push(members.doc(memberDocID(keyFingerprint(key.public_key))).collection('repos').doc(repoDocID).delete());
  }
  for (const grant of entry.data.user_grants || []) {
    const user = (users.data.users || []).find((item) => item.id === grant.user_id || String(item.username || '').trim().toLowerCase() === String(grant.user || grant.username || '').trim().toLowerCase());
    for (const key of user && user.keys || []) {
      const publicKey = key.public_key || key;
      if (!publicKey) continue;
      deletes.push(members.doc(memberDocID(keyFingerprint(publicKey))).collection('repos').doc(repoDocID).delete());
    }
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
  repo = validateRepo(repo);
  const entry = await loadRepo(repo);
  if (entry.data.repo && entry.data.repo.logical && entry.data.repo.team_id === coreTeamID) await ensureCoreTeam('repo');
  const owners = await loadOwners();
  const ownerKeys = new Set((owners.data.keys || []).filter((owner) => owner.role === 'owner').map((owner) => normalizeKey(owner.public_key)));
  entry.data.keys = (entry.data.keys || []).filter((key) => !(key.role === 'owner' && ownerKeys.has(normalizeKey(key.public_key)) && key.source !== 'ownership-transfer'));
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
      const bootstrapping = !(entry.data.keys || []).some((k) => (k.role === 'admin' || k.role === 'owner') && !k.suspended);
      if (!await verifySignature(req, entry)) throw Object.assign(new Error('owner SSH signature required'), {status: 403});
      const user = body.user || 'owner';
      const role = normalizeRole(body.role || 'owner');
      if (role !== 'owner') throw new Error('owner bootstrap only accepts owner role');
      for (const publicKey of body.public_keys || []) {
        if (!assertUniqueRepoKey(entry, publicKey, user)) entry.data.keys.push({user, role, public_key: publicKey, source: body.source || '', suspended: false});
      }
      if (bootstrapping) {
        entry.data.bootstrap_used_at = new Date().toISOString();
        audit(entry, {type: 'owner_bootstrap', user, source_ip: req.get('x-forwarded-for') || req.ip || '', user_agent: req.get('user-agent') || ''});
      }
      await saveRepo(entry);
      await ensureCoreTeam(user);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/broker/users/list' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const users = await loadBrokerUsers();
      res.status(200).send(JSON.stringify({users: users.data.users || []}));
      return;
    }
    if (req.path === '/broker/users/upsert' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const users = await loadBrokerUsers();
      const username = String(body.user || body.username || '').trim();
      if (!username) throw new Error('user is required');
      const role = normalizeBrokerRole(body.broker_role || body.role || 'user');
      if (!validBrokerRole(body.broker_role || body.role || 'user')) throw new Error('invalid broker role');
      if (role === 'owner') throw Object.assign(new Error('broker owner role is managed by owner transfer'), {status: 403});
      let user = users.data.users.find((item) => String(item.username || '').toLowerCase() === username.toLowerCase());
      if (user && user.broker_role === 'owner') throw Object.assign(new Error('broker owner cannot be reassigned or suspended'), {status: 403});
      if (!user) {
        user = {id: 'u_' + crypto.randomBytes(6).toString('hex'), username, broker_role: role, keys: [], suspended: false};
        users.data.users.push(user);
      }
      user.username = username;
      user.broker_role = role;
      user.suspended = !!body.suspended;
      user.keys = user.keys || [];
      for (const publicKey of body.public_keys || []) {
        if (!assertUniqueBrokerUserKey(users, publicKey, username)) user.keys.push({public_key: publicKey, source: body.source || ''});
      }
      await saveBrokerUsers(users);
      res.status(200).send(JSON.stringify({ok: true, user}));
      return;
    }
    if (req.path === '/broker/users/delete' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const users = await loadBrokerUsers();
      const username = String(body.user || body.username || '').trim();
      if (!username) throw new Error('user is required');
      const normalizedUser = username.toLowerCase();
      const user = (users.data.users || []).find((item) => String(item.username || '').toLowerCase() === normalizedUser);
      if (!user) throw Object.assign(new Error('broker user not found'), {status: 404});
      if (user.broker_role === 'owner') throw Object.assign(new Error('broker owner cannot be deleted'), {status: 403});
      users.data.users = (users.data.users || []).filter((item) => String(item.username || '').toLowerCase() !== normalizedUser);
      users.data.invites = (users.data.invites || []).filter((invite) => String(invite.user || '').trim().toLowerCase() !== normalizedUser);
      await saveBrokerUsers(users);
      for (const key of user.keys || []) {
        if (!key.public_key) continue;
        const idx = await members.doc(memberDocID(keyFingerprint(key.public_key))).collection('repos').get();
        await Promise.all(idx.docs.map((doc) => doc.ref.delete()));
      }
      const snap = await repos.get();
      const removedRepoKeys = [];
      for (const doc of snap.docs) {
        const id = String(doc.id || '');
        if (id.startsWith('_')) continue;
        const data = doc.data() || {};
        let changed = false;
        if (id.startsWith('team:')) {
          const nextMembers = (data.members || []).filter((member) => String(member.username || '').trim().toLowerCase() !== normalizedUser && String(member.user_id || '') !== user.id);
          if (nextMembers.length !== (data.members || []).length) {
            data.members = nextMembers;
            changed = true;
          }
        } else {
          const originalKeys = data.keys || [];
          for (const key of originalKeys) {
            if (String(key.user || '').trim().toLowerCase() === normalizedUser && key.public_key) removedRepoKeys.push(key.public_key);
          }
          const nextKeys = originalKeys.filter((key) => String(key.user || '').trim().toLowerCase() !== normalizedUser);
          const nextInvites = (data.invites || []).filter((invite) => String(invite.user || '').trim().toLowerCase() !== normalizedUser);
          if (nextKeys.length !== (data.keys || []).length) {
            data.keys = nextKeys;
            changed = true;
          }
          if (nextInvites.length !== (data.invites || []).length) {
            data.invites = nextInvites;
            changed = true;
          }
        }
        if (changed) await doc.ref.set(data, {merge: false});
      }
      for (const publicKey of removedRepoKeys) {
        const idx = await members.doc(memberDocID(keyFingerprint(publicKey))).collection('repos').get();
        await Promise.all(idx.docs.map((doc) => doc.ref.delete()));
      }
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/broker/users/invite/create' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const users = await loadBrokerUsers();
      const username = String(body.user || body.username || '').trim();
      if (!username) throw new Error('user is required');
      const role = normalizeBrokerRole(body.broker_role || body.role || 'user');
      if (!validBrokerRole(body.broker_role || body.role || 'user') || role === 'owner') throw new Error('invalid broker role');
      users.data.invites = (users.data.invites || []).filter((invite) => Date.parse(invite.expires_at || '') > Date.now());
      const normalizedUser = username.toLowerCase();
      if (users.data.invites.some((invite) => String(invite.user || '').trim().toLowerCase() === normalizedUser)) throw Object.assign(new Error('broker user invite already pending for user'), {status: 409});
      let user = (users.data.users || []).find((item) => String(item.username || '').toLowerCase() === normalizedUser);
      if (!user) {
        user = {id: 'u_' + crypto.randomBytes(6).toString('hex'), username, broker_role: role, keys: [], suspended: false, pending: true};
        users.data.users.push(user);
      } else {
        user.username = username;
        user.broker_role = role;
        if (!(user.keys || []).length) user.pending = true;
      }
      const token = crypto.randomBytes(24).toString('base64url');
      const brokerURL = String(body.broker_url || '').trim();
      const expires = new Date(Date.now() + 7 * 24 * 60 * 60 * 1000).toISOString();
      users.data.invites.push({token_hash: ownershipTransferTokenHash(token), user: username, role, broker_url: brokerURL, expires_at: expires});
      await saveBrokerUsers(users);
      const code = brokerUserInviteCode(brokerURL, username, role, token);
      res.status(200).send(JSON.stringify({ok: true, code, accept_command: 'bgit admin accept-broker-invite ' + code, user: username, role}));
      return;
    }
    if (req.path === '/broker/users/invite/accept' && req.method === 'POST') {
      const users = await loadBrokerUsers();
      const signed = await submittedSignedKey(req);
      if (!signed) throw Object.assign(new Error('SSH signature required'), {status: 403});
      const tokenHash = ownershipTransferTokenHash(body.token);
      const invites = users.data.invites || [];
      const invite = invites.find((item) => item.token_hash === tokenHash && Date.parse(item.expires_at || '') > Date.now());
      if (!invite) throw Object.assign(new Error('broker user invite is not pending or has expired'), {status: 404});
      const username = String(invite.user || body.user || '').trim();
      let user = (users.data.users || []).find((item) => String(item.username || '').toLowerCase() === username.toLowerCase());
      if (!user) {
        user = {id: 'u_' + crypto.randomBytes(6).toString('hex'), username, broker_role: invite.role || 'user', keys: [], suspended: false};
        users.data.users.push(user);
      }
      user.username = username;
      user.broker_role = invite.role || user.broker_role || 'user';
      user.pending = false;
      user.keys = user.keys || [];
      if (!assertUniqueBrokerUserKey(users, signed.public_key, username)) user.keys.push({public_key: signed.public_key, source: 'broker-invite'});
      users.data.invites = invites.filter((item) => item.token_hash !== tokenHash);
      await saveBrokerUsers(users);
      await syncAllMembershipIndexes();
      res.status(200).send(JSON.stringify({ok: true, user: username, role: user.broker_role, fingerprint: signed.fingerprint}));
      return;
    }
    if (req.path === '/broker/users/invite/cancel' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const users = await loadBrokerUsers();
      const username = String(body.user || body.username || '').trim().toLowerCase();
      const invites = users.data.invites || [];
      const next = invites.filter((item) => String(item.user || '').trim().toLowerCase() !== username);
      if (next.length === invites.length) throw Object.assign(new Error('broker user invite is not pending or has expired'), {status: 404});
      users.data.invites = next;
      users.data.users = (users.data.users || []).filter((item) => {
        if (String(item.username || '').trim().toLowerCase() !== username) return true;
        return (item.keys || []).length > 0 || !item.pending;
      });
      await saveBrokerUsers(users);
      await syncAllMembershipIndexes();
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/teams/create' && req.method === 'POST') {
      const actor = await requireBrokerAdmin(req);
      const name = String(body.name || '').trim();
      if (!name) throw new Error('team name is required');
      const id = body.id || body.team_id || ('t_' + crypto.randomBytes(6).toString('hex'));
      const ref = repos.doc(teamDocID(id));
      const snap = await ref.get();
      if (snap.exists) throw Object.assign(new Error('team already exists'), {status: 409});
      const team = {id, name, members: [], created_by: actor.user || '', created_at: new Date().toISOString()};
      await ref.set(team, {merge: false});
      res.status(200).send(JSON.stringify({ok: true, team}));
      return;
    }
    if (req.path === '/teams/delete' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const teamID = String(body.team_id || body.id || body.name || '').trim();
      if (!teamID) throw new Error('team_id is required');
      const team = await loadTeam(teamID);
      if (!team) throw Object.assign(new Error('team not found'), {status: 404});
      if (team.data.id === coreTeamID || String(team.data.name || '').trim().toLowerCase() === coreTeamName) {
        throw Object.assign(new Error('core team cannot be deleted'), {status: 403});
      }
      const snap = await repos.get();
      const updates = [];
      snap.forEach((doc) => {
        if (String(doc.id || '').startsWith('_') || String(doc.id || '').startsWith('team:')) return;
        const data = doc.data() || {};
        const teams = data.teams || [];
        const next = teams.filter((item) => (item.id || item.team_id) !== teamID);
        if (next.length !== teams.length) {
          data.teams = next;
          updates.push(saveRepo({ref: doc.ref, data}));
        }
      });
      updates.push(team.ref.delete());
      await Promise.all(updates);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/teams/resolve' && req.method === 'POST') {
      const team = await loadTeamByName(body.name || body.team || body.team_name);
      if (!team) throw Object.assign(new Error('team not found'), {status: 404});
      res.status(200).send(JSON.stringify({team: team.data}));
      return;
    }
    if (req.path === '/teams/list' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const snap = await repos.get();
      res.status(200).send(JSON.stringify({teams: snap.docs.filter((doc) => String(doc.id || '').startsWith('team:')).map((doc) => doc.data() || {})}));
      return;
    }
    if (req.path === '/teams/member/upsert' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const team = await loadTeam(body.team_id);
      if (!team) throw Object.assign(new Error('team not found'), {status: 404});
      const userID = String(body.user_id || '').trim();
      const username = String(body.user || body.username || '').trim();
      const role = normalizeRole(body.role || 'read');
      if (!validRole(role)) throw new Error('invalid role');
      if (!userID && !username) throw new Error('user is required');
      let resolvedUserID = userID;
      if (!resolvedUserID && username) {
        const users = await loadBrokerUsers();
        const user = (users.data.users || []).find((item) => String(item.username || '').toLowerCase() === username.toLowerCase());
        if (!user) throw Object.assign(new Error('broker user not found'), {status: 404});
        resolvedUserID = user.id;
      }
      team.data.members = (team.data.members || []).filter((item) => item.user_id !== resolvedUserID && String(item.username || '').toLowerCase() !== username.toLowerCase());
      team.data.members.push({user_id: resolvedUserID, username, role, suspended: false});
      await team.ref.set(team.data, {merge: false});
      await syncReposForTeam(team.data.id);
      res.status(200).send(JSON.stringify({ok: true, team: team.data}));
      return;
    }
    if (req.path === '/teams/member/remove' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const team = await loadTeam(body.team_id);
      if (!team) throw Object.assign(new Error('team not found'), {status: 404});
      const userID = String(body.user_id || '').trim();
      const username = String(body.user || body.username || '').trim().toLowerCase();
      team.data.members = (team.data.members || []).filter((item) => item.user_id !== userID && String(item.username || '').toLowerCase() !== username);
      await team.ref.set(team.data, {merge: false});
      await syncReposForTeam(team.data.id);
      res.status(200).send(JSON.stringify({ok: true, team: team.data}));
      return;
    }
    if (req.path === '/repos/create' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      if (await loadExistingRepo(body.repo)) throw Object.assign(new Error('repository already exists'), {status: 409});
      const entry = await loadRepo(body.repo);
      const user = body.admin_user || 'admin';
      const role = normalizeRole(body.role || 'developer');
      if (!validRole(role)) throw new Error('invalid role');
      entry.data.repo = {...(entry.data.repo || {}), ...(body.repo || {})};
      if (body.repo && body.repo.team_id) {
        const team = await loadTeam(body.repo.team_id);
        if (!team) throw Object.assign(new Error('team not found'), {status: 404});
        entry.data.teams = entry.data.teams || [];
        entry.data.teams.push({id: body.repo.team_id, role});
      }
      if (body.repo && body.repo.logical && !entry.data.repo.bucket) await ensurePhysicalRepo(entry);
      for (const publicKey of body.public_keys || []) {
        if (!assertUniqueRepoKey(entry, publicKey, user)) entry.data.keys.push({user, role, public_key: publicKey, source: body.source || '', suspended: false});
      }
      audit(entry, {type: 'repo_create', user});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, repo: entry.data.repo, bucket_suffix: entry.data.bucket_suffix}));
      return;
    }
    if (req.path === '/repos/get' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      try {
        await requireRead(req, entry);
      } catch (err) {
        try {
          await requireBrokerAdmin(req);
        } catch (_) {
          throw err;
        }
      }
      res.status(200).send(JSON.stringify({ok: true, repo: entry.data.repo || body.repo, teams: entry.data.teams || []}));
      return;
    }
    if (req.path === '/repos/upsert' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireAdmin(req, entry);
      const user = body.admin_user || 'admin';
      const role = normalizeRole(body.role || 'admin');
      if (!validRole(role)) throw new Error('invalid role');
      entry.data.repo = {...(entry.data.repo || {}), ...(body.repo || {})};
      if (body.repo && body.repo.team_id && !(entry.data.teams || []).find((t) => (t.id || t.team_id) === body.repo.team_id)) {
        const team = await loadTeam(body.repo.team_id);
        if (!team) throw Object.assign(new Error('team not found'), {status: 404});
        entry.data.teams = entry.data.teams || [];
        entry.data.teams.push({id: body.repo.team_id, role: body.role || 'developer'});
      }
      if (body.repo && body.repo.logical && !entry.data.repo.bucket) await ensurePhysicalRepo(entry);
      for (const publicKey of body.public_keys || []) {
        if (!assertUniqueRepoKey(entry, publicKey, user)) entry.data.keys.push({user, role, public_key: publicKey, source: body.source || '', suspended: false});
      }
      audit(entry, {type: 'repo_upsert', user});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, repo: entry.data.repo, bucket_suffix: entry.data.bucket_suffix}));
      return;
    }
    if (req.path === '/repo/teams/list' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireAdmin(req, entry);
      res.status(200).send(JSON.stringify({teams: entry.data.teams || []}));
      return;
    }
    if (req.path === '/repo/teams/upsert' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireAdmin(req, entry);
      const teamID = String(body.team_id || '').trim();
      if (!teamID) throw new Error('team_id is required');
      const team = await loadTeam(teamID);
      if (!team) throw Object.assign(new Error('team not found'), {status: 404});
      const role = normalizeRole(body.role || 'read');
      if (!validRole(role)) throw new Error('invalid role');
      entry.data.teams = (entry.data.teams || []).filter((item) => (item.id || item.team_id) !== teamID);
      entry.data.teams.push({id: teamID, role});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, teams: entry.data.teams}));
      return;
    }
    if (req.path === '/repo/users/list' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireAdmin(req, entry);
      res.status(200).send(JSON.stringify({users: entry.data.user_grants || []}));
      return;
    }
    if (req.path === '/repo/users/upsert' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireBrokerAdmin(req);
      await requireAdmin(req, entry);
      const users = await loadBrokerUsers();
      const wantID = String(body.user_id || '').trim();
      const wantUser = String(body.user || body.username || '').trim().toLowerCase();
      const user = (users.data.users || []).find((item) => (wantID && item.id === wantID) || (wantUser && String(item.username || '').trim().toLowerCase() === wantUser));
      if (!user) throw Object.assign(new Error('broker user not found'), {status: 404});
      const role = normalizeRole(body.role || 'read');
      if (!validRole(role) || role === 'owner') throw new Error('invalid role');
      entry.data.user_grants = (entry.data.user_grants || []).filter((grant) => grant.user_id !== user.id && String(grant.user || grant.username || '').trim().toLowerCase() !== String(user.username || '').trim().toLowerCase());
      entry.data.user_grants.push({user_id: user.id, user: user.username || user.id, role});
      audit(entry, {type: 'repo_user_grant', user: user.username || user.id, role});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, users: entry.data.user_grants}));
      return;
    }
    if (req.path === '/repo/users/remove' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireBrokerAdmin(req);
      await requireAdmin(req, entry);
      const wantID = String(body.user_id || '').trim();
      const wantUser = String(body.user || body.username || '').trim().toLowerCase();
      const before = (entry.data.user_grants || []).length;
      entry.data.user_grants = (entry.data.user_grants || []).filter((grant) => {
        if (wantID && grant.user_id === wantID) return false;
        if (wantUser && String(grant.user || grant.username || '').trim().toLowerCase() === wantUser) return false;
        return true;
      });
      if (entry.data.user_grants.length === before) throw Object.assign(new Error('repo user grant not found'), {status: 404});
      audit(entry, {type: 'repo_user_remove', user: body.user || body.user_id || ''});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, users: entry.data.user_grants || []}));
      return;
    }
    if (req.path === '/repos/list' && req.method === 'POST') {
      await requireBrokerAdmin(req);
      const snap = await repos.get();
      const out = [];
      snap.forEach((doc) => {
        if (String(doc.id || '').startsWith('_') || String(doc.id || '').startsWith('team:')) return;
        const data = doc.data() || {};
        const repo = data.repo || {};
        if (!repo.logical) return;
        out.push({repo, logical: repo.logical, teams: data.teams || []});
      });
      out.sort((a, b) => String(a.logical || '').localeCompare(String(b.logical || '')));
      res.status(200).send(JSON.stringify({repos: out}));
      return;
    }
    if (req.path === '/repo/teams/remove' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireAdmin(req, entry);
      const teamID = String(body.team_id || '').trim();
      entry.data.teams = (entry.data.teams || []).filter((item) => (item.id || item.team_id) !== teamID);
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, teams: entry.data.teams || []}));
      return;
    }
    if (req.path === '/repo/info' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireRead(req, entry);
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
      await requireAdmin(req, entry);
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
      const key = await requireOwner(req, entry);
      const logical = normalizeLogicalRepo(body.logical);
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
      await requireOwner(req, entry);
      const repo = await ensurePhysicalRepo(entry);
      await deletePhysicalRepo(repo);
      await deleteRepoMetadata(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/keys/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireAdmin(req, entry);
      res.status(200).send(JSON.stringify({keys: entry.data.keys}));
      return;
    }
    if (req.path === '/keys/add' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireAdmin(req, entry);
      const user = body.user || 'admin';
      const role = normalizeRole(body.role || 'read');
      if (!validRole(role) || role === 'owner') throw new Error('invalid role');
      for (const publicKey of body.public_keys || []) {
        if (!assertUniqueRepoKey(entry, publicKey, user)) entry.data.keys.push({user, role, public_key: publicKey, source: body.source || '', suspended: false});
      }
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/keys/invite/create' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireAdmin(req, entry);
      const user = String(body.user || '').trim();
      const role = normalizeRole(body.role || 'read');
      if (!user) throw new Error('user is required');
      if (!validRole(role) || role === 'owner') throw new Error('invalid role');
      const token = crypto.randomBytes(24).toString('base64url');
      const brokerURL = String(body.broker_url || '').trim();
      const expires = new Date(Date.now() + 7 * 24 * 60 * 60 * 1000).toISOString();
      entry.data.invites = (entry.data.invites || []).filter((invite) => Date.parse(invite.expires_at || '') > Date.now());
      const normalizedUser = user.toLowerCase();
      if (entry.data.invites.some((invite) => String(invite.user || '').trim().toLowerCase() === normalizedUser)) throw Object.assign(new Error('invite already pending for user'), {status: 409});
      entry.data.invites.push({token_hash: ownershipTransferTokenHash(token), user, role, broker_url: brokerURL, expires_at: expires});
      const code = memberInviteCode(brokerURL, entry.data.repo || body.repo, token);
      audit(entry, {type: 'member_invite_create', user, role});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, code, accept_command: 'bgit admin accept-invite ' + code}));
      return;
    }
    if (req.path === '/keys/invite/list' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireAdmin(req, entry);
      entry.data.invites = (entry.data.invites || []).filter((invite) => Date.parse(invite.expires_at || '') > Date.now());
      await saveRepo(entry);
      const invites = (entry.data.invites || []).map((invite) => ({user: invite.user || '', role: invite.role || 'read', expires_at: invite.expires_at || ''}));
      res.status(200).send(JSON.stringify({invites}));
      return;
    }
    if (req.path === '/keys/invite/accept' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const signed = await submittedSignedKey(req);
      if (!signed) throw Object.assign(new Error('SSH signature required'), {status: 403});
      const tokenHash = ownershipTransferTokenHash(body.token);
      const invites = entry.data.invites || [];
      const invite = invites.find((item) => item.token_hash === tokenHash && Date.parse(item.expires_at || '') > Date.now());
      if (!invite) throw Object.assign(new Error('invite is not pending or has expired'), {status: 404});
      const existing = assertUniqueRepoKey(entry, signed.public_key, invite.user);
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
    if (req.path === '/keys/invite/cancel' && req.method === 'POST') {
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      await requireAdmin(req, entry);
      const invites = entry.data.invites || [];
      const user = String(body.user || '').trim().toLowerCase();
      const tokenHash = body.token ? ownershipTransferTokenHash(body.token) : '';
      const next = invites.filter((item) => {
        if (tokenHash) return item.token_hash !== tokenHash;
        return String(item.user || '').trim().toLowerCase() !== user;
      });
      if (next.length === invites.length) throw Object.assign(new Error('invite is not pending or has expired'), {status: 404});
      entry.data.invites = next;
      audit(entry, {type: 'member_invite_cancel'});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if ((req.path === '/keys/remove' || req.path === '/keys/suspend') && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireAdmin(req, entry);
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
      await requireAdmin(req, entry);
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
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      const key = await requireOwner(req, entry);
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
      const entry = await loadExistingRepo(body.repo);
      if (!entry) throw Object.assign(new Error('repository not found'), {status: 404});
      const key = await requireOwner(req, entry);
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
      const accepted = await submittedSignedKey(req);
      if (!accepted) throw Object.assign(new Error('SSH signature required'), {status: 403});
      const user = String(body.user || 'owner').trim() || 'owner';
      const ownerFingerprint = transfer.requested_by_fingerprint || '';
      for (const item of entry.data.keys || []) {
        if (item.role === 'owner' && keyFingerprint(item.public_key) === ownerFingerprint) item.role = 'admin';
      }
      const existing = assertUniqueRepoKey(entry, accepted.public_key, user);
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
      await requireAdmin(req, entry);
      res.status(200).send(JSON.stringify({protections: entry.data.protections || []}));
      return;
    }
    if (req.path === '/protection/upsert' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireAdmin(req, entry);
      entry.data.protections = (entry.data.protections || []).filter((p) => p.ref !== body.ref);
      entry.data.protections.push({ref: body.ref, require_pr: body.require_pr !== false, allow_overrides: !!body.allow_overrides});
      audit(entry, {type: 'protection_upsert', ref: body.ref});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/protection/remove' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireAdmin(req, entry);
      entry.data.protections = (entry.data.protections || []).filter((p) => p.ref !== body.ref);
      audit(entry, {type: 'protection_remove', ref: body.ref});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/issues/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      if (entry.data.issues_enabled === false) throw Object.assign(new Error('issues are disabled'), {status: 403});
      let issues = await listIssues(entry);
      const type = String(body.type || '').trim().toLowerCase();
      if (type) issues = issues.filter((issue) => String(issue.type || '').toLowerCase() === type);
      if (type === 'story' && !body.include_archived) issues = issues.filter((issue) => !issue.archived);
      res.status(200).send(JSON.stringify({issues}));
      return;
    }
    if (req.path === '/issues/view' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      if (entry.data.issues_enabled === false) throw Object.assign(new Error('issues are disabled'), {status: 403});
      const issue = await loadIssue(entry, body.id);
      if (!issue) throw Object.assign(new Error('issue not found'), {status: 404});
      res.status(200).send(JSON.stringify({issue}));
      return;
    }
    if (req.path === '/issues/create' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      if (entry.data.issues_enabled === false) throw Object.assign(new Error('issues are disabled'), {status: 403});
      const story = String(body.type || '').trim().toLowerCase() === 'story';
      const key = story ? await requireBoardMutation(req, entry) : await requireIssueCreate(req, entry);
      const issueBody = String(body.body || '').trim();
      const title = String(body.title || '').trim() || (story ? issueBody.replace(/\s+/g, ' ').slice(0, 80) : '');
      if (!title) throw new Error(story ? 'story is required' : 'issue title is required');
      const lane = story ? normalizeBoardLane(body.lane) : '';
      const issue = {id: nextIssueID(entry.data), type: story ? 'story' : 'issue', title, body: issueBody, status: 'open', lane, assignee: '', position: story ? await nextStoryPosition(entry, lane) : 0, archived: false, author: key.user || 'anonymous', comments: [], history: [], created_at: new Date().toISOString(), updated_at: new Date().toISOString()};
      if (story) issueHistory(issue, key.user, 'created', {to: lane});
      await saveRepo(entry);
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/issues/comment' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      if (entry.data.issues_enabled === false) throw Object.assign(new Error('issues are disabled'), {status: 403});
      const issue = await loadIssue(entry, body.id);
      if (!issue) throw Object.assign(new Error('issue not found'), {status: 404});
      const key = issue.type === 'story' ? await requireBoardMutation(req, entry) : await requireIssueCreate(req, entry);
      const comment = String(body.comment || '').trim();
      if (!comment) throw new Error('comment is required');
      issue.comments = issue.comments || [];
      issue.comments.push({user: key.user || 'anonymous', body: comment, at: new Date().toISOString()});
      if (issue.type === 'story') issueHistory(issue, key.user, 'commented');
      issue.updated_at = new Date().toISOString();
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/issues/assignees' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireBoardMutation(req, entry);
      res.status(200).send(JSON.stringify({users: await boardAssignableUsers(entry)}));
      return;
    }
    if (req.path === '/issues/update' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      if (entry.data.issues_enabled === false) throw Object.assign(new Error('issues are disabled'), {status: 403});
      const key = await requireBoardMutation(req, entry);
      const issue = await loadIssue(entry, body.id);
      if (!issue || issue.type !== 'story') throw Object.assign(new Error('story not found'), {status: 404});
      const issueBody = String(body.body || '').trim();
      const title = String(body.title || '').trim() || issueBody.replace(/\s+/g, ' ').slice(0, 80);
      if (!title) throw new Error('story is required');
      issue.title = title;
      issue.body = issueBody;
      issue.updated_at = new Date().toISOString();
      issueHistory(issue, key.user, 'edited');
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/issues/archive' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = await requireBoardMutation(req, entry);
      const issue = await loadIssue(entry, body.id);
      if (!issue || issue.type !== 'story') throw Object.assign(new Error('story not found'), {status: 404});
      const archived = !!body.archived;
      issue.archived = archived;
      issue.updated_at = new Date().toISOString();
      issueHistory(issue, key.user, archived ? 'archived' : 'unarchived');
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/issues/reorder' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = await requireBoardMutation(req, entry);
      const issue = await loadIssue(entry, body.id);
      if (!issue || issue.type !== 'story') throw Object.assign(new Error('story not found'), {status: 404});
      const fromLane = normalizeBoardLane(issue.lane);
      const lane = normalizeBoardLane(body.lane || issue.lane);
      issue.lane = lane;
      issue.position = await storyPositionAfter(entry, lane, body.after_id, issue.id);
      issue.updated_at = new Date().toISOString();
      issueHistory(issue, key.user, 'reordered', {from: fromLane, to: lane, position: String(issue.position)});
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/issues/move' || req.path === '/issues/take' || req.path === '/issues/assign') {
      const entry = await ensureRepo(body.repo);
      const key = await requireBoardMutation(req, entry);
      const issue = await loadIssue(entry, body.id);
      if (!issue) throw Object.assign(new Error('issue not found'), {status: 404});
      if (issue.type !== 'story') throw Object.assign(new Error('story not found'), {status: 404});
      const fromLane = normalizeBoardLane(issue.lane);
      const fromAssignee = issue.assignee || '';
      if (req.path === '/issues/move') {
        const lane = normalizeBoardLane(body.lane);
        issue.lane = lane;
        if (Object.prototype.hasOwnProperty.call(body, 'after_id')) issue.position = await storyPositionAfter(entry, lane, body.after_id, issue.id);
        else if (fromLane !== lane || !storyPosition(issue)) issue.position = await nextStoryPosition(entry, lane, issue.id);
        issueHistory(issue, key.user, 'moved', {from: fromLane, to: lane});
      }
      else if (req.path === '/issues/take') {
        issue.assignee = key.user || '';
        if (normalizeBoardLane(issue.lane) === 'backlog') issue.lane = 'doing';
        if (fromLane !== normalizeBoardLane(issue.lane) || !storyPosition(issue)) issue.position = await nextStoryPosition(entry, normalizeBoardLane(issue.lane), issue.id);
        issueHistory(issue, key.user, 'assigned', {from: fromAssignee, to: issue.assignee || ''});
      } else {
        const assignee = String(body.assignee || '').trim();
        if (assignee) {
          const users = await boardAssignableUsers(entry);
          const canonical = users.find((user) => user.toLowerCase() === assignee.toLowerCase());
          if (!canonical) throw Object.assign(new Error('assignee is not assignable'), {status: 400});
          issue.assignee = canonical;
        } else {
          issue.assignee = '';
        }
        issueHistory(issue, key.user, 'assigned', {from: fromAssignee, to: issue.assignee || ''});
      }
      issue.updated_at = new Date().toISOString();
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/issues/close' || req.path === '/issues/reopen') {
      const entry = await ensureRepo(body.repo);
      await requireWrite(req, entry);
      const issue = await loadIssue(entry, body.id);
      if (!issue) throw Object.assign(new Error('issue not found'), {status: 404});
      issue.status = req.path === '/issues/reopen' ? 'open' : 'closed';
      issue.updated_at = new Date().toISOString();
      await saveIssue(entry, issue);
      res.status(200).send(JSON.stringify({ok: true, issue}));
      return;
    }
    if (req.path === '/ci/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      await refreshCIRuns(entry);
      res.status(200).send(JSON.stringify({runs: listCIRuns(entry.data)}));
      return;
    }
    if (req.path === '/ci/view' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      const run = findCIRun(entry.data, body.id);
      if (!run) throw Object.assign(new Error('CI run not found'), {status: 404});
      if (await refreshCIRun(run)) await saveRepo(entry);
      res.status(200).send(JSON.stringify({run}));
      return;
    }
    if (req.path === '/ci/logs' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      const run = findCIRun(entry.data, body.id);
      if (!run) throw Object.assign(new Error('CI run not found'), {status: 404});
      if (await refreshCIRun(run)) await saveRepo(entry);
      const logs = await ciRunLogs(run);
      res.status(200).send(JSON.stringify({run, logs}));
      return;
    }
    if (req.path === '/ci/run' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = await requireWrite(req, entry);
      const repo = entry.data.repo || {};
      const provider = normalizeCIProvider(body.provider, repo);
      const ref = normalizeCIRef(body.ref || repo.default_branch || 'refs/heads/main');
      const refs = entry.data.refs || {};
      const commit = String(body.commit || refs[ref] || '').trim();
      if (!commit || !/^[0-9a-f]{40}$/i.test(commit)) throw Object.assign(new Error('CI commit is required'), {status: 400});
      if (!refs[ref]) throw Object.assign(new Error('CI ref not found'), {status: 404});
      if (refs[ref] !== commit && repo.bucket && repo.prefix) {
        try {
          const physicalRef = (await readTextObject(repo, ref)).trim();
          if (physicalRef === commit) {
            refs[ref] = commit;
            entry.data.refs = refs;
          }
        } catch (_) {}
      }
      if (refs[ref] !== commit) throw Object.assign(new Error('CI commit does not match broker ref'), {status: 409});
      const config = cleanCIConfig(body.config, provider);
      const now = new Date().toISOString();
      const run = {id: nextCIID(entry.data), provider, ref, commit, config, status: 'queued', url: '', message: '', author: key.user || '', created_at: now, updated_at: now};
      await triggerCIRun(entry, run);
      run.updated_at = new Date().toISOString();
      entry.data.ci_runs = [run, ...(entry.data.ci_runs || [])].slice(0, 100);
      audit(entry, {type: 'ci_run', id: run.id, provider, ref, commit, config, user: key.user});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true, run}));
      return;
    }
    if (req.path === '/ci/secret/rotate' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = await requireAdmin(req, entry);
      await rotateCIMaterializerToken();
      audit(entry, {type: 'ci_secret_rotate', user: key.user});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    if (req.path === '/prs/create' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = await requireWrite(req, entry);
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
      await requireRead(req, entry);
      res.status(200).send(JSON.stringify({prs: await listPRs(entry)}));
      return;
    }
    if (req.path === '/prs/sync' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      res.status(200).send(JSON.stringify(await syncPRRecords(entry, body.known || {})));
      return;
    }
    if (req.path === '/prs/view' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      const pr = await loadPR(entry, body.id);
      if (!pr) throw Object.assign(new Error('pull request not found'), {status: 404});
      res.status(200).send(JSON.stringify({pr}));
      return;
    }
    if (req.path === '/prs/close' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = await requireWrite(req, entry);
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
      const key = await requireWrite(req, entry);
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
      const key = await requireRead(req, entry);
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
      const key = await requireRead(req, entry);
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
      const key = await requireWrite(req, entry);
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
      const key = await effectiveSignedKey(req, entry);
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
      const key = await effectiveSignedKey(req, entry);
      const operation = body.operation || '';
      const allowed = (operation === 'read' && repoIsPublic(entry)) || (!!key && roleAllows(key.role, operation));
      res.status(200).send(JSON.stringify({allowed, user: key && key.user || (allowed ? 'anonymous' : ''), role: key && key.role || (allowed ? 'read' : '')}));
      return;
    }
    if (req.path === '/auth/status' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      let key = await effectiveSignedKey(req, entry) || (repoIsPublic(entry) ? anonymousKey() : null);
      const brokerAdmin = await requireBrokerAdmin(req).catch(() => null);
      if (!key && brokerAdmin) {
        key = {user: brokerAdmin.user || '', role: '', broker_role: brokerAdmin.broker_role || 'admin', public_key: brokerAdmin.public_key || '', source: 'broker-admin'};
      }
      if (!key) throw Object.assign(new Error('SSH signature required'), {status: 403});
      const capabilities = roleCapabilities(key.role || '');
      if (brokerAdmin) {
        capabilities.admin_keys = true;
        capabilities.manage_protection = true;
        capabilities.broker_upgrade = true;
      }
      res.status(200).send(JSON.stringify({
        broker_version: brokerVersion,
        repo: entry.data.repo || body.repo,
        identity: {user: key.user || '', source: key.source || '', key_fingerprint: key.public_key ? keyFingerprint(key.public_key) : '', public_key: key.public_key || ''},
        user: key.user || '',
        role: key.role || '',
        capabilities,
        resolved_at: new Date().toISOString(),
      }));
      return;
    }
    if ((req.path === '/profile/get' || req.path === '/profile/update') && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = await effectiveSignedKey(req, entry);
      if (!key) throw Object.assign(new Error('SSH signature required'), {status: 403});
      const users = await loadBrokerUsers();
      const user = ensureProfileUser(users, key);
      if (req.path === '/profile/update') {
        user.profile.bio = String(body.bio || '').trim().slice(0, 2000);
        user.profile.avatar = normalizeProfileAvatar(body.avatar || '');
        await saveBrokerUsers(users);
      }
      res.status(200).send(JSON.stringify({user: user.username || key.user || '', profile: publicProfileForUser(user), keys: profileKeysForUser(entry, users, key)}));
      return;
    }
    if (req.path === '/repos/mine' && req.method === 'POST') {
      const fingerprint = req.get('x-bgit-key-fingerprint') || '';
      if (!fingerprint) throw Object.assign(new Error('SSH signature required'), {status: 403});
      const signed = await submittedSignedKey(req);
      if (!signed || signed.fingerprint !== fingerprint) throw Object.assign(new Error('SSH signature required'), {status: 403});
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
        await requireAdmin(req, entry);
        await syncMembershipIndex(entry);
        res.status(200).send(JSON.stringify({ok: true, repositories: 1}));
        return;
      }
      const owners = await loadOwners();
      if (!await verifySignature(req, owners)) throw Object.assign(new Error('owner SSH signature required'), {status: 403});
      const count = await syncAllMembershipIndexes();
      res.status(200).send(JSON.stringify({ok: true, repositories: count}));
      return;
    }
    if (req.path === '/objects/capability' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const operation = body.operation || 'read';
      const key = operation === 'read' ? await requireRead(req, entry) : await requireWrite(req, entry);
      const repo = await ensurePhysicalRepo(entry);
      const capabilityPath = validateCapabilityPath(operation, body.path);
      const capability = body.resumable ? await resumableUpload(repo, capabilityPath) : await signedURL(repo, capabilityPath, operation);
      audit(entry, {type: 'capability_issued', operation, path: capabilityPath, user: key.user, role: key.role});
      await saveRepo(entry);
      res.status(200).send(JSON.stringify({...capability, bucket: repo.bucket, prefix: repo.prefix, object: objectName(repo, capabilityPath)}));
      return;
    }
    if (req.path === '/objects/read' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      const repo = await ensurePhysicalRepo(entry);
      const data = await readObject(repo, body.path);
      res.status(200).send(JSON.stringify({data}));
      return;
    }
    if (req.path === '/objects/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      const repo = await ensurePhysicalRepo(entry);
      const paths = await listObjects(repo, body.prefix);
      res.status(200).send(JSON.stringify({paths}));
      return;
    }
    if (req.path === '/refs/list' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      await requireRead(req, entry);
      res.status(200).send(JSON.stringify({refs: entry.data.refs || {}}));
      return;
    }
    if (req.path === '/refs/update' && req.method === 'POST') {
      const entry = await ensureRepo(body.repo);
      const key = await requireWrite(req, entry);
      await updateRefCAS(body.repo, body.ref, body.old, body.new, key, {override: !!body.override});
      res.status(200).send(JSON.stringify({ok: true}));
      return;
    }
    res.status(404).send(JSON.stringify({error: 'unknown broker endpoint'}));
  } catch (err) {
    res.status(err.status || 500).send(JSON.stringify({error: err.message || String(err)}));
  }
};

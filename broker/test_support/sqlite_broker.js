'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');
const {execFileSync} = require('child_process');
const {Readable} = require('stream');
const {DatabaseSync} = require('node:sqlite');

function ensureDir(dir) {
  fs.mkdirSync(dir, {recursive: true});
}

function safeJoin(root, ...parts) {
  const base = path.resolve(root);
  const target = path.resolve(base, ...parts);
  if (target !== base && !target.startsWith(base + path.sep)) {
    throw Object.assign(new Error('invalid object path'), {statusCode: 400});
  }
  return target;
}

function encodeObjectName(name) {
  return Buffer.from(String(name || ''), 'utf8').toString('base64url');
}

function decodeObjectName(name) {
  return Buffer.from(String(name || ''), 'base64url').toString('utf8');
}

class SQLiteStore {
  constructor(file) {
    ensureDir(path.dirname(file));
    this.db = new DatabaseSync(file);
    this.db.exec(`
      create table if not exists documents (
        path text primary key,
        data text not null
      );
      create table if not exists aws_items (
        table_name text not null,
        pk text not null,
        data text not null,
        primary key (table_name, pk)
      );
    `);
  }

  getDocument(docPath) {
    const row = this.db.prepare('select data from documents where path = ?').get(docPath);
    return row ? JSON.parse(row.data) : null;
  }

  setDocument(docPath, data, merge) {
    const current = merge ? this.getDocument(docPath) : null;
    const next = current ? {...current, ...data} : data;
    this.db.prepare('insert or replace into documents(path, data) values (?, ?)').run(docPath, JSON.stringify(next || {}));
  }

  deleteDocument(docPath) {
    this.db.prepare('delete from documents where path = ?').run(docPath);
  }

  listCollection(collectionPath) {
    const prefix = collectionPath.replace(/\/+$/g, '') + '/';
    const rows = this.db.prepare('select path, data from documents where path like ?').all(prefix + '%');
    return rows
      .filter((row) => !row.path.slice(prefix.length).includes('/'))
      .map((row) => ({id: row.path.slice(prefix.length), path: row.path, data: JSON.parse(row.data)}));
  }

  awsGet(tableName, pk) {
    const row = this.db.prepare('select data from aws_items where table_name = ? and pk = ?').get(tableName, pk);
    return row ? JSON.parse(row.data) : null;
  }

  awsPut(tableName, pk, data) {
    this.db.prepare('insert or replace into aws_items(table_name, pk, data) values (?, ?, ?)').run(tableName, pk, JSON.stringify(data || {}));
  }

  awsDelete(tableName, pk) {
    this.db.prepare('delete from aws_items where table_name = ? and pk = ?').run(tableName, pk);
  }

  awsScan(tableName) {
    return this.db.prepare('select data from aws_items where table_name = ?').all(tableName).map((row) => JSON.parse(row.data));
  }

  awsQuery(tableName, prefix) {
    return this.awsScan(tableName).filter((item) => String(item.__pk || '').startsWith(prefix));
  }
}

const brokerStatePrefix = '.bucketgit/broker-state/v1';

function stateKey(repo, suffix) {
  const prefix = String(repo && repo.prefix || '').replace(/^\/+|\/+$/g, '');
  return [prefix, brokerStatePrefix, suffix].filter(Boolean).join('/');
}

function repoFromTableData(data) {
  const repo = data && data.repo || {};
  if (!repo.bucket) return null;
  return repo;
}

function brokerRepoID(repo) {
  if (repo && repo.logical) return ['logical', repo.team_id || 't_core', repo.logical].join(':');
  return [repo && repo.provider || 's3', repo && repo.bucket || '', repo && repo.prefix || ''].join(':');
}

function readJSONFromRepo(objectRoot, repo, suffix) {
  try {
    const target = localObjectTarget(objectRoot, repo.bucket);
    const raw = target.backend.get(target.bucket, stateKey(repo, suffix));
    return JSON.parse(Buffer.from(raw).toString('utf8') || '{}');
  } catch (_) {
    return null;
  }
}

function writeJSONToRepo(objectRoot, repo, suffix, data) {
  const target = localObjectTarget(objectRoot, repo.bucket);
  target.backend.ensureBucket(target.bucket);
  target.backend.put(target.bucket, stateKey(repo, suffix), Buffer.from(JSON.stringify(data || {}, null, 2)));
}

function deleteJSONFromRepo(objectRoot, repo, suffix) {
  try {
    const target = localObjectTarget(objectRoot, repo.bucket);
    target.backend.delete(target.bucket, stateKey(repo, suffix));
  } catch (_) {}
}

function tableObjectSuffix(tableName, pk) {
  return `tables/${encodeURIComponent(tableName)}/${encodeObjectName(pk)}.json`;
}

class StorageBackedStore {
  constructor(file, objectRoot) {
    this.local = new SQLiteStore(file);
    this.objectRoot = objectRoot;
    this.repoIndex = new Map();
    for (const item of this.local.awsScan('*repo-index*')) {
      if (item && item.repo_id && item.repo) this.repoIndex.set(item.repo_id.S || item.repo_id, item.repo);
    }
  }

  getDocument(docPath) { return this.local.getDocument(docPath); }
  setDocument(docPath, data, merge) { return this.local.setDocument(docPath, data, merge); }
  deleteDocument(docPath) { return this.local.deleteDocument(docPath); }
  listCollection(collectionPath) { return this.local.listCollection(collectionPath); }

  rememberRepo(repoID, repo) {
    if (!repoID || !repo || !repo.bucket) return;
    this.repoIndex.set(repoID, repo);
    this.local.awsPut('*repo-index*', repoID, {repo_id: repoID, repo});
  }

  importRepo(repo) {
    if (!repo || !repo.bucket) return null;
    const item = readJSONFromRepo(this.objectRoot, repo, 'repo-table-item.json');
    if (!item || !item.id || !item.data) return null;
    this.local.awsPut(process.env.TABLE_NAME || 'bgit-local-repos', item.id.S, item);
    const data = JSON.parse(item.data.S || '{}');
    if (data && data.repo) {
      this.rememberRepo(item.id.S, data.repo);
      this.rememberRepo(brokerRepoID(data.repo), data.repo);
    }
    this.importRepoTable(process.env.PR_TABLE_NAME || 'bgit-local-prs', item.id.S, data.repo || repo);
    this.importRepoTable(process.env.MEMBER_TABLE_NAME || 'bgit-local-members', item.id.S, data.repo || repo);
    return item;
  }

  importRepoTable(tableName, repoID, repo) {
    const target = localObjectTarget(this.objectRoot, repo.bucket);
    const prefix = stateKey(repo, `tables/${encodeURIComponent(tableName)}/`);
    for (const key of target.backend.list(target.bucket, prefix)) {
      try {
        const raw = target.backend.get(target.bucket, key);
        const item = JSON.parse(Buffer.from(raw).toString('utf8') || '{}');
        const pk = awsItemPK(item);
        if (pk) this.local.awsPut(tableName, pk, item);
      } catch (_) {}
    }
  }

  importRepoByID(repoID) {
    const repo = this.repoIndex.get(repoID);
    if (repo) this.importRepo(repo);
  }

  mirrorItem(tableName, pk, item) {
    let data = null;
    try {
      data = JSON.parse(item && item.data && item.data.S || '{}');
    } catch (_) {}
    const repo = repoFromTableData(data);
    if (repo && item.id && item.id.S) {
      this.rememberRepo(item.id.S, repo);
      this.rememberRepo(brokerRepoID(repo), repo);
      writeJSONToRepo(this.objectRoot, repo, 'repo-table-item.json', item);
      return;
    }
    const repoID = item && item.repo_id && item.repo_id.S;
    const repoForTable = repoID ? this.repoIndex.get(repoID) : repo;
    if (repoForTable) writeJSONToRepo(this.objectRoot, repoForTable, tableObjectSuffix(tableName, pk), item);
  }

  awsGet(tableName, pk) {
    let item = this.local.awsGet(tableName, pk);
    if (item) return item;
    if (tableName === (process.env.TABLE_NAME || 'bgit-local-repos')) {
      for (const repo of this.repoIndex.values()) {
        const imported = this.importRepo(repo);
        if (imported && imported.id && imported.id.S === pk) return imported;
      }
    }
    if (tableName === (process.env.PR_TABLE_NAME || 'bgit-local-prs') || tableName === (process.env.MEMBER_TABLE_NAME || 'bgit-local-members')) {
      const parts = pk.split('#');
      const repoID = tableName === (process.env.PR_TABLE_NAME || 'bgit-local-prs') ? parts.slice(0, -1).join('#') : parts.slice(1).join('#');
      this.importRepoByID(repoID);
      item = this.local.awsGet(tableName, pk);
    }
    return item;
  }

  awsPut(tableName, pk, data) {
    this.local.awsPut(tableName, pk, data);
    this.mirrorItem(tableName, pk, data || {});
  }

  awsDelete(tableName, pk) {
    const item = this.local.awsGet(tableName, pk);
    this.local.awsDelete(tableName, pk);
    if (!item) return;
    let data = null;
    try {
      data = JSON.parse(item && item.data && item.data.S || '{}');
    } catch (_) {}
    const repo = repoFromTableData(data);
    if (repo && item.id && item.id.S) {
      deleteJSONFromRepo(this.objectRoot, repo, 'repo-table-item.json');
      this.repoIndex.delete(item.id.S);
      this.local.awsDelete('*repo-index*', item.id.S);
      return;
    }
    const repoID = item && item.repo_id && item.repo_id.S;
    const repoForTable = repoID ? this.repoIndex.get(repoID) : null;
    if (repoForTable) deleteJSONFromRepo(this.objectRoot, repoForTable, tableObjectSuffix(tableName, pk));
  }

  awsScan(tableName) {
    for (const repo of this.repoIndex.values()) this.importRepo(repo);
    return this.local.awsScan(tableName);
  }

  awsQuery(tableName, prefix) {
    if (tableName === (process.env.PR_TABLE_NAME || 'bgit-local-prs')) this.importRepoByID(prefix.replace(/#$/, ''));
    if (tableName === (process.env.MEMBER_TABLE_NAME || 'bgit-local-members')) {
      for (const repo of this.repoIndex.values()) this.importRepo(repo);
    }
    return this.local.awsQuery(tableName, prefix);
  }
}

class FakeSnapshot {
  constructor(ref, value) {
    this.ref = ref;
    this.id = ref.id;
    this.exists = value !== null && value !== undefined;
    this._value = value || {};
  }
  data() {
    return JSON.parse(JSON.stringify(this._value));
  }
}

class FakeQuerySnapshot {
  constructor(docs) {
    this.docs = docs;
  }
  forEach(fn) {
    this.docs.forEach(fn);
  }
}

class FakeDocumentRef {
  constructor(store, docPath) {
    this._store = store;
    this.path = docPath;
    this.id = docPath.split('/').pop();
  }
  async get() {
    return new FakeSnapshot(this, this._store.getDocument(this.path));
  }
  async set(data, opts = {}) {
    this._store.setDocument(this.path, data || {}, !!opts.merge);
  }
  async delete() {
    this._store.deleteDocument(this.path);
  }
  collection(name) {
    return new FakeCollectionRef(this._store, this.path + '/' + name);
  }
}

class FakeCollectionRef {
  constructor(store, collectionPath) {
    this._store = store;
    this.path = collectionPath.replace(/\/+$/g, '');
    this._orderBy = null;
    this._direction = 'asc';
  }
  doc(id) {
    return new FakeDocumentRef(this._store, this.path + '/' + id);
  }
  orderBy(field, direction) {
    const next = new FakeCollectionRef(this._store, this.path);
    next._orderBy = field;
    next._direction = direction || 'asc';
    return next;
  }
  async get() {
    let docs = this._store.listCollection(this.path).map((row) => new FakeSnapshot(new FakeDocumentRef(this._store, row.path), row.data));
    if (this._orderBy) {
      const field = this._orderBy;
      const mult = this._direction === 'desc' ? -1 : 1;
      docs = docs.sort((a, b) => {
        const av = a.data()[field];
        const bv = b.data()[field];
        return av === bv ? 0 : av > bv ? mult : -mult;
      });
    }
    return new FakeQuerySnapshot(docs);
  }
}

class FakeFirestore {
  constructor() {
    this._store = new SQLiteStore(process.env.BROKER_TEST_SQLITE || path.join(process.cwd(), 'broker-test.sqlite'));
  }
  collection(name) {
    return new FakeCollectionRef(this._store, name);
  }
  async runTransaction(fn) {
    const tx = {
      get: (ref) => ref.get(),
      set: (ref, data, opts) => ref.set(data, opts),
    };
    return fn(tx);
  }
}

class FakeFile {
  constructor(root, bucket, name) {
    this.root = root;
    this.bucket = bucket;
    this.name = name;
  }
  diskPath() {
    return safeJoin(this.root, this.bucket, this.name);
  }
  async download() {
    return [fs.readFileSync(this.diskPath())];
  }
  async save(value) {
    ensureDir(path.dirname(this.diskPath()));
    fs.writeFileSync(this.diskPath(), value);
  }
  async delete() {
    fs.rmSync(this.diskPath(), {force: true});
  }
  async getSignedUrl(opts = {}) {
    const method = opts.method || (opts.action === 'write' ? 'PUT' : opts.action === 'delete' ? 'DELETE' : 'GET');
    return [`${process.env.BROKER_TEST_BASE_URL}/_objects/${encodeURIComponent(this.bucket)}/${encodeObjectName(this.name)}?method=${encodeURIComponent(method)}`];
  }
  async createResumableUpload() {
    return [`${process.env.BROKER_TEST_BASE_URL}/_objects/${encodeURIComponent(this.bucket)}/${encodeObjectName(this.name)}?method=PUT`];
  }
}

class FakeBucket {
  constructor(root, name) {
    this.root = root;
    this.name = name;
  }
  file(name) {
    return new FakeFile(this.root, this.name, name);
  }
  async get() {
    ensureDir(safeJoin(this.root, this.name));
    return [this];
  }
  async getFiles(opts = {}) {
    const bucketRoot = safeJoin(this.root, this.name);
    const prefix = opts.prefix || '';
    if (!fs.existsSync(bucketRoot)) return [[]];
    const out = [];
    const walk = (dir) => {
      for (const entry of fs.readdirSync(dir, {withFileTypes: true})) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) {
          walk(full);
        } else {
          const rel = path.relative(bucketRoot, full).split(path.sep).join('/');
          if (rel.startsWith(prefix)) out.push({name: rel, delete: async () => fs.rmSync(full, {force: true})});
        }
      }
    };
    walk(bucketRoot);
    return [out];
  }
  async delete() {
    fs.rmSync(safeJoin(this.root, this.name), {recursive: true, force: true});
  }
}

class FakeStorage {
  constructor() {
    this.root = process.env.BROKER_TEST_OBJECT_ROOT || path.join(process.cwd(), 'broker-test-objects');
    ensureDir(this.root);
  }
  bucket(name) {
    return new FakeBucket(this.root, name);
  }
  async createBucket(name) {
    const bucket = this.bucket(name);
    await bucket.get();
    return [bucket];
  }
}

class FakeGoogleAuth {
  async getClient() {
    return {};
  }
  async getProjectId() {
    return 'bgit-test';
  }
}

function awsAttrToValue(attr) {
  if (!attr) return undefined;
  if (Object.prototype.hasOwnProperty.call(attr, 'S')) return attr.S;
  if (Object.prototype.hasOwnProperty.call(attr, 'N')) return Number(attr.N);
  return undefined;
}

function awsItemPK(item) {
  if (item.id) return item.id.S;
  if (item.repo_id && item.pr_id) return item.repo_id.S + '#' + item.pr_id.N;
  if (item.fingerprint && item.repo_id) return item.fingerprint.S + '#' + item.repo_id.S;
  return JSON.stringify(item);
}

function makeAWSModules(store, objectRoot) {
  const objectTarget = (bucket) => localObjectTarget(objectRoot, bucket || '');
  class DynamoDBClient {
    async send(command) {
      const input = command.input || {};
      const name = command.constructor.name;
      if (name === 'GetItemCommand') {
        const item = store.awsGet(input.TableName, awsItemPK(input.Key || {}));
        return item ? {Item: item} : {};
      }
      if (name === 'PutItemCommand') {
        const pk = awsItemPK(input.Item || {});
        store.awsPut(input.TableName, pk, {...input.Item, __pk: pk});
        return {};
      }
      if (name === 'DeleteItemCommand') {
        store.awsDelete(input.TableName, awsItemPK(input.Key || {}));
        return {};
      }
      if (name === 'ScanCommand') {
        return {Items: store.awsScan(input.TableName)};
      }
      if (name === 'QueryCommand') {
        const values = input.ExpressionAttributeValues || {};
        const fingerprint = awsAttrToValue(values[':fingerprint']);
        const repoID = awsAttrToValue(values[':repo_id']);
        const prefix = fingerprint ? fingerprint + '#' : repoID ? repoID + '#' : '';
        return {Items: store.awsQuery(input.TableName, prefix)};
      }
      throw new Error('unsupported fake DynamoDB command ' + name);
    }
  }
  class GetItemCommand { constructor(input) { this.input = input || {}; } }
  class PutItemCommand { constructor(input) { this.input = input || {}; } }
  class QueryCommand { constructor(input) { this.input = input || {}; } }
  class ScanCommand { constructor(input) { this.input = input || {}; } }
  class DeleteItemCommand { constructor(input) { this.input = input || {}; } }
  class GetObjectCommand { constructor(input) { this.input = input || {}; } }
  class PutObjectCommand { constructor(input) { this.input = input || {}; } }
  class DeleteObjectCommand { constructor(input) { this.input = input || {}; } }
  class ListObjectsV2Command { constructor(input) { this.input = input || {}; } }
  class HeadBucketCommand { constructor(input) { this.input = input || {}; } }
  class CreateBucketCommand { constructor(input) { this.input = input || {}; } }
  class DeleteBucketCommand { constructor(input) { this.input = input || {}; } }
  class AssumeRoleCommand { constructor(input) { this.input = input || {}; } }
  class GetSecretValueCommand { constructor(input) { this.input = input || {}; } }
  class PutSecretValueCommand { constructor(input) { this.input = input || {}; } }
  class InvokeCommand { constructor(input) { this.input = input || {}; } }
  class StartBuildCommand { constructor(input) { this.input = input || {}; } }
  class BatchGetBuildsCommand { constructor(input) { this.input = input || {}; } }
  class GetLogEventsCommand { constructor(input) { this.input = input || {}; } }
  class S3Client {
    async send(command) {
      const input = command.input || {};
      const name = command.constructor.name;
      const target = objectTarget(input.Bucket || '');
      if (name === 'HeadBucketCommand' || name === 'CreateBucketCommand') {
        target.backend.ensureBucket(target.bucket);
        return {};
      }
      if (name === 'PutObjectCommand') {
        if (input.IfNoneMatch === '*' && target.backend.exists && target.backend.exists(target.bucket, input.Key || '')) {
          const err = new Error('object already exists');
          err.name = 'PreconditionFailed';
          throw err;
        }
        target.backend.put(target.bucket, input.Key || '', Buffer.isBuffer(input.Body) || typeof input.Body === 'string' ? input.Body : Buffer.from(input.Body || ''));
        return {};
      }
      if (name === 'GetObjectCommand') {
        return {Body: Readable.from(target.backend.get(target.bucket, input.Key || ''))};
      }
      if (name === 'DeleteObjectCommand') {
        target.backend.delete(target.bucket, input.Key || '');
        return {};
      }
      if (name === 'DeleteBucketCommand') {
        target.backend.deleteBucket(target.bucket);
        return {};
      }
      if (name === 'ListObjectsV2Command') {
        return {Contents: target.backend.list(target.bucket, input.Prefix || '').map((key) => ({Key: key}))};
      }
      throw new Error('unsupported fake S3 command ' + name);
    }
  }
  class STSClient {
    async send() {
      return {Credentials: {AccessKeyId: 'test', SecretAccessKey: 'test', SessionToken: 'test'}};
    }
  }
  let secretValue = process.env.BGIT_TEST_CI_MATERIALIZER_TOKEN || 'local-ci-materializer-token';
  class SecretsManagerClient {
    async send(command) {
      const name = command.constructor.name;
      if (name === 'GetSecretValueCommand') return {SecretString: secretValue};
      if (name === 'PutSecretValueCommand') {
        secretValue = String((command.input || {}).SecretString || '');
        return {};
      }
      throw new Error('unsupported fake Secrets Manager command ' + name);
    }
  }
  class LambdaClient {
    async send(command) {
      const name = command.constructor.name;
      if (name === 'InvokeCommand') {
        const id = 'local-codebuild-' + Date.now();
        return {Payload: Buffer.from(JSON.stringify({statusCode: 200, body: JSON.stringify({
          status: 'queued',
          provider_build_id: id,
          provider_build_name: id,
          message: 'local AWS CI materializer queued',
          log_group: '/aws/codebuild/bgit-local',
          log_stream: id,
        })}))};
      }
      throw new Error('unsupported fake Lambda command ' + name);
    }
  }
  class CodeBuildClient {
    async send(command) {
      const name = command.constructor.name;
      if (name === 'StartBuildCommand') {
        const id = 'local-codebuild-' + Date.now();
        return {build: {id, arn: id, buildStatus: 'IN_PROGRESS', currentPhase: 'SUBMITTED', logs: {groupName: '/aws/codebuild/bgit-local', streamName: id}}};
      }
      if (name === 'BatchGetBuildsCommand') {
        return {builds: (command.input.ids || []).map((id) => ({id, arn: id, buildStatus: 'SUCCEEDED', currentPhase: 'COMPLETED', startTime: new Date(), endTime: new Date(), logs: {groupName: '/aws/codebuild/bgit-local', streamName: id}}))};
      }
      throw new Error('unsupported fake CodeBuild command ' + name);
    }
  }
  class CloudWatchLogsClient {
    async send(command) {
      const name = command.constructor.name;
      if (name === 'GetLogEventsCommand') return {events: [{message: 'local AWS CI log'}]};
      throw new Error('unsupported fake CloudWatch Logs command ' + name);
    }
  }
  return {
    '@aws-sdk/client-dynamodb': {
      DynamoDBClient,
      GetItemCommand,
      PutItemCommand,
      QueryCommand,
      ScanCommand,
      DeleteItemCommand,
    },
    '@aws-sdk/client-s3': {
      S3Client,
      GetObjectCommand,
      PutObjectCommand,
      DeleteObjectCommand,
      ListObjectsV2Command,
      HeadBucketCommand,
      CreateBucketCommand,
      DeleteBucketCommand,
    },
    '@aws-sdk/client-sts': {
      STSClient,
      AssumeRoleCommand,
    },
    '@aws-sdk/client-secrets-manager': {
      SecretsManagerClient,
      GetSecretValueCommand,
      PutSecretValueCommand,
    },
    '@aws-sdk/client-lambda': {
      LambdaClient,
      InvokeCommand,
    },
    '@aws-sdk/client-codebuild': {
      CodeBuildClient,
      StartBuildCommand,
      BatchGetBuildsCommand,
    },
    '@aws-sdk/client-cloudwatch-logs': {
      CloudWatchLogsClient,
      GetLogEventsCommand,
    },
  };
}

function localObjectTarget(objectRoot, bucket) {
  if (String(bucket || '').includes('://')) {
    const target = parseStorageURI(bucket);
    return {backend: localObjectBackend(target), bucket: target.bucket || ''};
  }
  return {backend: fileObjectBackend(objectRoot || process.env.BROKER_TEST_OBJECT_ROOT || path.join(process.cwd(), 'broker-test-objects')), bucket};
}

function localObjectBackend(target) {
  if (target.scheme === 's3') return s3CLIObjectBackend(target);
  if (target.scheme === 'gs') return gcsCLIObjectBackend(target);
  return fileObjectBackend(target.root);
}

function parseStorageURI(value) {
  const raw = String(value || '').trim();
  if (!raw || !raw.includes('://')) {
    return {scheme: 'file', root: raw || path.join(process.cwd(), 'broker-test-objects')};
  }
  const scheme = raw.slice(0, raw.indexOf('://')).toLowerCase();
  const rest = raw.slice(raw.indexOf('://') + 3);
  if (scheme === 'file') {
    const filePath = rest.startsWith('localhost/') ? rest.slice('localhost'.length) : rest;
    return {scheme: 'file', root: decodeURIComponent(filePath)};
  }
  if (scheme !== 's3' && scheme !== 'gs') {
    throw new Error(`unsupported local broker storage URI scheme ${scheme}`);
  }
  const slash = rest.indexOf('/');
  const host = slash >= 0 ? rest.slice(0, slash) : rest;
  const prefix = slash >= 0 ? rest.slice(slash + 1).replace(/^\/+|\/+$/g, '') : '';
  const parsed = parseCloudStorageHost(scheme, host);
  parsed.prefix = prefix;
  return parsed;
}

function parseCloudStorageHost(scheme, host) {
  const cleanHost = String(host || '');
  const labels = cleanHost.split('.').filter(Boolean);
  if (labels.length === 0) throw new Error(`missing bucket in ${scheme} storage URI`);
  const isRegion = scheme === 's3' ? isAWSRegion : isGCPRegion;
  let profile = '';
  let region = '';
  let bucketLabels = labels;
  if (labels.length >= 3 && isRegion(labels[1])) {
    profile = labels[0];
    region = labels[1];
    bucketLabels = labels.slice(2);
  } else if (labels.length >= 2) {
    profile = labels[0];
    bucketLabels = labels.slice(1);
  }
  const bucket = bucketLabels.join('.');
  if (!bucket) throw new Error(`missing bucket in ${scheme} storage URI`);
  return {scheme, profile, region, bucket};
}

function isAWSRegion(value) {
  return /^[a-z]{2}(-gov)?-[a-z]+-[0-9]+$/.test(String(value || '')) || /^cn-[a-z]+-[0-9]+$/.test(String(value || ''));
}

function isGCPRegion(value) {
  return /^[a-z]+-[a-z0-9]+[0-9]$/.test(String(value || ''));
}

function fileObjectBackend(objectRoot) {
  return {
    ensureBucket(bucket) {
      ensureDir(safeJoin(objectRoot, bucket));
    },
    put(bucket, key, data) {
      const filePath = safeJoin(objectRoot, bucket, key);
      ensureDir(path.dirname(filePath));
      fs.writeFileSync(filePath, data);
    },
    exists(bucket, key) {
      return fs.existsSync(safeJoin(objectRoot, bucket, key));
    },
    get(bucket, key) {
      return fs.readFileSync(safeJoin(objectRoot, bucket, key));
    },
    delete(bucket, key) {
      fs.rmSync(safeJoin(objectRoot, bucket, key), {force: true});
    },
    deleteBucket(bucket) {
      fs.rmSync(safeJoin(objectRoot, bucket), {recursive: true, force: true});
    },
    list(bucket, prefix) {
      const bucketRoot = safeJoin(objectRoot, bucket);
      const contents = [];
      if (fs.existsSync(bucketRoot)) {
        const walk = (dir) => {
          for (const entry of fs.readdirSync(dir, {withFileTypes: true})) {
            const full = path.join(dir, entry.name);
            if (entry.isDirectory()) walk(full);
            else {
              const key = path.relative(bucketRoot, full).split(path.sep).join('/');
              if (key.startsWith(prefix || '')) contents.push(key);
            }
          }
        };
        walk(bucketRoot);
      }
      return contents;
    },
  };
}

function tmpFile() {
  ensureDir(path.join(os.tmpdir(), 'bgit-local-broker'));
  return path.join(os.tmpdir(), 'bgit-local-broker', `${process.pid}-${Date.now()}-${Math.random().toString(16).slice(2)}`);
}

function s3CLIObjectBackend(target) {
  const realBucket = target.bucket;
  const basePrefix = String(target.prefix || '').replace(/^\/+|\/+$/g, '');
  const awsArgs = [];
  if (target.profile) awsArgs.push('--profile', target.profile);
  if (target.region) awsArgs.push('--region', target.region);
  const mapKey = (_bucket, key = '') => [basePrefix, key].filter(Boolean).join('/');
  const run = (...args) => {
    const env = {...process.env};
    if (target.region) env.AWS_REGION = target.region;
    else if (env.AWS_REGION === 'local') delete env.AWS_REGION;
    return execFileSync('aws', [...awsArgs, ...args], {stdio: ['ignore', 'pipe', 'pipe'], env});
  };
  return {
    ensureBucket() {
      try {
        run('s3api', 'head-bucket', '--bucket', realBucket);
      } catch (_) {
        const args = ['s3api', 'create-bucket', '--bucket', realBucket];
        if (target.region && target.region !== 'us-east-1') {
          args.push('--create-bucket-configuration', `LocationConstraint=${target.region}`);
        }
        run(...args);
      }
    },
    put(bucket, key, data) {
      const file = tmpFile();
      fs.writeFileSync(file, data);
      try {
        run('s3api', 'put-object', '--bucket', realBucket, '--key', mapKey(bucket, key), '--body', file);
      } finally {
        fs.rmSync(file, {force: true});
      }
    },
    exists(bucket, key) {
      return this.list(bucket, key).includes(key);
    },
    get(bucket, key) {
      const file = tmpFile();
      try {
        run('s3api', 'get-object', '--bucket', realBucket, '--key', mapKey(bucket, key), file);
        return fs.readFileSync(file);
      } finally {
        fs.rmSync(file, {force: true});
      }
    },
    delete(bucket, key) {
      run('s3api', 'delete-object', '--bucket', realBucket, '--key', mapKey(bucket, key));
    },
    deleteBucket(bucket) {
      for (const key of this.list(bucket, '')) this.delete(bucket, key);
    },
    list(bucket, prefix) {
      const mappedPrefix = mapKey(bucket, prefix || '');
      const out = run('s3api', 'list-objects-v2', '--bucket', realBucket, '--prefix', mappedPrefix, '--output', 'json');
      const parsed = JSON.parse(out.toString('utf8') || '{}');
      const strip = mapKey(bucket, '');
      const stripPrefix = strip ? strip + '/' : '';
      return (parsed.Contents || []).map((item) => String(item.Key || '')).filter((key) => key.startsWith(stripPrefix)).map((key) => key.slice(stripPrefix.length));
    },
  };
}

function gcsCLIObjectBackend(target) {
  const realBucket = target.bucket;
  const basePrefix = String(target.prefix || '').replace(/^\/+|\/+$/g, '');
  const mapKey = (_bucket, key = '') => [basePrefix, key].filter(Boolean).join('/');
  const uri = (bucket, key = '') => `gs://${realBucket}/${mapKey(bucket, key)}`;
  const gcloudArgs = [];
  if (target.profile) gcloudArgs.push('--configuration', target.profile);
  const run = (...args) => execFileSync('gcloud', [...gcloudArgs, ...args], {stdio: ['ignore', 'pipe', 'pipe']});
  return {
    ensureBucket() {
      try {
        run('storage', 'ls', `gs://${realBucket}`);
      } catch (_) {
        const args = ['storage', 'buckets', 'create', `gs://${realBucket}`];
        if (target.region) args.push('--location', target.region);
        run(...args);
      }
    },
    put(bucket, key, data) {
      const file = tmpFile();
      fs.writeFileSync(file, data);
      try {
        run('storage', 'cp', file, uri(bucket, key));
      } finally {
        fs.rmSync(file, {force: true});
      }
    },
    exists(bucket, key) {
      return this.list(bucket, key).includes(key);
    },
    get(bucket, key) {
      const file = tmpFile();
      try {
        run('storage', 'cp', uri(bucket, key), file);
        return fs.readFileSync(file);
      } finally {
        fs.rmSync(file, {force: true});
      }
    },
    delete(bucket, key) {
      try {
        run('storage', 'rm', uri(bucket, key));
      } catch (_) {}
    },
    deleteBucket(bucket) {
      try {
        run('storage', 'rm', '--recursive', uri(bucket, '**'));
      } catch (_) {}
    },
    list(bucket, prefix) {
      const objectPrefix = mapKey(bucket, prefix || '');
      let out = Buffer.alloc(0);
      try {
        out = run('storage', 'ls', '--recursive', `gs://${realBucket}/${objectPrefix}**`);
      } catch (_) {
        return [];
      }
      const stripPrefix = `gs://${realBucket}/${mapKey(bucket, '')}/`;
      return out.toString('utf8').split(/\r?\n/).map((line) => line.trim()).filter((line) => line.startsWith(stripPrefix) && !line.endsWith('/')).map((line) => line.slice(stripPrefix.length));
    },
  };
}

async function handleObjectRequest(req, res, objectRoot) {
  const parts = req.url.split('?')[0].split('/').filter(Boolean);
  if (parts[0] !== '_objects' || parts.length < 3) return false;
  const bucket = decodeURIComponent(parts[1]);
  const target = localObjectTarget(objectRoot, bucket);
  const name = decodeObjectName(parts.slice(2).join('/'));
  if (req.method === 'GET') {
    let data;
    try {
      data = target.backend.get(target.bucket, name);
    } catch (_) {
      res.writeHead(404).end('not found');
      return true;
    }
    res.writeHead(200, {'content-type': 'application/octet-stream'});
    res.end(data);
    return true;
  }
  if (req.method === 'PUT') {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(Buffer.from(chunk)));
    req.on('end', () => {
      target.backend.put(target.bucket, name, Buffer.concat(chunks));
      res.writeHead(200, {'content-type': 'application/json'}).end('{}');
    });
    return true;
  }
  if (req.method === 'DELETE') {
    target.backend.delete(target.bucket, name);
    res.writeHead(200, {'content-type': 'application/json'}).end('{}');
    return true;
  }
  res.writeHead(405).end('method not allowed');
  return true;
}

function xmlEscape(value) {
  return String(value || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&apos;');
}

async function handleS3Request(req, res, objectRoot) {
  const parsed = new URL(req.url, 'http://127.0.0.1');
  const parts = parsed.pathname.split('/').filter(Boolean);
  if (parts.length === 0 || parts[0] === '_objects') return false;
  const bucket = decodeURIComponent(parts[0]);
  const target = localObjectTarget(objectRoot, bucket);
  const key = parts.slice(1).map(decodeURIComponent).join('/');
  if (req.method === 'HEAD' && !key) {
    try {
      target.backend.ensureBucket(target.bucket);
      res.writeHead(200).end();
    } catch (_) {
      res.writeHead(404).end();
    }
    return true;
  }
  if (req.method === 'PUT' && !key) {
    target.backend.ensureBucket(target.bucket);
    res.writeHead(200, {'content-type': 'application/xml'}).end('');
    return true;
  }
  if (req.method === 'DELETE' && !key) {
    target.backend.deleteBucket(target.bucket);
    res.writeHead(204).end();
    return true;
  }
  if (req.method === 'GET' && parsed.searchParams.get('list-type') === '2') {
    const prefix = parsed.searchParams.get('prefix') || '';
    const contents = target.backend.list(target.bucket, prefix);
    const body = [
      '<?xml version="1.0" encoding="UTF-8"?>',
      '<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">',
      `<Name>${xmlEscape(bucket)}</Name>`,
      `<Prefix>${xmlEscape(prefix)}</Prefix>`,
      '<IsTruncated>false</IsTruncated>',
      ...contents.map((item) => `<Contents><Key>${xmlEscape(item)}</Key><Size>0</Size></Contents>`),
      '</ListBucketResult>',
    ].join('');
    res.writeHead(200, {'content-type': 'application/xml'}).end(body);
    return true;
  }
  if (!key) return false;
  if (req.method === 'GET') {
    let data;
    try {
      data = target.backend.get(target.bucket, key);
    } catch (_) {
      res.writeHead(404, {'content-type': 'application/xml'}).end('<Error><Code>NoSuchKey</Code></Error>');
      return true;
    }
    res.writeHead(200, {'content-type': 'application/octet-stream'});
    res.end(data);
    return true;
  }
  if (req.method === 'PUT') {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(Buffer.from(chunk)));
    req.on('end', () => {
      target.backend.put(target.bucket, key, Buffer.concat(chunks));
      res.writeHead(200, {'content-type': 'application/xml'}).end('');
    });
    return true;
  }
  if (req.method === 'DELETE') {
    target.backend.delete(target.bucket, key);
    res.writeHead(204).end();
    return true;
  }
  return false;
}

module.exports = {
  SQLiteStore,
  StorageBackedStore,
  FakeFirestore,
  FakeStorage,
  FakeGoogleAuth,
  makeAWSModules,
  handleObjectRequest,
  handleS3Request,
};

'use strict';

const fs = require('fs');
const path = require('path');
const {Readable} = require('stream');
const {DatabaseSync} = require('node:sqlite');

function ensureDir(dir) {
  fs.mkdirSync(dir, {recursive: true});
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
    return path.join(this.root, this.bucket, this.name);
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
    ensureDir(path.join(this.root, this.name));
    return [this];
  }
  async getFiles(opts = {}) {
    const bucketRoot = path.join(this.root, this.name);
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
    fs.rmSync(path.join(this.root, this.name), {recursive: true, force: true});
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
  class S3Client {
    async send(command) {
      const input = command.input || {};
      const name = command.constructor.name;
      const bucketRoot = path.join(objectRoot, input.Bucket || '');
      const filePath = path.join(bucketRoot, input.Key || '');
      if (name === 'HeadBucketCommand' || name === 'CreateBucketCommand') {
        ensureDir(bucketRoot);
        return {};
      }
      if (name === 'PutObjectCommand') {
        ensureDir(path.dirname(filePath));
        fs.writeFileSync(filePath, Buffer.isBuffer(input.Body) || typeof input.Body === 'string' ? input.Body : Buffer.from(input.Body || ''));
        return {};
      }
      if (name === 'GetObjectCommand') {
        return {Body: Readable.from(fs.readFileSync(filePath))};
      }
      if (name === 'DeleteObjectCommand') {
        fs.rmSync(filePath, {force: true});
        return {};
      }
      if (name === 'DeleteBucketCommand') {
        fs.rmSync(bucketRoot, {recursive: true, force: true});
        return {};
      }
      if (name === 'ListObjectsV2Command') {
        const contents = [];
        if (fs.existsSync(bucketRoot)) {
          const walk = (dir) => {
            for (const entry of fs.readdirSync(dir, {withFileTypes: true})) {
              const full = path.join(dir, entry.name);
              if (entry.isDirectory()) walk(full);
              else {
                const key = path.relative(bucketRoot, full).split(path.sep).join('/');
                if (key.startsWith(input.Prefix || '')) contents.push({Key: key});
              }
            }
          };
          walk(bucketRoot);
        }
        return {Contents: contents};
      }
      throw new Error('unsupported fake S3 command ' + name);
    }
  }
  class STSClient {
    async send() {
      return {Credentials: {AccessKeyId: 'test', SecretAccessKey: 'test', SessionToken: 'test'}};
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
  };
}

async function handleObjectRequest(req, res, objectRoot) {
  const parts = req.url.split('?')[0].split('/').filter(Boolean);
  if (parts[0] !== '_objects' || parts.length < 3) return false;
  const bucket = decodeURIComponent(parts[1]);
  const name = decodeObjectName(parts.slice(2).join('/'));
  const filePath = path.join(objectRoot, bucket, name);
  if (req.method === 'GET') {
    if (!fs.existsSync(filePath)) {
      res.writeHead(404).end('not found');
      return true;
    }
    res.writeHead(200, {'content-type': 'application/octet-stream'});
    fs.createReadStream(filePath).pipe(res);
    return true;
  }
  if (req.method === 'PUT') {
    ensureDir(path.dirname(filePath));
    const chunks = [];
    req.on('data', (chunk) => chunks.push(Buffer.from(chunk)));
    req.on('end', () => {
      fs.writeFileSync(filePath, Buffer.concat(chunks));
      res.writeHead(200, {'content-type': 'application/json'}).end('{}');
    });
    return true;
  }
  if (req.method === 'DELETE') {
    fs.rmSync(filePath, {force: true});
    res.writeHead(200, {'content-type': 'application/json'}).end('{}');
    return true;
  }
  res.writeHead(405).end('method not allowed');
  return true;
}

module.exports = {
  SQLiteStore,
  FakeFirestore,
  FakeStorage,
  FakeGoogleAuth,
  makeAWSModules,
  handleObjectRequest,
};

#!/usr/bin/env node
'use strict';

const fs = require('fs');
const http = require('http');
const Module = require('module');
const path = require('path');
const vm = require('vm');
const {
  SQLiteStore,
  FakeFirestore,
  FakeStorage,
  FakeGoogleAuth,
  makeAWSModules,
  handleObjectRequest,
} = require('./test_support/sqlite_broker');

const runtime = process.argv[2] || process.env.BROKER_TEST_RUNTIME || 'gcp';
const port = Number(process.env.PORT || process.env.BROKER_TEST_PORT || 19080);
const root = process.env.BROKER_TEST_ROOT || path.join(process.cwd(), '.broker-test');
const sqlitePath = process.env.BROKER_TEST_SQLITE || path.join(root, runtime + '.sqlite');
const objectRoot = process.env.BROKER_TEST_OBJECT_ROOT || path.join(root, runtime + '-objects');

fs.mkdirSync(root, {recursive: true});
fs.mkdirSync(objectRoot, {recursive: true});
process.env.BROKER_TEST_MODE = 'sqlite';
process.env.BROKER_TEST_SQLITE = sqlitePath;
process.env.BROKER_TEST_OBJECT_ROOT = objectRoot;
process.env.BROKER_VERSION = process.env.BROKER_VERSION || '1.0.0-test';
process.env.TABLE_NAME = process.env.TABLE_NAME || 'bgit-broker-repos';
process.env.PR_TABLE_NAME = process.env.PR_TABLE_NAME || 'bgit-broker-prs';
process.env.MEMBER_TABLE_NAME = process.env.MEMBER_TABLE_NAME || 'bgit-broker-members';
process.env.TRANSFER_ROLE_ARN = process.env.TRANSFER_ROLE_ARN || 'arn:aws:iam::000000000000:role/bgit-test-transfer';
process.env.AWS_REGION = process.env.AWS_REGION || 'us-east-1';

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(Buffer.from(chunk)));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function installGCPMocks() {
  const originalLoad = Module._load;
  Module._load = function patchedLoad(request, parent, isMain) {
    if (request === '@google-cloud/firestore') return {Firestore: FakeFirestore};
    if (request === '@google-cloud/storage') return {Storage: FakeStorage};
    if (request === 'google-auth-library') return {GoogleAuth: FakeGoogleAuth};
    return originalLoad.call(this, request, parent, isMain);
  };
}

function loadGCPHandler() {
  installGCPMocks();
  return require('./gcp/index.js').broker;
}

function awsZipFileSource() {
  const template = fs.readFileSync(path.join(__dirname, 'aws/template.yaml'), 'utf8').split(/\r?\n/);
  const start = template.findIndex((line) => line.includes('ZipFile: |'));
  if (start < 0) throw new Error('AWS template ZipFile not found');
  const lines = [];
  for (let i = start + 1; i < template.length; i++) {
    const line = template[i];
    if (/^  BrokerFunctionUrl:/.test(line)) break;
    lines.push(line.replace(/^          /, ''));
  }
  return lines.join('\n');
}

function loadAWSHandler() {
  const store = new SQLiteStore(sqlitePath);
  const awsModules = makeAWSModules(store, objectRoot);
  const sandbox = {
    exports: {},
    module: {exports: {}},
    require: (name) => {
      if (awsModules[name]) return awsModules[name];
      return require(name);
    },
    process,
    Buffer,
    console,
    setTimeout,
    clearTimeout,
    URL,
  };
  sandbox.module.exports = sandbox.exports;
  vm.runInNewContext(awsZipFileSource(), sandbox, {filename: 'broker/aws/template.js'});
  return sandbox.exports.handler || sandbox.module.exports.handler;
}

let server;

function gcpResponse(res) {
  return {
    set: (key, value) => res.setHeader(key, value),
    status(code) {
      res.statusCode = code;
      return this;
    },
    send(value) {
      res.end(value);
    },
  };
}

async function handleGCP(handler, req, res, raw) {
  const bodyText = raw.toString('utf8');
  const gcpReq = {
    path: new URL(req.url, process.env.BROKER_TEST_BASE_URL).pathname,
    method: req.method,
    headers: req.headers,
    rawBody: raw,
    body: bodyText ? JSON.parse(bodyText) : {},
    get: (name) => req.headers[String(name).toLowerCase()],
  };
  await handler(gcpReq, gcpResponse(res));
}

async function handleAWS(handler, req, res, raw) {
  const event = {
    rawPath: new URL(req.url, process.env.BROKER_TEST_BASE_URL).pathname,
    requestContext: {http: {method: req.method}},
    headers: req.headers,
    body: raw.toString('utf8'),
  };
  const out = await handler(event);
  res.writeHead(out.statusCode || 200, out.headers || {'content-type': 'application/json'});
  res.end(out.body || '');
}

async function main() {
  const handler = runtime === 'aws' ? loadAWSHandler() : loadGCPHandler();
  server = http.createServer(async (req, res) => {
    try {
      if (await handleObjectRequest(req, res, objectRoot)) return;
      const raw = await readBody(req);
      if (runtime === 'aws') await handleAWS(handler, req, res, raw);
      else await handleGCP(handler, req, res, raw);
    } catch (err) {
      res.writeHead(err.statusCode || err.status || 500, {'content-type': 'application/json'});
      res.end(JSON.stringify({error: err.message || String(err)}));
    }
  });
  await new Promise((resolve) => server.listen(port, '127.0.0.1', resolve));
  const address = server.address();
  process.env.BROKER_TEST_BASE_URL = `http://127.0.0.1:${address.port}`;
  console.log(`bgit test broker ${runtime} listening on ${process.env.BROKER_TEST_BASE_URL}`);
}

process.on('SIGTERM', () => server && server.close(() => process.exit(0)));
process.on('SIGINT', () => server && server.close(() => process.exit(130)));

main().catch((err) => {
  console.error(err && err.stack || err);
  process.exit(1);
});


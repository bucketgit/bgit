'use strict';

const crypto = require('crypto');
const fs = require('fs');
const path = require('path');

const fixture = JSON.parse(fs.readFileSync(path.join(__dirname, '..', '..', 'spec', 'testdata', 'signing-v2.json'), 'utf8'));

function canonicalGCP(vector) {
  return [
    'bgit-broker-v2',
    String(vector.method || '').trim().toUpperCase(),
    String(vector.path || '/').trim(),
    String(vector.host || '').trim().toLowerCase(),
    String(vector.timestamp || '').trim(),
    String(vector.nonce || '').trim(),
    crypto.createHash('sha256').update(Buffer.from(vector.body)).digest('hex'),
  ].join('\n');
}

function canonicalAWS(vector) {
  const digest = crypto.createHash('sha256').update(Buffer.from(vector.body)).digest('hex');
  return ['bgit-broker-v2', vector.method.toUpperCase(), vector.path, vector.host.toLowerCase(), vector.timestamp, vector.nonce, digest].join('\n');
}

for (const vector of fixture.vectors) {
  for (const [runtime, canonical] of [['gcp', canonicalGCP], ['aws', canonicalAWS]]) {
    const actual = canonical(vector);
    if (actual !== vector.message) {
      throw new Error(`${runtime} signing fixture ${vector.name} mismatch\nactual: ${actual}\nexpected: ${vector.message}`);
    }
  }
}

process.stdout.write(`validated ${fixture.vectors.length} signing fixture(s) for AWS and GCP\n`);

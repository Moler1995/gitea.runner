import assert from 'node:assert/strict';
import { appendFileSync, existsSync } from 'node:fs';
import path from 'node:path';

assert.ok(existsSync(path.join(process.env.GITHUB_WORKSPACE, '.git')));
appendFileSync(process.env.GITHUB_OUTPUT, 'value=from-local-action\n');
appendFileSync(process.env.GITHUB_STATE, 'checked=yes\n');
console.log('LOCAL_ACTION_OK');

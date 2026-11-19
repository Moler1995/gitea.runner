import { open } from 'node:fs/promises';
import path from 'node:path';

function inside(name) {
  const clean = path.posix.normalize(name);
  if (!name || name.includes('\0') || clean.startsWith('/') || clean === '..' || clean.startsWith('../')) throw new Error('Archive entry escapes its destination');
  return clean;
}

function text(buffer) { return buffer.toString('utf8').split('\0', 1)[0]; }
function size(buffer) {
  if (buffer[0] & 0x80) {
    let value = BigInt(buffer[0] & 0x7f);
    for (const byte of buffer.subarray(1)) value = value * 256n + BigInt(byte);
    if (value > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error('Archive entry is too large');
    return Number(value);
  }
  const value = text(buffer).trim();
  if (value && !/^[0-7]+$/.test(value)) throw new Error('Invalid archive entry size');
  return value ? parseInt(value, 8) : 0;
}

function pax(buffer) {
  const fields = {};
  for (let offset = 0; offset < buffer.length;) {
    const space = buffer.indexOf(32, offset);
    const length = Number(buffer.subarray(offset, space).toString());
    if (space < offset || !Number.isSafeInteger(length) || length <= space - offset + 1 || offset + length > buffer.length) throw new Error('Invalid PAX archive header');
    const entry = buffer.subarray(space + 1, offset + length - 1).toString();
    const equals = entry.indexOf('=');
    if (equals < 1) throw new Error('Invalid PAX archive field');
    fields[entry.slice(0, equals)] = entry.slice(equals + 1);
    offset += length;
  }
  return fields;
}

export async function validateArchive(filename) {
  const file = await open(filename, 'r');
  try {
    const total = (await file.stat()).size;
    let local = {}, global = {};
    for (let offset = 0; offset + 512 <= total;) {
      const header = Buffer.alloc(512);
      if ((await file.read(header, 0, 512, offset)).bytesRead !== 512) throw new Error('Truncated archive');
      if (header.every(byte => byte === 0)) return;
      const type = String.fromCharCode(header[156] || 48);
      const count = size(header.subarray(124, 136));
      offset += 512;
      if (offset + count > total) throw new Error('Truncated archive entry');
      if ('xgLK'.includes(type)) {
        if (count > 1 << 20) throw new Error('Archive metadata exceeds 1 MiB');
        const data = Buffer.alloc(count);
        if ((await file.read(data, 0, count, offset)).bytesRead !== count) throw new Error('Truncated archive metadata');
        if (type === 'g') global = { ...global, ...pax(data) };
        else if (type === 'x') local = { ...local, ...pax(data) };
        else local[type === 'L' ? 'path' : 'linkpath'] = text(data).replace(/\n$/, '');
      } else {
        const attributes = { ...global, ...local };
        const name = inside(attributes.path || [text(header.subarray(345, 500)), text(header.subarray(0, 100))].filter(Boolean).join('/'));
        const link = attributes.linkpath || text(header.subarray(157, 257));
        if (!'0125'.includes(type)) throw new Error('Unsupported archive entry type');
        if (type === '1') inside(link);
        if (type === '2') {
          if (path.posix.isAbsolute(link)) throw new Error('Absolute archive symlink');
          inside(path.posix.join(path.posix.dirname(name), link));
        }
        local = {};
      }
      offset += Math.ceil(count / 512) * 512;
    }
    throw new Error('Archive has no complete end marker');
  } finally { await file.close(); }
}

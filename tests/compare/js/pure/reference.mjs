// Writes the reference outputs of the desktop client's pure functions:
// testdata/compat/<version>/pure-functions.json on stdout.
//
//   node reference.mjs <app/dist> [symbols.json]
//
// With a second argument, the minified names the anchors found are written
// there. Go tests read the outputs with internal/pkg/compattest and need
// neither Node nor the client.
import fs from 'node:fs';
import util from 'node:util';
import {open} from './desktop.mjs';

if (process.argv[2] === undefined) throw new Error('usage: node reference.mjs <app/dist> [symbols.json]');
const desktop = open(process.argv[2]);

const cases = [
  {text: '', args: {}}, {text: 'one\ntwo\nthree\n', args: {offset: 2, limit: 1}},
  {text: 'one\ntwo\n', args: {offset: 20}}, {text: 'one\ntwo\nthree', args: {offset: -2}},
];
const streamCases = [...cases,
  {text: 'one\ntwo\nthree\n', args: {maxChars: 5}},
  {text: 'one\ntwo\nthree\n', args: {maxChars: 7}},
  {text: 'one\ntwo\nthree', args: {maxChars: 100}},
  {text: 'one\ntwo\nthree\nfour\n', args: {offset: 2, maxChars: 4}},
  {text: 'one\ntwo\nthree\nfour\n', args: {offset: 2, limit: 1, maxChars: 1}},
  {text: 'one\ntwo\nthree\nfour', args: {offset: -2, maxChars: 1}},
  {text: 'one\ntwo\n', args: {offset: 20, maxChars: 1}},
  {text: 'ab\r\ncd\r\nef\r\n', args: {maxChars: 5}},
  {text: 'ab\r\ncd\r\nef\r\n', args: {maxChars: 4}},
  {text: '\u{1F600}\u{1F600}\nx', args: {maxChars: 1}},
  {text: 'a'.repeat(70000) + '\nb', args: {maxChars: 10}},
  {text: 'a' + 'é'.repeat(40000), args: {maxChars: 10}},
  {text: '\uFEFF' + 'a'.repeat(70000), args: {maxChars: 10}},
  {text: 'x\n' + 'a'.repeat(140000) + '\ny', args: {offset: 2, maxChars: 70000}},
];

// How the reader decodes a file that is not valid UTF-8: lossy UTF-8 if the
// desktop finds it mostly UTF-8, Latin-1 otherwise.
const fallback = [
  '636166e920617520616974', 'd0bfd180d0b8d0b2d0b5d18220ff20d0bcd0b8d180', 'e28220616263',
  'e697a5e69cace8aa9efffee697a5', '61fffe62',
].map(hex => {
  const lossy = new util.TextDecoder('utf-8').decode(Buffer.from(hex, 'hex'));
  return {hex, mostlyUtf8: desktop.mostlyUtf8(lossy), lossy};
});

// Long texts are stored as runs of one character to keep the file small.
const runs = text => [...text.matchAll(/(.)\1*/gsu)].map(m => ({char: m[1], count: [...m[0]].length}));
const streaming = [];
for (const c of streamCases) {
  const {content, ...result} = await desktop.stream(c.text, c.args);
  streaming.push(c.text.length > 200
    ? {textRuns: runs(c.text), args: c.args, result: {contentRuns: runs(content), ...result}}
    : {text: c.text, args: c.args, result: {content, ...result}});
}

// How the start of a file is classified: UTF-16 and UTF-32 by byte order
// mark or zero bytes, binary by NUL or control bytes, images by signature.
const utf16 = (s, bigEndian) => {
  const b = Buffer.from(s, 'utf16le');
  if (bigEndian) b.swap16();
  return b;
};
const utf32 = (s, bigEndian) => Buffer.concat([...s].map(c => {
  const b = Buffer.alloc(4);
  if (bigEndian) b.writeUInt32BE(c.codePointAt(0));
  else b.writeUInt32LE(c.codePointAt(0));
  return b;
}));
const formatInputs = [
  Buffer.alloc(0), Buffer.from('hello\n'), Buffer.from('a'.repeat(9000) + '\0'), Buffer.from('\x1b[31mred\x1b[0m\n'.repeat(20)),
  Buffer.from('plain \x1b[1mbold\x1b[0m text that is mostly letters and spaces '.repeat(5)), Buffer.from('café \x1b[0m'),
  utf16('hello world\n'), utf16('hello world\n', true), Buffer.concat([Buffer.from([0xff, 0xfe]), utf16('hi there')]),
  Buffer.concat([Buffer.from([0xfe, 0xff]), utf16('hi there', true)]),
  Buffer.concat([Buffer.from([0xff, 0xfe, 0, 0]), utf32('hi there')]), Buffer.concat([Buffer.from([0, 0, 0xfe, 0xff]), utf32('hi', true)]),
  utf32('hello'), utf32('hello', true),
  Buffer.from('ab\0cd'), Buffer.from('ff\n', 'hex'), Buffer.from('89504e470d0a1a0a0000000d49484452', 'hex'),
  Buffer.from('ffd8ffe000104a464946', 'hex'),
  Buffer.from('474946383961010001000000', 'hex'), Buffer.from('52494646240000005745425056503820', 'hex'),
  Buffer.from('\x89PNG\r\n\x1a\nno zero bytes', 'latin1'),
  Buffer.from('\x01'.repeat(5) + 'a'.repeat(95)), Buffer.from('\x01'.repeat(4) + 'a'.repeat(96)), Buffer.from('ff'.repeat(40), 'hex'),
  Buffer.concat([Buffer.from('ж'.repeat(4095) + 'a'), Buffer.from([0xd0])]),
  Buffer.concat([Buffer.from('ж'.repeat(4095) + 'a'), Buffer.from([0xd0, 0xb6]), Buffer.from('\x01'.repeat(30))]),
  Buffer.from('d0bfd180d0b8d0b2d0b5d18220d0bcd0b8d180ff', 'hex'), Buffer.from('61ff6262', 'hex'), Buffer.from('a\0b\0c'),
  Buffer.from('\0a\0b\0c\0d'),
];
const format = formatInputs.flatMap(bytes => {
  const result = desktop.format(bytes);
  return result === null ? [] : [{hex: bytes.toString('hex'), ...result}];
});

// The size prepareImageForModel scales an image to.
const sizes = [[1, 1], [1024, 1024], [1025, 10], [4000, 3000], [3840, 2160], [2160, 3840], [10000, 9], [10000, 7], [7, 10000], [2000, 8],
  [1281, 800], [0, 5], [1500, 1501]];
const imageFit = sizes.flatMap(([width, height]) => [1024, 1280].map(side => ({width, height, side, result: desktop.imageFit(width, height, side)})));

// Titles of discovered sessions and setup: what the desktop replaces in
// them, where its patterns stop, and where it cuts.
const key = 'AbCdEfGhIjKlMnOpQrStUvWxYz012345';
const titleTexts = [
  'fix the bug in /home/me/project/main.go and mail me@example.com', 'see https://example.com/a?b=c, then src/main/java and ./a/b',
  `open C:\\Users\\me\\file.txt or ~/notes/todo.md`, '  several\n lines\tand\u00a0spaces  ',
  'use sk-abcdefghijklmnop1234 and ghp_abcdefghijklmnop1234', 'xoxb-1234567890-abc AKIA0123456789ABCDEF AIzaSyA-abcdefghijklmnopqrstuvwxyz0123',
  'Bearer abcdefghijklmnop.qrst', 'xBearer abcdefghijklmnop.qrst', 'BEARER   abcdefghijklmnop',
  'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnop',
  `token=${key}`, `token: ${key}==`, `token:  ${key}`, `token="${key}"`, `a=${key}==${key}`, `a=${key}=${key}`, `a=${key}===b`,
  `${key} ${key}${key}`, '0123456789abcdef0123456789abcdef', 'x0123456789abcdef0123456789abcdefg', 'key=0123456789abcdef0123456789abcdef!',
  'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN', 'wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY', 'sk_live_a1a1a1a1a1a1a1a1a1a1a1a1 glpat-c3c3c3c3c3c3c3c3c3c3',
  'ſ://host/path K://x/y HTTP://Example.COM', 'a@b.c a@b.co @b.co a@.co', '.../a/b x/a/b -/a/b (/a/b) /a /a/b/ //a//b',
  'x'.repeat(160), 'x'.repeat(161), 'x'.repeat(158) + '  tail', `${'x'.repeat(150)} /home/me/project/file`, `${'x'.repeat(140)} ${key}${key}`,
];
const discoveryTitles = titleTexts.map(text => ({text, title: desktop.discoveryTitle(text), whole: desktop.discoveryTitle(text, null)}));
const commandTitles = [['ns', '/home/me/x', 'cmd'], ['review'], ['a', `k=${key}`, 'x'.repeat(170)], ['  a  b ', 'me@example.com']]
  .map(parts => ({parts, title: desktop.commandTitle(parts)}));

// An administrator's denylist: the simple commands the desktop finds in a
// command, and whether rules block it. The parser is a native module of the
// client, so this part needs the Node of the comparison image.
const denylistCases = [
  ['rm -f x; echo one', ['rm']], ['echo ok', ['rm']], ['r\\m -f x', ['rm']], ["'rm' -f x", ['rm']], ['/bin/rm -rf x', ['rm -rf']],
  ['sudo rm -rf /', ['rm']], ['sudo rm -rf /', ['*rm*']], ['git push --force origin main', ['git:*--force*']], ['git push origin main', ['git:*--force*']],
  ['aws s3 rm s3://b --recursive', ['aws:*s3 rm*']], ['echo hi', ['*']], ['echo hi', [':x']], ['echo hi', ['  ', '']], ['echo hi', []],
  ['', ['rm']], ['A=1 B=2', ['rm']], ['ls | xargs rm', ['rm']], ['ls | xargs rm', ['xargs:rm*']], ['echo "$(rm x)"', ['rm']],
  ['cat <<EOF\nbody $(rm x)\nEOF', ['rm']], ['if [ -f x ]; then rm x; fi', ['rm']], ['for f in *; do echo $f; done', ['rm', 'echo']],
  ['time rm x', ['rm']], ['time rm x', ['time']], ['FOO=1 rm x > out y', ['rm x y']], ['FOO=1 rm x > out y', ['rm x']],
  ['unset A; export B=1; [ -f x ] && local c=2', ['unset', 'export', 'local', '[']], ['! unset A', ['unset']], ['let x=1+2', ['let']],
  ['rm    -rf\t x', ['rm -rf']], ['RM -rf x', ['rm']], ['echo rm', ['rm']], ["$'r\\x6d' -rf x", ['rm']], ['f() { rm x; }; f', ['rm']],
  ['( cd /tmp && rm x ) &', ['rm']], ['git commit -m "rm old files"', ['rm']], ['echo a\nrm b', ['e*b']], ['echo a b', ['e*b']],
];
const denylist = denylistCases.map(([command, rules]) => ({command, rules, ...desktop.denylist(command, rules)}));

const output = {
  provenance: {
    version: desktop.version, platform: 'linux', method: 'isolated pure functions from source; not desktop traffic',
    source: 'local-exec-daemon/main.cjs',
  },
  ranges: cases.map(c => ({...c, result: desktop.ranges(c.text, c.args)})),
  streaming, fallback,
  discoveryMissing: desktop.discoveryMissing('codex'),
  discoveryTitles, commandTitles, denylist,
  terminalHeader: desktop.terminalHeader({
    pid: 42, cwd: '/workspace', command: 'printf hello', status: 'running', startedAt: '2026-01-01T00:00:00.000Z', runningForMs: 0,
  }),
  format, imageFit,
};
process.stdout.write(JSON.stringify(output, null, 2) + '\n');
if (process.argv[3] !== undefined) fs.writeFileSync(process.argv[3], JSON.stringify(desktop.symbols, null, 2) + '\n');

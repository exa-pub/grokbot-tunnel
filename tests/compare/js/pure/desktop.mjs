// The pure functions of the desktop client the reference data is taken
// from, by what they do. Each is found in the bundle by an anchor; a release
// that changes one of them fails here with the name of the anchor.
import fs from 'node:fs';
import path from 'node:path';
import util from 'node:util';
import Module, {createRequire} from 'node:module';
import {Bundle} from './bundle.mjs';

// The read stream of node:fs, from memory: a "file" is its text, read in
// chunks of the size the reader asks for.
const chunked = (text, size) => ({
  async *[Symbol.asyncIterator]() {
    const bytes = Buffer.from(text);
    for (let i = 0; i < bytes.length; i += size) yield bytes.subarray(i, i + size);
  },
});

// open loads the daemon's bundle of an unpacked desktop client; dist is its
// app/dist directory.
export function open(dist) {
  const daemon = new Bundle(path.join(dist, 'local-exec-daemon', 'main.cjs'));
  const version = JSON.parse(fs.readFileSync(path.join(dist, '..', 'package.json'), 'utf8')).version;

  // applyRange(text, lineCount, args): a read with a line range.
  const ranges = daemon.find('ranges', String.raw`function (ID)\(\w,\w,\w\)\{let \w=ID\(\w,\w==="",\w\);return \w\.applied\?`);
  // readTextStreaming(ctx, path, args, encoding, ?, size): the reader the
  // Linux read executor uses.
  const reader = daemon.find('streamReader',
    String.raw`async function (ID)\(\w,\w,\w,\w,\w,\w\)\{let \w=\w\.offset!==void 0\|\|\w\.limit!==void 0,`);
  // What the executor does to the reader's content when max_chars cut it.
  const trim = daemon.find('trimToMaxChars',
    String.raw`function (ID)\(\w,\w,\w\)\{if\(!\w\|\|\w\.maxChars===void 0\|\|\w\.length-1<=\w\.maxChars\)return \w;`);
  const mostlyUtf8 = daemon.find('mostlyUtf8',
    String.raw`function (ID)\(\w\)\{let \w=0,\w=0,\w=0,\w=0;for\(let \w=0;\w<\w\.length;\w\+\+\)\{let \w=\w\.charCodeAt\(\w\);\w<128\?`);
  const missing = daemon.find('discoveryMissing', String.raw`function (ID)\(\w\)\{return\{source:\w,found:!1,sessionCount:0,items:\[\]\}\}`);
  // discoveryTitle(text, limit): a text as a discovery title, with keys,
  // links, emails and paths replaced, cut to limit; and the title of a
  // command, its parts joined.
  const title = daemon.find('discoveryTitle',
    String.raw`function (ID)\(\w,\w=ID\)\{let \w=\w\.replace\(/\\s\+/g," "\)\.trim\(\);for\(let \w of ID\)\w=\w\.replace\(\w,"\[redacted\]"\);`);
  const commandTitle = daemon.find('commandTitle',
    String.raw`function (ID)\(\w\)\{return ID\(\w\.map\(\w=>${title.replaceAll('$', '\\$')}\(\w,Number\.POSITIVE_INFINITY\)\)\.join\(":"\),ID\)\}`);
  // parseCommand(command): what a shell command consists of, by the
  // desktop's bash grammar; and denylist({command, parsingResult,
  // blockedCommands}): why an administrator's denylist refuses it, if it does.
  const parse = daemon.find('parseCommand', String.raw`=new ID\((ID)\(\w\)\.structured\),`);
  const denylist = daemon.find('denylist',
    String.raw`function (ID)\(\{command:\w,parsingResult:\w,blockedCommands:\w\}\)\{if\(\w\.length===0\)return;`);
  // The grammar is a native module of the client; node-gyp-build, which it
  // loads itself with, is found through NODE_PATH.
  const deps = path.join(dist, 'deps');
  const depsRequire = name => {
    if (!(process.env.NODE_PATH ?? '').split(path.delimiter).includes(deps)) {
      process.env.NODE_PATH = [deps, process.env.NODE_PATH].filter(Boolean).join(path.delimiter);
      Module._initPaths();
    }
    return createRequire(path.join(deps, 'index.js'))(name);
  };
  daemon.module('tree-sitter', () => depsRequire('tree-sitter'));
  daemon.module('tree-sitter-bash', () => depsRequire('tree-sitter-bash'));
  const header = daemon.find('terminalHeader', String.raw`function (ID)\(\w\)\{let \w=String\(\w\.runningForMs\)\.padEnd\(`);
  // getFormatForFile's classifier of the start of a file, and the function
  // that cuts a chunk at a character boundary first.
  const classify = daemon.find('classifyFormat', String.raw`function (ID)\(\w\)\{if\(\w\.length===0\)return ID\(\);let \w=ID\(\w\)\?\?ID\(\w\);`);
  const format = daemon.find('format', String.raw`function (ID)\(\w\)\{return ${classify}\(\w\.length===ID\?ID\(\w\):\w\)\}`);
  const fit = daemon.find('imageFit',
    String.raw`function (ID)\(\w,\w,\w=ID\)\{if\(\w<1\|\|\w<1\)return\{width:\w,height:\w,needsResize:!1\};`);

  // What the functions reach outside themselves.
  // The reader's chunks: the file is read from memory instead.
  daemon.stub(daemon.find('fs', String.raw`\(0,(ID)\.createReadStream\)\(\w,\{highWaterMark:ID,signal:\w\.signal,`), {
    createReadStream: (text, options) => chunked(text, options.highWaterMark),
  });
  const classifySource = daemon.functionSource(classify);
  // jschardet is not run: inputs that would reach it are left out.
  daemon.stub(daemon.find('jschardet', String.raw`(ID)\.default\.detect\(`, classifySource), {
    default: {detect() { throw new Error('jschardet'); }},
  });
  const decode = String.raw`(ID)\.default\.decode\(\w\.subarray\(0,ID\),(?:ID)\((?:ID)\(\w\)\)\)`;
  daemon.stub(daemon.find('iconv', decode, classifySource), {
    default: {decode: (bytes, encoding) => new util.TextDecoder(encoding.startsWith('utf-16') ? encoding : 'utf-8').decode(bytes)},
  });
  // The two functions that turn an encoding's label into iconv's name.
  daemon.stub(daemon.find('iconvName', decode.replace('(ID)', 'ID').replace('(?:ID)', '(ID)'), classifySource), name => name);
  daemon.stub(daemon.find('encodingLabel', decode.replace('(ID)', 'ID').replace(String.raw`(?:ID)\((?:ID)`, 'ID\\((ID)'), classifySource),
    name => name.toLowerCase());

  return {
    version,
    symbols: daemon.symbols,
    ranges: (text, args) => daemon.call(ranges, text, text.split('\n').length, args),
    // A read of text as the Linux read executor runs it.
    async stream(text, args) {
      const encoding = {kind: 'strictUtf8', cacheEncoding: 'utf8'};
      const result = await daemon.callAsync(reader, {signal: undefined}, text, args, encoding, false, Buffer.byteLength(text));
      return {...result, content: daemon.call(trim, result.content, result.truncated, args)};
    },
    mostlyUtf8: lossy => daemon.call(mostlyUtf8, lossy),
    discoveryMissing: source => daemon.call(missing, source),
    terminalHeader: info => daemon.call(header, info),
    // limit is the longest title, the desktop's 160 if left out; null is no
    // limit.
    discoveryTitle: (text, limit) => (limit === undefined ? daemon.call(title, text)
      : daemon.call(title, text, limit ?? Number.POSITIVE_INFINITY)),
    commandTitle: parts => daemon.call(commandTitle, parts),
    // Whether an administrator's denylist refuses command: the reason, or
    // null, with the commands the desktop found in it.
    denylist(command, rules) {
      const parsed = daemon.call(parse, command).structured;
      return {
        reason: daemon.call(denylist, {command, parsingResult: parsed, blockedCommands: rules}) ?? null,
        commands: parsed.executableCommands.map(c => c.fullText),
        parsingFailed: parsed.parsingFailed,
      };
    },
    // The classification of the start of a file, or null if it would take
    // jschardet to tell.
    format(bytes) {
      try {
        const {encoding, isBinaryFile, isImageFile} = daemon.call(format, bytes);
        return {encoding, isBinaryFile, isImageFile};
      } catch (error) {
        if (error.message !== 'jschardet') throw error;
        return null;
      }
    },
    imageFit: (width, height, side) => daemon.call(fit, width, height, side),
  };
}

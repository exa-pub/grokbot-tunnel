// Answers calls of the desktop client's pure functions, for Go tests that
// compare their own functions with them on many inputs.
//
//   node oracle.mjs <app/dist>
//
// One JSON object a line on stdin, {"fn": name, "args": [...]}, gives one a
// line on stdout: {"result": ...} or {"error": text}. Bytes are passed as
// {"hex": "..."}.
import readline from 'node:readline';
import {open} from './desktop.mjs';

const desktop = open(process.argv[2]);
const functions = {
  ranges: (text, args) => desktop.ranges(text, args),
  stream: (text, args) => desktop.stream(text, args),
  mostlyUtf8: lossy => desktop.mostlyUtf8(lossy),
  format: bytes => desktop.format(bytes),
  imageFit: (width, height, side) => desktop.imageFit(width, height, side),
  terminalHeader: info => desktop.terminalHeader(info),
  discoveryMissing: source => desktop.discoveryMissing(source),
  discoveryTitle: (text, limit) => desktop.discoveryTitle(text, limit),
  commandTitle: parts => desktop.commandTitle(parts),
  version: () => desktop.version,
};
const decode = value => (value !== null && typeof value === 'object' && typeof value.hex === 'string' ? Buffer.from(value.hex, 'hex') : value);

for await (const line of readline.createInterface({input: process.stdin})) {
  let answer;
  try {
    const {fn, args} = JSON.parse(line);
    if (!Object.hasOwn(functions, fn)) throw new Error(`no function ${fn}`);
    answer = {result: (await functions[fn](...args.map(decode))) ?? null};
  } catch (error) {
    answer = {error: String(error?.stack ?? error)};
  }
  process.stdout.write(JSON.stringify(answer) + '\n');
}

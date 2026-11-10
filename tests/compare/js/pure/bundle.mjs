// Runs single functions of the desktop client's bundle in isolation.
//
// The bundle is minified: its names change with every release. A function
// is therefore found by an anchor, a pattern over what it does, and what it
// uses is taken from the bundle when it turns out to be missing: a call that
// fails with "X is not defined" has X looked up, defined and is tried again.
// Nothing of the application starts: only the pieces a call reaches are
// evaluated, in a vm context without the file system or the network.
import fs from 'node:fs';
import path from 'node:path';
import util from 'node:util';
import vm from 'node:vm';
import {createRequire} from 'node:module';

const nodeRequire = createRequire(import.meta.url);
// Modules a pure function may need. Anything that reaches the file system,
// processes or the network must be given as a stub instead.
const allowed = new Set(['node:buffer', 'node:util', 'node:string_decoder', 'node:path', 'buffer', 'util', 'string_decoder', 'path']);

const id = String.raw`[\w$]+`;

export class Bundle {
  // file is a bundle of the unpacked desktop client, e.g.
  // <dist>/local-exec-daemon/main.cjs.
  constructor(file) {
    this.file = file;
    this.source = fs.readFileSync(file, 'utf8');
    // Logical name → minified name, for symbols.json.
    this.symbols = {};
    // Modules of the desktop client a function may need, loaded on first
    // use; see module.
    this.modules = new Map();
    this.context = vm.createContext({
      Buffer, Math, TextDecoder: util.TextDecoder, TextEncoder: util.TextEncoder, process: {platform: 'linux', env: {}},
      require: name => {
        if (this.modules.has(name)) return this.modules.get(name)();
        if (!allowed.has(name)) throw new Error(`the bundle requires ${name}: give a stub for what uses it`);
        return nodeRequire(name);
      },
    });
    this.defined = new Set(Object.keys(this.context));
  }

  // find returns the minified name an anchor captures. The anchor is a
  // pattern with one group; in it, ID stands for an identifier. It must match
  // exactly one name, in `within` (the source of a function) or the bundle.
  find(name, anchor, within = this.source) {
    const pattern = new RegExp(anchor.replaceAll('ID', id), 'g');
    const names = new Set([...within.matchAll(pattern)].map(m => m[1]));
    if (names.size !== 1) {
      throw new Error(`${path.basename(this.file)}: anchor "${name}" matches ${names.size} names, want 1: ${anchor}`);
    }
    const [found] = names;
    this.symbols[name] = found;
    return found;
  }

  // functionSource returns the source of the function or class `name`, or
  // null if the bundle defines none.
  functionSource(name) {
    const forms = [`async function*${name}(`, `async function ${name}(`, `function*${name}(`, `function ${name}(`, `var ${name}=class`];
    for (const form of forms) {
      const start = this.source.indexOf(form);
      if (start < 0) continue;
      for (let end = this.source.indexOf('}', start); end >= 0; end = this.source.indexOf('}', end + 1)) {
        const code = this.source.slice(start, end + 1);
        try {
          new vm.Script(form.startsWith('var ') ? code : `(${code})`);
          return code;
        } catch {
          // Not the closing brace yet.
        }
      }
    }
    return null;
  }

  // valueSource returns the expression the bundle assigns to the variable
  // `name` where it is initialized, or null.
  valueSource(name) {
    const assignment = new RegExp(String.raw`(?<![\w$.])${name.replaceAll('$', '\\$')}=(?![=>])`, 'g');
    for (const match of this.source.matchAll(assignment)) {
      const start = match.index + match[0].length;
      for (let end = start + 1; end <= this.source.length && end - start < 200000; end++) {
        const next = this.source[end];
        if (next !== ',' && next !== ';' && next !== ')' && next !== '}' && end !== this.source.length) continue;
        const code = this.source.slice(start, end);
        try {
          new vm.Script(`(${code})`);
          return code;
        } catch {
          // The expression goes on.
        }
      }
    }
    return null;
  }

  // module lets the bundle require `name`, which load returns: a module
  // that neither reads files nor starts anything, such as a parser.
  module(name, load) {
    let loaded;
    this.modules.set(name, () => (loaded ??= load()));
  }

  // stub gives `name` a value instead of what the bundle defines.
  stub(name, value) {
    this.context[name] = value;
    this.defined.add(name);
  }

  // define evaluates the bundle's definition of `name` in the context.
  define(name) {
    if (this.defined.has(name)) throw new Error(`${name} is defined and still reported missing`);
    this.defined.add(name);
    const code = this.functionSource(name);
    if (code !== null) {
      this.run(() => vm.runInContext(code, this.context));
      return;
    }
    const value = this.valueSource(name);
    if (value === null) throw new Error(`${path.basename(this.file)} does not define ${name}`);
    this.run(() => vm.runInContext(`var ${name}=(${value})`, this.context));
  }

  // run calls fn, defining what it finds missing, until it returns.
  run(fn) {
    for (;;) {
      try {
        return fn();
      } catch (error) {
        this.resolve(error);
      }
    }
  }

  // runAsync is run for a function that returns a promise.
  async runAsync(fn) {
    for (;;) {
      try {
        return await fn();
      } catch (error) {
        this.resolve(error);
      }
    }
  }

  // resolve defines the name a ReferenceError reports, or throws the error.
  resolve(error) {
    const missing = error?.name === 'ReferenceError' && /^([\w$]+) is not defined$/.exec(error.message);
    if (!missing) throw error;
    this.define(missing[1]);
  }

  // call calls the function with the minified name `name`.
  call(name, ...args) {
    this.context.__args = args;
    return this.run(() => vm.runInContext(`${name}(...__args)`, this.context));
  }

  callAsync(name, ...args) {
    this.context.__args = args;
    return this.runAsync(() => vm.runInContext(`${name}(...__args)`, this.context));
  }
}

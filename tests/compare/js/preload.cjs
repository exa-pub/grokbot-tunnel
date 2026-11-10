// Loaded into the desktop's daemon with --require when a scenario asks for a
// patch of the bundle. A patch replaces one piece of the daemon's source as it
// is compiled; the piece is found by a text that survives minification and
// must occur exactly once, so a release that changes it fails loudly instead
// of running unpatched.
//
//   GROKBOT_COMPARE_PATCHES=ripgrep,...
'use strict';
const Module = require('node:module');
const path = require('node:path');

const patches = {
  // The released daemon never sets its ripgrep path, so every listing ends in
  // "Ripgrep path not configured". With the path of the image's rg the code
  // behind that error runs: this is how the listing was meant to work.
  ripgrep: {
    find: 'throw new Error("Ripgrep path not configured. Call configureRipgrepPath() at startup.")',
    replace: 'return "/usr/bin/rg"',
  },
};

const wanted = (process.env.GROKBOT_COMPARE_PATCHES ?? '').split(',').filter(Boolean);
delete process.env.GROKBOT_COMPARE_PATCHES;
for (const name of wanted) {
  if (!Object.hasOwn(patches, name)) throw new Error(`preload.cjs: unknown patch ${name}`);
}

const compile = Module.prototype._compile;
Module.prototype._compile = function (source, filename) {
  if (path.basename(filename) === 'main.cjs' && path.basename(path.dirname(filename)) === 'local-exec-daemon') {
    for (const name of wanted) {
      const {find, replace} = patches[name];
      const parts = source.split(find);
      if (parts.length !== 2) throw new Error(`preload.cjs: patch ${name} matches ${parts.length - 1} places, want 1`);
      source = parts.join(replace);
    }
  }
  return compile.call(this, source, filename);
};

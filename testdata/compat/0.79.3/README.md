# Grok Bot 0.79.3 reference data

What the desktop client's Linux bundle does, for tests that need neither the
client nor Node. `make compare-record` regenerates all of it but the TLS
probe; `tests/compare/README.md` says how the comparison works.

`pure-functions.json` holds outputs of pure functions of the bundle: read
ranges, the streaming text reader the Linux read executor uses (including
`max_chars`), the choice between lossy UTF-8 and Latin-1 for invalid UTF-8,
the missing-setup discovery result, the titles of discovered items with
what is replaced in them (`discoveryTitles`, `commandTitles`), the simple
commands found in a shell command and what an administrator's denylist says
of it (`denylist`), the terminal header, how the start of a
file is classified (`format`: UTF-16/UTF-32, binary, image; inputs that would
reach jschardet are left out) and the size an image is scaled to (`imageFit`).
`tests/compare/js/pure/reference.mjs` runs only these functions, in a `vm`
context, feeding the reader its 64 KiB file chunks from memory; the
application is not started. The denylist cases alone load a native module
of the client, its bash grammar, which needs the Node of the comparison
image. `symbols.json` lists the minified names the functions have in this
release.

`transcripts/` holds what the client's local-exec daemon answered to the
requests of the comparison scenarios (`tests/compare/scenarios`), one file a
scenario. For these the daemon itself ran, in a container without network,
against the fake backend of `tests/e2e/fakesand`, with the files and requests
of the scenario and nothing else. `known-diffs.json` lists where
grokbot-tunnel differs from these answers and why.

`tls-runtime-probe.json` compares, on loopback, the TLS ClientHello of the Go
transport with Node's `https.Agent` from the client's Electron runtime. It was
measured with 0.75.8; the 0.77.0, 0.78.1, 0.78.4 and 0.79.3 runtime executables
are byte-identical to it. The cipher suites, supported groups and signature
algorithms differ; TLS versions and ALPN match. Extension order is not
recorded, so this is not a JA3/JA4 fingerprint. The official executable cannot
run as Node, so the measurement used a copy with the RunAsNode fuse enabled:

```sh
ELECTRON_RUN_AS_NODE=1 go run ./tools/compattls --node /path/to/runtime
```

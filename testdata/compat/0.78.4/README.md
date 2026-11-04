# Grok Bot 0.78.4 reference data

`pure-functions.json` holds outputs of pure functions from the desktop
client's Linux bundle: read ranges, the streaming text reader the Linux read
executor uses (including `max_chars`), the choice between lossy UTF-8 and
Latin-1 for invalid UTF-8, the missing-setup discovery result, the terminal
header, how the start of a file is classified (`format`: UTF-16/UTF-32,
binary, image; inputs that would reach jschardet are left out) and the size an
image is scaled to (`imageFit`). `scripts/compat-reference.mjs` runs only these
functions, feeding the reader its 64 KiB file chunks from memory; the
application itself is not executed. Go tests read the file and need neither Node nor the client.

```sh
node scripts/compat-reference.mjs ai.tmp/grok-bot/0.78.4/app/dist > testdata/compat/0.78.4/pure-functions.json
```

`tls-runtime-probe.json` compares, on loopback, the TLS ClientHello of the Go
transport with Node's `https.Agent` from the client's Electron runtime. It was
measured with 0.75.8; the 0.77.0, 0.78.1 and 0.78.4 runtime executables are
byte-identical to it. The cipher suites, supported groups and signature
algorithms differ; TLS versions and ALPN match. Extension order is not
recorded, so this is not a JA3/JA4 fingerprint. The official executable cannot
run as Node, so the measurement used a copy with the RunAsNode fuse enabled:

```sh
ELECTRON_RUN_AS_NODE=1 go run ./tools/compattls --node /path/to/runtime
```

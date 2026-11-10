# Comparison with the desktop client

These tests run the same requests against grokbot-tunnel and against the
desktop client's local-exec daemon and compare what the two do: the frames
they answer with, the files they leave and, for the channel scenarios, the
calls they make. A difference is either listed in
`testdata/compat/<version>/known-diffs.json` with its reason, or the test
fails.

Everything runs in a container without network. The clients talk to the fake
backend of `tests/e2e/fakesand` on the container's loopback.

## Running

Needs docker; set `DOCKER="sudo docker"` if your user may not use it directly.

| Command | What it does | Needs the desktop |
|---|---|---|
| `make compare-golden` | compares grokbot-tunnel with the recorded answers of the desktop | no |
| `make compare` | runs the desktop and grokbot-tunnel side by side | yes |
| `make compare-record` | the same, and records the desktop's answers | yes |
| `make compare-oracle` | compares single functions with the desktop's on random inputs | yes |
| `make compare-image` | builds the image with the desktop's bundle | yes |

CI runs `compare-golden`. The others are for a machine that has the desktop's
AppImage: `Grok_Bot_<version>.AppImage` in `tests/compare/desktop/`, or in
the directory `DESKTOP_DIR` names, or at the URL `docker/desktop.lock` gives
for the version. Git ignores that directory. The image built from it
holds the desktop's bundle, which is not ours to publish: do not push it.

`ARGS` go to the test binary and `DESKTOP_VERSION` picks a release other than
the one the recordings are of:

```sh
make compare ARGS='-test.run TestLive/shell/session'
make compare DESKTOP_VERSION=0.79.2
COMPARE_KEEP=1 make compare ARGS='-test.run TestLive/background/output$'
```

A run writes `tests/compare/out/<version>/report.md` (`COMPARE_OUT` names
another directory) and the transcripts of both clients next to it
(`desktop/`, `go/`). With `COMPARE_KEEP=1` it also keeps
what each client left in its root (`raw/`).

## How a scenario is compared

A scenario (`scenario.Scenario`, listed in `scenarios/`) is the files a client
starts with and the requests it gets. `Run` starts a client in a fresh home
and root, sends the requests through the fake backend and records a
transcript (`transcript.Transcript`): the frames of every request, the state
of the watched files and, if asked, the hello, the channel calls and their
headers. Both clients get the same paths, one after the other.

Before two transcripts are compared, `transcript.Normalize` removes what
differs between two runs of the same client: times, process IDs, the IDs of
background shells and of output files, how output is split into chunks, the
JavaScript stack of an error. Each rule names the field it is for. What a
client decides is never normalized: texts, exit codes, the order of a
listing, the order of the frames of a request.

Add to the built-in rules only what both clients vary in on their own. If one
scenario needs more (the order ripgrep walks a large directory in), give it
`Rules` of its own and say why.

## Recordings

`make compare-record` writes the desktop's transcript of every scenario to
`testdata/compat/<version>/transcripts/`, after running it three times with
the same result; a scenario whose answers vary is not recorded and must be
fixed first. `make compare-golden` runs grokbot-tunnel against these files in
an image without the desktop.

A recording carries the digest of its scenario and the versions of bash, zsh,
ripgrep and git it was made with. `compare-golden` fails with its own message
if the scenario changed since (record again), if a scenario has no recording,
or if the image has other versions of the programs (rebuild it, or record
again).

So adding or changing a scenario needs the desktop's AppImage: write the
scenario, look at `make compare ARGS='-test.run TestLive/<id>'`, fix
grokbot-tunnel or list the difference, then `make compare-record`.

## Known differences

`known-diffs.json` is a list of

```json
{"scenario": "shell/session-alias", "path": "requests/r2/*", "reason": "…", "ref": "…"}
```

`scenario` is an ID or a pattern (`channel/*`), `path` a pattern over the path
of a difference as the report prints it (`*` is one segment, `**` any number).
`desktop` and `go`, if given, are the values the difference must have. An
entry that no difference matches any more fails the test: remove it together
with the fix.

## What the desktop is given

The daemon is started by `js/launcher.mjs`, which does what the desktop's
main process does for it: writes the credential, the settings and a
heartbeat into the data root, and hands the daemon the file key and the
machine ID over IPC. The fake backend issues grokbot-tunnel the same
credential.

The released daemon cannot list directories: its ripgrep path is never set
(`ls/as-released`). Scenarios that compare listings set `Patches` to
`ripgrep`, and `js/preload.cjs` puts the path of the image's `rg` in place of
the error. A patch finds its place by a text that must occur exactly once.

Logging in, refreshing the token and issuing the credential are done by the
desktop's main process, which these tests do not run; they are covered by the
unit tests of `oauth`, `tokens` and `login`.

## Single functions

`js/pure/` runs single functions of the bundle in a `vm` context: line
ranges, the streaming reader, the classification of the start of a file, the
size an image is scaled to. A function is found by an anchor, a pattern over
what it does (`js/pure/desktop.mjs`); what it uses is taken from the bundle
when a call reports it missing. A release that changes a function fails with
the name of its anchor.

- `node tests/compare/js/pure/reference.mjs <app/dist> [symbols.json]` writes
  `testdata/compat/<version>/pure-functions.json`, which the `compat_test.go`
  files of the modules check in `make test`. `make compare-record` runs it.
- `js/pure/oracle.mjs` answers calls of these functions. The `oracle_test.go`
  files (build tag `compare`) compare the Go functions with it on random
  inputs; `make compare-oracle` runs them. A difference they find belongs in
  `reference.mjs` as a case, so that `make test` keeps checking it.

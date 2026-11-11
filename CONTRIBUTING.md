# Contributing

Requires Go 1.25+. Build, test, lint and code generation commands are in the
`Makefile`.

To connect to a different server, set `SAND_BACKEND_URL` (API address) and
`SAND_CURSOR_WEBSITE_URL` (sign-in page address).

Shell tests run against bash and, when found, zsh: set `GROK_TEST_ZSH` to a zsh
binary or put zsh on `PATH`.

`testdata/compat/1.0.1/` holds reference data from the desktop client;
`make compare-record` regenerates it (see `tests/compare/README.md`).
`scripts/extract-proto.py` reconstructs `.proto` schemas from the client's
bundle without running it; `api/proto/` is a subset of its output.

## Code layout

Packages under `internal/` form layers; a package imports only its own layer
or lower ones. `golangci-lint` enforces this with `depguard` (see
`.golangci.yml`).

| Layer | Packages | Holds |
|---|---|---|
| 1 | `entity`, `gen` | domain values without I/O (identity, machine, credential, permission mode); generated protobuf code |
| 2 | `pkg/*` | code without business logic: `frames` (user-computer frames), `textcodec`, `proc`, `atomicfile`, `nodeerr` (Node-style errno texts), `uuid`, `envvar` |
| 3 | `adapter/*`, `repository/*` | the outside world: `sandapi` (transport profiles, headers), `oauth`, `githubrelease`, `systemd`, `tty`; `staterepo` (`~/.grok/bot-tunnel`), `instancelock` |
| 4 | `service/*` | the logic: `computer`, `localexec`, `permission`, `tokens`, `login`, `install`, `selfupdate` |
| 5 | `api/cli` | cobra commands: flags become the options of one call |
| 6 | `app` | assembly: reads the environment, builds clients and services, runs the command line |

Further rules:

- A service does not import another service; the consumer declares an
  interface (`computer.Handler`, `localexec.Gate`, `sandapi.TokenSource`).
- The protocol is the domain: `aiserverv1`/`agentv1` messages pass through the
  services as they are.
- `service/computer` is a session loop around synchronous state machines
  (`credential`, `watch`, `admission`, `replies`): they take an event and the
  time and return what to do; goroutines and RPCs stay in the session.
- `service/localexec` mirrors the desktop's local-exec daemon: a dispatcher,
  `execstream` (exec IDs, heartbeats, the final frame), `pathroot` (the root
  check), `shellsession` (the shared shell state) and one module per tool under
  `tool/`. Tools do not import each other, except `shell` → `background`.
- A module's main file is named after it and holds its contract and `New`;
  the type comment of a module that mirrors a desktop component names a stable
  anchor in the desktop bundle (a log tag, `withName`, an error text).
- Texts the agent sees repeat the desktop's word for word; they are part of
  the protocol, not style.

## Tests

- `make test`, `make test-race`, `make e2e`, `make compare-golden`,
  `make lint`. CI installs zsh and ripgrep so that their code paths run.
- Unit tests of a module live next to it, in the `<module>_test` package when
  the contract is enough. State machines are tested with tables of events and
  times, without goroutines or `sleep`.
- Reference outputs of the desktop's pure functions live in
  `testdata/compat/<version>/` and are loaded with `internal/pkg/compattest`;
  each module checks the part it owns in `compat_test.go`.
- `pinned_test.go` files pin current behavior, including known differences
  from the desktop; a test comment names the difference. Changing such a
  behavior means changing its test in the same change.
- `tests/compare` runs scenarios against the built binary and compares its
  answers with the desktop client's: `make compare-golden` (needs docker)
  with the answers recorded in `testdata/compat/<version>/transcripts/`,
  `make compare` with the desktop itself, for those who have its AppImage.
  A difference from the desktop is fixed or listed with its reason in
  `testdata/compat/<version>/known-diffs.json`; a new or changed scenario is
  recorded with `make compare-record`. See `tests/compare/README.md`.
- `tests/e2e` runs the built binary against `tests/e2e/fakesand`, a fake
  backend that `internal/app`'s integration test uses too.
- `make systemd-test` (needs docker) boots systemd in a container and runs
  login, install, update from a fake GitHub (`GROKBOT_TUNNEL_RELEASES_URL`),
  the restart and uninstall, as root and as a user with lingering.

## Releases

Every push to `main` is released once CI passes: there is no tag to push.
The first job of `.github/workflows/ci.yml` computes the version from the
latest `vX.Y.Z` tag and the conventional commits since then: `type!:` or a
`BREAKING CHANGE:` footer bumps the major version, `feat:` the minor one,
anything else the patch. Pull requests are built as that version with a
`-g<sha>` suffix, which `grokbot-tunnel update` treats as a development build.
The release job creates the tag and a GitHub Release with the linux/amd64 and
linux/arm64 archives and `checksums.txt`; a re-run of a released commit keeps
its version and skips the release. `grokbot-tunnel update` and the update
timer installed by `grokbot-tunnel install` download these archives, so keep
their names and the `checksums.txt` format.

Package builds should pass
`-X github.com/exa-pub/grokbot-tunnel/internal/api/cli.packager=<name>` in
`-ldflags`: such builds refuse `update` and do not install the update timer.

# grokbot-tunnel

[![CI](https://github.com/exa-pub/grokbot-tunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/exa-pub/grokbot-tunnel/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/exa-pub/grokbot-tunnel)](https://github.com/exa-pub/grokbot-tunnel/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/exa-pub/grokbot-tunnel)](go.mod)
[![License: MIT](https://img.shields.io/github/license/exa-pub/grokbot-tunnel)](LICENSE)

**Headless Grok Bot for Linux servers.** grokbot-tunnel is a CLI that connects a
headless Linux server (VPS, cloud VM, home server, Docker container, Raspberry
Pi) to Grok Bot as a local computer. The Grok Bot agent can then run commands
and work with files on that server, the same way it does on a machine with the
Grok Bot desktop app, but without a desktop, GUI or browser.

It is a single static Go binary that does the job of both the headless
controller and the local execution daemon: it signs in, registers the machine
as a remote computer and executes the agent's requests itself. You don't need
the Grok Bot desktop app, Electron, Node.js, X11, Wayland or Xvfb on the server.

Use it to run Grok Bots on a remote server over SSH or give the agent access to
a build machine. It runs as a systemd service that connects at boot, restarts
on failure and keeps itself up to date.

## How to connect Grok Bot to a headless server

1. Install. The installer puts the binary in `~/.local/bin`. To install a
   specific release, pass its tag: `bash -s v0.1.0`.

   ```sh
   curl -fsSL https://raw.githubusercontent.com/exa-pub/grokbot-tunnel/main/install.sh | bash
   ```

2. Sign in. `login` prints a sign-in link that you open on any
   device. On a machine with a browser, plain `login` opens it for you. Use
   `--team ID` to select a team. This is how sign-in works on a headless server
   without a browser.

   ```sh
   grokbot-tunnel login
   ```

3. Start the service. `install` sets up a systemd service that makes the server
   appear as a computer in Grok Bot and runs the agent's requests. It returns
   once the server is connected, or fails with the service log.

   ```sh
   grokbot-tunnel install --permission always
   ```

   `--permission` decides what the agent may do, since the service has no
   terminal to ask you in: `always` runs every action the agent requests,
   `ask` runs only the actions you approve in Grok Bot, `never` refuses all
   unless Grok Bot manages permissions itself (see below).

The service starts at boot, restarts on failure and updates grokbot-tunnel
daily. Check on it with:

```sh
systemctl --user status grokbot-tunnel
journalctl --user -u grokbot-tunnel -f
```

Linux on x86_64 (amd64) and arm64 is supported. The server needs bash (or zsh,
if `$SHELL` points to it). `rg` (ripgrep) is
optional; with it, directory listings respect `.gitignore`. Without systemd,
e.g. in a Docker container, run `connect` instead (see below).

## Options

`install` and `connect` take the same flags. To change them for the service,
run `install` again with the new ones.

| Flag | Meaning |
|------|---------|
| `--permission` | `always` allows everything; `never` refuses everything; `ask` asks before each action. Required for `install`; `connect` defaults to `ask`. |
| `--root` | Directory for file operations and commands. Defaults to the home directory. |
| `--label` | Computer name in Grok Bot, at most 100 characters. Saved for later runs; defaults to the current host name. |
| `--no-auto-update` | `install` only: do not update grokbot-tunnel daily. |

With `ask`, the service runs only actions approved in Grok Bot, and `connect`
in a terminal asks there: `y` allows the action, `a` allows all actions until
`connect` exits (and from then on reports `always` to Grok Bot), anything else
refuses it. The question says when a command can keep running after the
request ends; requests the tunnel does not support are refused without asking.
A `--permission` given explicitly (always the case for the service) is also
set as the computer's permission in Grok Bot at every start; a team policy may
keep a lower one, which the log then says. A plain `connect` without the flag
leaves the setting in Grok Bot as it is. If Grok Bot manages permissions
itself, the tunnel never asks and runs exactly the actions Grok Bot approved,
by that setting; its log then says so. A change made in Grok Bot lasts until
the tunnel starts again with `--permission`. To refuse all actions, start it
with `--permission never` or stop it (`grokbot-tunnel uninstall`, or stop
`connect`).

## Managing the service

- A regular user gets a service of their systemd user manager
  (`~/.config/systemd/user/grokbot-tunnel.service`), and lingering is enabled so
  that it runs without a login session. Root gets a system service
  (`/etc/systemd/system/grokbot-tunnel.service`); drop `--user` from the
  `systemctl` and `journalctl` commands.
- The service keeps the `PATH` and `SHELL` of the shell you run `install` in,
  so the agent's commands find the same programs.
- The service and the daily update also keep the proxy (`HTTPS_PROXY`,
  `HTTP_PROXY`, `NO_PROXY`, in either case) and CA (`SSL_CERT_FILE`,
  `SSL_CERT_DIR`) settings of that shell. A proxy URL with credentials is kept
  in `~/.grok/bot-tunnel/service.env`, readable only by you, instead of the
  unit file. Re-run `install` after changing them.
- Stopping or restarting the service also stops background commands the agent
  started.
- After the login is revoked the service stops; run `grokbot-tunnel login`
  and then `install` again.
- If the computer is removed in Grok Bot while the tunnel runs, the tunnel
  stops and systemd does not restart it; run `install` (or `connect`) again to
  add it back. The service is still enabled, so the computer comes back the
  next time the service starts, e.g. after a reboot; run
  `grokbot-tunnel uninstall` to keep it removed.
- `grokbot-tunnel uninstall` stops and removes the service and keeps the binary
  and your login.

## Update

The service updates itself daily unless installed with `--no-auto-update`. To
update now:

```sh
grokbot-tunnel update
```

`update` downloads the latest release (or the one given, e.g.
`grokbot-tunnel update v0.2.0`), verifies its checksum, replaces the binary and
restarts the service. `--check` only shows the versions; `--force` reinstalls
the same version or downgrades; `--no-restart` leaves the service alone.

## Run in the foreground

`connect` runs the tunnel in your terminal until you press Ctrl-C, without a
service: to try it out, to approve actions in the terminal with
`--permission ask`, or where there is no systemd.

```sh
grokbot-tunnel connect
```

Ctrl-C stops running commands; background commands keep running. Closing the
terminal or SSH session also stops the tunnel cleanly. Only one
tunnel runs per user at a time, so stop the service first
(`systemctl --user stop grokbot-tunnel`); otherwise `connect` exits and says so.

## What the agent gets

- A persistent shell: each command continues from the working directory,
  variables, shell options and functions the previous one left (aliases are
  not kept). Background commands
  write their output to `<root>/terminals/<number>.txt`.
- File reads, directory listings, and uploads and downloads up to 100 MiB.
  Paths may start with `~` or be `file://` URLs; they are checked against
  `--root` after expansion.
- Names of Codex and Claude Code skills, MCP servers, plugins and commands, with
  keys, links, emails and paths removed.

## Security

- Commands run with your privileges. `--root` limits only file operations;
  commands start in it unless the agent asks for another directory, and can
  access anything you can. Codex and Claude Code
  settings are read from the home directory even if it is outside `--root`.
- Tokens are stored unencrypted in `~/.grok/bot-tunnel/`, readable only by you.
- `--debug` logs requests in full, including commands and file contents; for
  the service they go to the journal.

To build from source or work on the code, see [CONTRIBUTING.md](CONTRIBUTING.md).

grokbot-tunnel is an independent project, not affiliated with or endorsed by
xAI. Grok Bot is a trademark of its owner.

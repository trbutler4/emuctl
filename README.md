# emuctl

Android emulators for people and AI agents. emuctl boots and supervises a set
of emulators, hands them out through a FIFO lease queue, and exposes them over
an HTTP API, a CLI and an [MCP](https://modelcontextprotocol.io) server.

Use it on one laptop to give your coding agent an emulator it can install
builds on, tap around in and screenshot. Or run it on a build box and share a
pool of emulators with your team and CI.

- **MCP server.** `emuctl mcp` gives agents tools to acquire a device, run
  `adb shell` commands, take screenshots, install APKs, reset and release.
  Agents don't need adb or the Android SDK on their machine.
- **Lease queue.** One holder per device, FIFO across users. Ask for a specific
  device or any device. Leases expire without a heartbeat, so a crashed agent
  frees its device within `lease_ttl`.
- **Supervision.** Emulators restart on crash or boot timeout, with backoff. A
  device takes no leases until Android has booted and unlocked.
- **Clean devices.** Wipe and reboot on request, or after every lease with
  `reset_on_release`.
- **Holder-only adb.** Optionally forward each device's adb port. Only the
  current holder's IP can connect, and connections are dropped when the lease
  ends. That's enough for `adb`, Maestro, Appium or Android Studio.
- **Single static binary.** Pure Go, standard library only.

## Quick start (one machine)

You need the Android SDK with the emulator and at least one system image. If
you have Android Studio installed, you already have both.

```bash
go install github.com/trbutler4/emuctl/cmd/emuctl@latest
emuctl init      # writes ~/.config/emuctl/config.json with one device
emuctl doctor    # checks the SDK, system image and config
emuctl serve     # boots the emulator, API on 127.0.0.1:7600
```

Then add the MCP server to your agent (Claude Code, Cursor, Codex, jcode, etc.):

```json
{ "mcpServers": { "emuctl": { "command": "emuctl", "args": ["mcp"] } } }
```

For Claude Code: `claude mcp add emuctl -- emuctl mcp`.

With no users in the config, the API needs no token, and emuctl only allows
that when it listens on loopback.

## Install

```bash
go install github.com/trbutler4/emuctl/cmd/emuctl@latest   # Go 1.26+
nix profile install github:trbutler4/emuctl                 # Nix, Linux and Apple Silicon
nix run github:trbutler4/emuctl -- devices                  # without installing
```

As a flake input: `inputs.emuctl.url = "github:trbutler4/emuctl";`, then
`inputs.emuctl.packages.${system}.default` or `overlays.default` for
`pkgs.emuctl`.

The server runs the Android emulator, so it needs hardware virtualization:
Linux with `/dev/kvm`, or macOS. Clients (`mcp`, `run`, `acquire`) run
anywhere. `emuctl run` also needs `adb` on PATH.

## MCP tools

| Tool | |
| --- | --- |
| `emulator_usage` | workflow instructions for the agent |
| `emulator_devices` | devices, state and holders |
| `emulator_queue` | waiting leases |
| `emulator_acquire` | queue for a device, blocks until ready or `wait_seconds` |
| `emulator_status` | state of a lease |
| `emulator_shell` | `adb shell` command, returns output and exit code |
| `emulator_screenshot` | PNG of the screen as MCP image content, optionally saved to a file |
| `emulator_install` | install a local APK |
| `emulator_reset` | reboot, optionally wiping data, waits until ready |
| `emulator_release` | release the lease |
| `emulator_logs` | tail of the emulator log |

The MCP process heartbeats the leases it holds and releases them when it exits.

The client reads `EMUCTL_URL` (default `http://127.0.0.1:7600`) and the token
from `EMUCTL_TOKEN`, the file in `EMUCTL_TOKEN_FILE`, or
`~/.config/emuctl/token`. For a remote server:

```json
{ "mcpServers": { "emuctl": {
    "command": "emuctl", "args": ["mcp"],
    "env": { "EMUCTL_URL": "http://emu-host:7600", "EMUCTL_TOKEN_FILE": "/home/me/.config/emuctl/token" } } } }
```

## CLI

```bash
emuctl devices
emuctl queue
emuctl run -apk app.apk -- maestro test flows/     # queue, install, connect, run, release
emuctl run -reset -device pixel-1 -- ./e2e.sh
emuctl acquire -note "manual QA"                   # hold until Ctrl-C, prints lease JSON
emuctl shell LEASE pm list packages
emuctl screenshot LEASE screen.png
```

`run` exports `ANDROID_SERIAL`, `EMUCTL_SERIAL` (the `adb connect` target),
`EMUCTL_DEVICE` and `EMUCTL_LEASE` to the command. Maestro takes the serial as
`--device`:

```bash
emuctl run -- sh -c 'maestro --device "$EMUCTL_SERIAL" test flows/'
```

Maestro only talks to the default adb server (5037), so `run` connects the
device there with `adb connect`. Don't set `ANDROID_ADB_SERVER_PORT` for Maestro.

## Configuration

`~/.config/emuctl/config.json` (or `-config PATH`, or `EMUCTL_CONFIG`):

```jsonc
{
  "sdk": "/path/to/android-sdk",   // default: $ANDROID_SDK_ROOT, $ANDROID_HOME, then Android Studio's location
  "api": "127.0.0.1:7600",         // HTTP API
  "listen": "127.0.0.1",           // address for the adb forwarders
  "lease_ttl": "2m",               // lease lifetime without a heartbeat
  "max_lease_ttl": "4h",
  "boot_timeout": "6m",
  "reset_on_release": false,       // wipe the device after every lease
  "users": {},                     // name -> "sha256:..." token hash; empty = no auth, loopback only
  "devices": {
    "pixel-1": { "expose_port": 15601 },
    "tablet":  { "profile": "pixel_tablet", "width": 2560, "height": 1600, "density": 320, "expose_port": 15603 }
  }
}
```

Device fields:

| Field | Default | |
| --- | --- | --- |
| `image` | newest installed image for the host CPU | SDK package, e.g. `system-images;android-36;google_apis;arm64-v8a` |
| `profile` | `pixel_7` | hardware profile (`avdmanager list device`) |
| `expose_port` | none | TCP port on `listen` forwarded to the device's adb, for `adb connect` |
| `ram_mb`, `cores` | 2048, 4 | |
| `width`, `height`, `density` | 1080, 2400, 420 | |
| `gpu` | `swiftshader` | `host` is faster on machines with a GPU |
| `window` | `false` | show the emulator window instead of running headless |
| `console_port` | 5600 + 2n | even, so device ports never collide with other emulators |
| `avd_home`, `avd_name` | per-device AVD in `state_dir` | use an existing AVD instead |
| `unlock_pin` | none | PIN entered after boot if the AVD has a screen lock |
| `extra_args` | none | appended to the emulator command line |

emuctl creates each device's AVD under `state_dir` (default
`~/.local/share/emuctl`) on first start, with `avdmanager` if the SDK
command-line tools are installed and without them otherwise. It never touches
your Android Studio AVDs unless you point `avd_home` at them. To get another
system image: `sdkmanager "system-images;android-36;google_apis;arm64-v8a"`, or
Android Studio > Settings > Languages & Frameworks > Android SDK.

Each device gets its own private adb server, so emuctl never disturbs your
normal `adb` or Android Studio.

## Sharing over a network

To serve other machines, add users and bind to a reachable address:

```bash
emuctl add-user alice      # prints alice's token once, stores only its sha256
```

```jsonc
{ "api": "0.0.0.0:7600", "listen": "0.0.0.0", "users": { "alice": "sha256:..." }, ... }
```

Give each user their token. They put it in `~/.config/emuctl/token` and set
`EMUCTL_URL=http://your-host:7600`. `emuctl new-token` prints a token and its
hash separately, for configs managed elsewhere (e.g. Nix).

Security notes:

- The API uses bearer tokens over plain HTTP. Run it on a trusted network or
  VPN (Tailscale, WireGuard), put a TLS reverse proxy in front, or use SSH
  (below).
- The adb forwarders (`expose_port`) aren't authenticated by token. They admit
  only the current lease holder's IP. On a shared network, firewall those ports
  so only your clients can reach them. Leave `expose_port` unset if clients
  only use the MCP tools or the API.
- Anyone holding a lease has full control of that emulator, including
  `adb shell`. Treat emulators as disposable and use `reset_on_release` when
  sharing them.

## Over SSH (no open ports)

If clients can already SSH to the emulator host, nothing else needs to be
exposed. Keep `api` and `listen` on loopback (the defaults), add users, and
point clients at an `ssh://` URL:

```bash
export EMUCTL_URL=ssh://me@emu-host          # or ssh://emu-host:2222/127.0.0.1:7600
emuctl devices
emuctl run -- sh -c 'maestro --device "$EMUCTL_SERIAL" test flows/'
```

The client runs `ssh -L` itself: one forward for the API, opened on first
use, and one per leased device's adb port, opened when needed. `run`,
`acquire` and `emulator_acquire` return the local end of that forward as the
serial. Tunnels close when the command or MCP server exits. emuctl uses your
`ssh` binary, so `~/.ssh/config`, agents and keys all work. Auth must be
non-interactive (keys or an agent).

Through a tunnel every connection reaches the server from loopback, so the
holder-only check on adb ports can't tell SSH users apart. Anyone who can SSH
to the host can already reach its loopback ports. Tokens still separate users
and leases.

## Running as a service

systemd user unit:

```ini
[Unit]
Description=emuctl
After=network-online.target

[Service]
ExecStart=%h/go/bin/emuctl serve
Restart=on-failure

[Install]
WantedBy=default.target
```

## HTTP API

Bearer token auth (none in open mode), JSON.

| Method | Path | |
| --- | --- | --- |
| GET | `/v1/devices` | states, holders |
| GET | `/v1/queue` | waiting leases |
| POST | `/v1/leases` `{"device","note","ttl_seconds"}` | enqueue, `device` empty for any |
| GET | `/v1/leases/{id}?wait=30s` | long-poll |
| POST | `/v1/leases/{id}/renew` | heartbeat |
| DELETE | `/v1/leases/{id}` | release or leave the queue |
| POST | `/v1/leases/{id}/install` | body is the APK |
| POST | `/v1/leases/{id}/reset?wipe=1` | reboot, optionally wipe |
| POST | `/v1/leases/{id}/shell` `{"command"}` | `adb shell`, returns `{"output","exit_code"}` |
| GET | `/v1/leases/{id}/screenshot` | `image/png` |
| GET | `/v1/devices/{name}/logs` | emulator log tail |
| POST | `/v1/devices/{name}/restart` | restart a device you hold or nobody holds |

## Development

```bash
go test -race ./...
nix build                 # runs the tests too
nix flake check
```

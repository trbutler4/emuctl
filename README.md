# emuctl

Android emulators for people and AI agents. emuctl boots and supervises a set
of emulators, hands them out through a FIFO lease queue, and exposes them over
an HTTP API, a CLI and an [MCP](https://modelcontextprotocol.io) server.

Use it on one laptop to give your coding agent an emulator it can install
builds on, tap around in and screenshot. Or run it on a build box and share a
pool of emulators with your team and CI.

**Agents and LLMs:** start at [`llms.txt`](llms.txt) or
[`docs/`](docs/README.md), which cover usage in short, task-focused pages.

- **MCP server.** `emuctl mcp` gives agents tools to acquire a device, run
  `adb shell` commands, take screenshots, install APKs, reset and release.
  Agents don't need adb or the Android SDK on their machine.
- **Lease queue.** One holder per device, FIFO across users. Ask for a specific
  device or any device. Leases expire without a heartbeat, so a crashed agent
  frees its device within `lease_ttl`.
- **Supervision.** Emulators restart on crash or boot timeout, with backoff. A
  device takes no leases until Android has booted and unlocked. Idle devices
  are health-checked, and a device is only restarted on evidence it is
  broken, never because a check timed out. Emulators left behind by a killed
  `emuctl serve` are stopped on the next start.
- **Clean devices.** Between leases, clear your app's data without a reboot
  (`"release": "clear"`) or wipe and reboot (`"release": "wipe"`). Nobody
  gets the device until cleanup and a health check finish. Animations are
  turned off after boot and on every release.
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

## Stored APKs

The server keeps APKs under `state_dir/apks`, by a key you choose, so agents
switching branches don't re-upload or rebuild the same app. Use a key that
changes exactly when the native app changes, such as an Expo native
fingerprint: `android-<fingerprint>`. Entries are immutable, and the server
remembers which key each device last had, skipping the install when it
matches. A wipe or an ad hoc `-apk` install clears that.

```sh
key=android-$(node scripts/dev-client.mjs --hash)
emuctl run -apk-key "$key" -- maestro test flows/
# exit 3: not stored. Build once, upload, retry:
emuctl apk put "$key" app-debug.apk && emuctl run -apk-key "$key" -- maestro test flows/
```

`run -apk-key` checks the store before queueing, so a miss never holds a
device. Every builder must sign with the same keystore, or installing over an
existing app fails with `INSTALL_FAILED_UPDATE_INCOMPATIBLE`.

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
  "release": "none",               // between leases: none | clear | wipe (see below)
  "release_packages": [],          // packages to force-stop and `pm clear` with "clear"
  "release_commands": [],          // adb shell commands run on every release; failure restarts the device
  "disable_animations": true,      // animations off after boot and on release
  "health_interval": "2m",         // idle health checks, "0" disables
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
  `adb shell`. Treat emulators as disposable and use `"release": "wipe"` when
  sharing them with people you don't trust (`clear` only resets the listed apps).

## Between leases

When a lease ends, the device stays out of the pool until emuctl has:

1. dropped the old holder's adb connections,
2. cleaned up according to `release`,
3. run `release_commands` and turned animations off (skipped for `wipe`),
4. health-checked the device.

| `release` | Effect | Keeps installed APKs |
| --- | --- | --- |
| `none` | Nothing. Fastest; the next user sees the last app state. | yes |
| `clear` | `am force-stop` and `pm clear` each of `release_packages`. Seconds, no reboot. | yes |
| `wipe` | Reboot with `-wipe-data`. Minutes. | no |

For an app under test, `clear` is usually right: every lease starts signed
out with fresh app data, and stored APKs don't need reinstalling.

```jsonc
"release": "clear",
"release_packages": ["com.example.app", "com.example.app.dev"],
"release_commands": ["rm -f /data/local/tmp/my-test-marker"]
```

A failing cleanup step restarts the device. Health checks run on release and
every `health_interval` on idle devices. A device is restarted only on
evidence it is broken (its adb port is closed, or Android reports it is no
longer booted). A check that times out or can't reach adb is logged and
counted in `unverified` on `emuctl devices`, but the device stays in service,
so a slow host never restarts healthy emulators.

## Sizing

Emulators under CPU contention boot slowly and drop to `adb offline`, and an
overloaded host tends to fail every device, not just the extra one. `emuctl
doctor` warns when the configured `cores` or `ram_mb` add up to more than the
host has. Measure under real load: one report saw a 5th emulator on a 48-core
host time out on boot while the others ran tests.

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

## Metrics

`emuctl serve` exposes Prometheus metrics at `/metrics`. By default it shares
the API listener and needs a token like every other endpoint:

```yaml
scrape_configs:
  - job_name: emuctl
    authorization: {credentials_file: /etc/prometheus/emuctl-token}
    static_configs: [{targets: ["emulator-host:7600"]}]
```

Or set `"metrics_addr": "127.0.0.1:9600"` to serve `/metrics` on its own
listener without a token. Bind it to loopback or a monitoring network only.

| Metric | Labels | |
| --- | --- | --- |
| `emuctl_device_state` | device, state | 1 for the current state (stopped, starting, booted, crashed, resetting) |
| `emuctl_device_leased` | device | 1 while leased |
| `emuctl_device_state_seconds` | device | time in the current state |
| `emuctl_device_health_unverified` | device | inconclusive checks in a row (restarts at 3) |
| `emuctl_device_last_healthy_timestamp_seconds` | device | last passing check or boot |
| `emuctl_emulator_boots_total` | device, result | ok, timeout |
| `emuctl_emulator_boot_seconds` | device | start to booted and unlocked |
| `emuctl_emulator_exits_total` | device | unexpected exits |
| `emuctl_device_restarts_total` | device, reason | broken, unverified, cleanup, wipe, request |
| `emuctl_stale_emulators_reaped_total` | device | leftovers from an earlier emuctl |
| `emuctl_health_checks_total` | device, trigger, result | trigger idle or release; result healthy, unknown, broken |
| `emuctl_leases_granted_total`, `emuctl_leases_ended_total` | device (, reason) | reason released or expired |
| `emuctl_lease_queue_wait_seconds` | device | request to grant |
| `emuctl_lease_duration_seconds` | device, reason | grant to end |
| `emuctl_queue_length` | want | device name or `any` |
| `emuctl_queue_oldest_wait_seconds` | | |
| `emuctl_release_cleanup_seconds`, `emuctl_release_cleanup_failures_total` | device | |
| `emuctl_apk_installs_total` | device, source, result | source store or upload; result installed, skipped, failed |
| `emuctl_apk_install_seconds` | device, source | |
| `emuctl_apk_uploads_total` | result | stored, exists, rejected |
| `emuctl_apk_store_entries`, `emuctl_apk_store_bytes` | | |
| `emuctl_adb_forward_connections_total` | device, result | accepted, refused (not the lease holder) |
| `emuctl_adb_forward_open_connections` | device | |
| `emuctl_http_requests_total`, `emuctl_http_request_seconds` | route (, code) | route is the pattern, e.g. `GET /v1/leases/{id}` |
| `emuctl_build_info` | version | |

Labels stay bounded: device names come from the config, and lease IDs, users
and APK keys are never labels. Use the logs for per-lease detail.

Alert rules for the common failures (no booted device, flapping or
slow-booting devices, a growing queue, failing cleanups or installs, API
errors) are in
[`contrib/prometheus/emuctl-alerts.yml`](contrib/prometheus/emuctl-alerts.yml).

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

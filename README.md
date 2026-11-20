# emuctl

A pool of Android emulators on one KVM host, shared by people and agents.

- **FIFO lease queue.** One holder per device. Ask for a specific device or any device. Leases expire without a heartbeat, so a crashed agent frees its device within `lease_ttl`.
- **Holder-only adb.** Each device's adb port is forwarded on `listen:expose_port`. Only connections from the current holder's IP get through, and they are dropped when the lease ends.
- **Install and reset on request.** Agents upload an APK or wipe and reboot the device through the API. `reset_on_release` wipes the device between users.
- **Supervision.** Emulators restart on crash or boot timeout, with backoff. A device takes no leases until it has booted.
- **Agent friendly.** `emuctl mcp` is an MCP server with usage instructions. `emuctl run -- maestro test ...` wraps any command.

Networking is up to you. Bind `api` and `listen` to an address your clients can reach, for example a Tailscale IP.

## Server

```bash
emuctl init                      # ~/.config/emuctl/config.json
emuctl add-user alice            # prints a token once, stores its sha256
emuctl serve
```

Config:

```json
{
  "sdk": "/path/to/android-sdk",          // default $ANDROID_SDK_ROOT
  "listen": "0.0.0.0",                     // adb forwarders
  "api": "0.0.0.0:7600",
  "lease_ttl": "2m",
  "reset_on_release": true,
  "users": { "alice": "sha256:..." },
  "devices": {
    "pixel-1": { "profile": "pixel_7", "expose_port": 15601 },
    "pixel-2": { "profile": "pixel_7", "expose_port": 15603, "ram_mb": 4096 }
  }
}
```

Device fields: `profile`, `image` (SDK package), `console_port` (even, default
5600+2n so the host's default adb server ignores them), `expose_port`, `ram_mb`,
`cores`, `gpu`, `width`, `height`, `density`, `extra_args`. State (AVDs, logs)
lives in `state_dir`, default `~/.local/share/emuctl`.

## Clients

```bash
export EMUCTL_URL=http://gfxdesktop:7600 EMUCTL_TOKEN=emu_...
emuctl devices
emuctl queue
emuctl run -apk app.apk -- maestro test flows/     # queue, install, connect, run, release
emuctl run -reset -device pixel-1 -- ./e2e.sh
emuctl acquire -note "manual QA"                   # hold until Ctrl-C
```

`run` exports `ANDROID_SERIAL`, `EMUCTL_SERIAL` (the `adb connect` target),
`EMUCTL_DEVICE` and `EMUCTL_LEASE`. Maestro takes the serial as `--device`:

```bash
emuctl run -- sh -c 'maestro --device "$EMUCTL_SERIAL" test flows/'
```

Maestro only talks to the default adb server (5037), so `run` connects the
device there with `adb connect`. Don't set `ANDROID_ADB_SERVER_PORT` for Maestro.

## Agents (MCP)

```json
{ "mcpServers": { "emuctl": {
    "command": "emuctl", "args": ["mcp"],
    "env": { "EMUCTL_URL": "http://gfxdesktop:7600", "EMUCTL_TOKEN": "emu_..." } } } }
```

Tools: `emulator_usage`, `emulator_devices`, `emulator_queue`,
`emulator_acquire`, `emulator_status`, `emulator_install`, `emulator_reset`,
`emulator_release`, `emulator_logs`. The MCP process heartbeats the leases it
holds and releases them when it exits.

## API

Bearer token auth, JSON.

| Method | Path | |
| --- | --- | --- |
| GET | `/v1/devices` | states, holders |
| GET | `/v1/queue` | waiting leases |
| POST | `/v1/leases` `{"device","note"}` | enqueue |
| GET | `/v1/leases/{id}?wait=30s` | long-poll |
| POST | `/v1/leases/{id}/renew` | heartbeat |
| DELETE | `/v1/leases/{id}` | release |
| POST | `/v1/leases/{id}/install` | body is the APK |
| POST | `/v1/leases/{id}/reset?wipe=1` | reboot, optionally wipe |
| GET | `/v1/devices/{name}/logs` | emulator log tail |
| POST | `/v1/devices/{name}/restart` | restart an unleased device |

## Development

```bash
go test -race ./...
nix build
```

# Quickstart

## 0. Point at the server

```sh
export EMUCTL_URL=http://emu-host:7600      # or ssh://user@emu-host, default http://127.0.0.1:7600
export EMUCTL_TOKEN=emu_...                 # or put it in ~/.config/emuctl/token
emuctl devices                              # should list devices; 401 means a bad token
```

`ssh://` tunnels the API and adb over SSH, so nothing but SSH has to be open.

## 1. Run a command on a device (preferred)

`emuctl run` waits for a free device, connects your local `adb` to it, runs
your command, and releases the device when the command exits, even on
Ctrl-C or failure.

```sh
emuctl run -- adb shell getprop ro.build.version.release
emuctl run -apk app.apk -- maestro test flows/
emuctl run -device pixel-1 -note "PR 123 smoke" -- ./e2e.sh
```

Inside the command:

| Variable | Value |
| --- | --- |
| `ANDROID_SERIAL` | adb serial of your device. `adb`, Maestro and Gradle pick it up. |
| `EMUCTL_SERIAL` | same |
| `EMUCTL_DEVICE` | device name |
| `EMUCTL_LEASE` | lease id, for `emuctl shell/screenshot/install` |

The exit code of `emuctl run` is your command's exit code.

## 2. Hold a device interactively

```sh
emuctl acquire -note "debugging login"      # prints lease JSON, holds until Ctrl-C
# in another shell:
emuctl shell <lease-id> pm list packages
emuctl screenshot <lease-id> screen.png
emuctl install <lease-id> app.apk
```

`acquire` prints an `adb` field. `adb connect <adb>` lets local tools use it.

## 3. As an MCP agent

Add the server (see [mcp.md](mcp.md)), then:

1. `emulator_acquire` → returns `lease_id` once `ready` (call again with the
   same `lease_id` if it returns `queued`).
2. `emulator_install`, `emulator_shell`, `emulator_screenshot` with that
   `lease_id`.
3. `emulator_release` when done.

## What to check when it's slow

- `emuctl queue`: others are ahead of you. Your `-note` shows here, so
  others can see what you're doing.
- `emuctl devices`: a device in `starting`/`resetting` is booting and not
  available yet. `crashed` restarts on its own.

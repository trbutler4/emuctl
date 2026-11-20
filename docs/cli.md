# CLI reference

Client commands read `EMUCTL_URL` (default `http://127.0.0.1:7600`, or
`ssh://[user@]host[:port][/api-host:port]`) and the token from
`EMUCTL_TOKEN`, the file named by `EMUCTL_TOKEN_FILE`, or
`~/.config/emuctl/token`.

## Using devices

| Command | What it does |
| --- | --- |
| `emuctl devices` | Device, state, adb address, holder, restarts. |
| `emuctl queue` | Waiting leases in order. |
| `emuctl run [flags] -- CMD ARGS...` | Lease, set up, run CMD, release. Exit code is CMD's. |
| `emuctl acquire [-device NAME] [-note TEXT]` | Lease and hold until Ctrl-C. Prints lease JSON. |
| `emuctl release LEASE` | End a lease, or leave the queue. |
| `emuctl shell LEASE CMD...` | `adb shell` on the device, run by the server. Exit code is the command's. |
| `emuctl screenshot LEASE FILE.png` | Save a screenshot. |
| `emuctl install LEASE APK` | Upload and install an APK. |
| `emuctl install LEASE -key KEY` | Install a stored APK. |
| `emuctl reset LEASE [-wipe]` | Reboot (and wipe data), wait until ready. |
| `emuctl logs DEVICE` | Tail of the emulator log. |
| `emuctl apk list` | Stored APKs. |
| `emuctl apk has KEY` | Exit 0 if stored, 3 if not. |
| `emuctl apk put KEY FILE` | Store an APK. Existing key = success, not replaced. |
| `emuctl mcp` | MCP server on stdio. |

### `run` flags

| Flag | |
| --- | --- |
| `-device NAME` | A specific device. Default: first free. |
| `-apk PATH` | Install before running. Repeatable. |
| `-apk-key KEY` | Install a stored APK, skipped if the device has it. |
| `-reset` | Wipe the device first (slow: full reboot). |
| `-note TEXT` | Shown to others in `queue`/`devices`. Default: the command. |

Order inside `run`: lease → wait ready → `-reset` → `-apk-key` → `-apk` →
`adb connect` → CMD → release.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success (for `run`/`shell`: the command's own code is passed through). |
| 1 | emuctl error: server unreachable, lease lost, install failed. Message on stderr. |
| 2 | Bad usage. |
| 3 | APK key not stored (`run -apk-key`, `install -key`, `apk has`). Build and `apk put`. |

`run` passes your command's exit code through, so a command that itself
exits 1–3 is indistinguishable by code alone. When it matters, check the key
first with `emuctl apk has KEY`, whose 3 is unambiguous.

## Server commands

| Command | |
| --- | --- |
| `emuctl init` | Write a starter config (one local device, no auth). |
| `emuctl doctor` | Check SDK, images, KVM, config and host sizing. |
| `emuctl serve` | Run emulators, the queue and the API. |
| `emuctl add-user NAME` | Add a user; prints the token once. Restart `serve` to apply. |
| `emuctl new-token` | Print a token and its config hash. |

All take `-config PATH` (default `$EMUCTL_CONFIG` or
`~/.config/emuctl/config.json`).

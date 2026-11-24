# MCP

`emuctl mcp` is an MCP server on stdio. It talks to the emuctl API using the
same environment as the CLI.

```json
{ "mcpServers": { "emuctl": {
    "command": "emuctl", "args": ["mcp"],
    "env": { "EMUCTL_URL": "http://emu-host:7600", "EMUCTL_TOKEN_FILE": "/home/me/.config/emuctl/token" } } } }
```

The MCP process heartbeats every lease it holds and releases them when it
exits, so a crashed agent frees its device.

## Tools

| Tool | Arguments | Notes |
| --- | --- | --- |
| `emulator_usage` | | Workflow instructions. Call first if unsure. |
| `emulator_devices` | | Devices, state, holders. |
| `emulator_queue` | | Waiting leases. |
| `emulator_acquire` | `device?`, `note?`, `lease_id?`, `wait_seconds?` | Blocks up to `wait_seconds` (default 120, max 600). Returns `state: "queued"` if still waiting: call again **with the same `lease_id`** to keep your place. |
| `emulator_status` | `lease_id` | Current lease state. |
| `emulator_shell` | `lease_id`, `command` | `adb shell` on the server. Returns output and exit code. |
| `emulator_screenshot` | `lease_id`, `save_path?`, `source?` | PNG as image content. `source: "host"` shows lock/PIN prompts that the default capture returns black. |
| `emulator_unlock` | `lease_id` | Gets past the lock screen and app credential prompts (fingerprint, else the device PIN). |
| `emulator_finger` | `lease_id`, `finger?` | Touch the fingerprint sensor (finger 1 is enrolled by emuctl). |
| `emulator_install` | `lease_id`, `apk_path` | Installs a file from the MCP machine. |
| `emulator_reset` | `lease_id`, `wipe?` | Reboots (and wipes), waits until ready. |
| `emulator_release` | `lease_id` | Always call when done. |
| `emulator_logs` | `device` | Tail of the emulator log, for boot problems. |

## Typical loop

1. `emulator_acquire {"note": "checking login screen"}` → `lease_id`.
2. `emulator_install {"lease_id": ..., "apk_path": "/abs/path/app.apk"}`.
3. `emulator_shell {"lease_id": ..., "command": "am start -n com.example/.MainActivity"}`.
4. `emulator_screenshot {"lease_id": ...}` and look at it.
5. Interact with `emulator_shell` (`input tap X Y`, `input text ...`,
   `input keyevent KEYCODE_BACK`, `uiautomator dump /sdcard/ui.xml && cat /sdcard/ui.xml`).
6. `emulator_release {"lease_id": ...}`.

Tips:

- Screenshot after every action. UI changes are async; if the screen
  looks unchanged, wait a second and screenshot again before retrying.
- Find tap targets from `uiautomator dump` bounds rather than guessing
  coordinates from a screenshot.
- Animations are off on emuctl devices by default, so screens settle fast.
- `emulator_shell` runs one command string through `adb shell`, so quote
  for the device shell, e.g. `"command": "ls /sdcard | head"`.

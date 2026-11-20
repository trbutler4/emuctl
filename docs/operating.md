# Operating the server

## Minimal setup

```sh
emuctl init          # ~/.config/emuctl/config.json: one device, no auth, loopback only
emuctl doctor        # SDK, system image, KVM, sizing
emuctl serve
```

To share over a network, add users (`emuctl add-user NAME`, prints a token
once) and set `api`/`listen` to a reachable address. Without users, the
server refuses non-loopback addresses. Or keep it on loopback and have
clients use `EMUCTL_URL=ssh://host`.

## Config (`~/.config/emuctl/config.json`)

```jsonc
{
  "api": "127.0.0.1:7600",
  "listen": "127.0.0.1",                 // adb forwarders
  "metrics_addr": "",                    // "" = /metrics on the API with token
  "lease_ttl": "2m", "max_lease_ttl": "4h",
  "boot_timeout": "6m",
  "release": "clear",                    // none | clear | wipe
  "release_packages": ["com.example.app"],
  "release_commands": [],                // adb shell commands on every release
  "disable_animations": true,
  "health_interval": "2m",               // "0" off
  "users": { "alice": "sha256:..." },
  "devices": {
    "pixel-1": { "expose_port": 15601 },
    "pixel-2": { "expose_port": 15603, "cores": 2, "ram_mb": 3072 }
  }
}
```

Device fields: `profile`, `image`, `console_port` (even, 5554–5682),
`expose_port`, `ram_mb`, `cores`, `gpu`, `width`, `height`, `density`,
`avd_home`/`avd_name` (use an existing AVD), `unlock_pin`, `window`,
`extra_args`.

## Between leases

A released device leaves the pool until emuctl has dropped the old holder's
adb connections, cleaned up per `release`, run `release_commands`, turned
animations off, and passed a health check.

| `release` | Effect | Speed | Keeps APKs |
| --- | --- | --- | --- |
| `none` | nothing | instant | yes |
| `clear` | force-stop + `pm clear` each of `release_packages` | seconds | yes |
| `wipe` | reboot with `-wipe-data` | minutes | no |

Use `clear` for an app under test. Use `wipe` when sharing with untrusted
users, since `clear` only resets the listed apps.

## Health

Checks run on release and every `health_interval` on idle devices, with the
device out of the pool during the check.

- **Healthy**: adb answers and Android reports boot completed.
- **Broken** (adb port closed, or Android not booted): restart now.
- **Unknown** (timeout, adb error): keep the device; counted in
  `unverified`. 3 in a row restarts it, since a hung emulator still accepts
  connections.

Emulators that crash restart with backoff. Emulators left running by a
killed `emuctl serve` (only AVDs emuctl created) are stopped on the next
start. A second `serve` on the same `state_dir` refuses to start.

## Sizing

Overloaded hosts make emulators boot slowly and drop to `adb offline`, and
tend to fail every device, not just one. `emuctl doctor` warns when devices'
`cores` or `ram_mb` exceed the host. Measure under real test load before
adding devices.

## Metrics

`GET /metrics` (Prometheus). Key series:

| Question | Query |
| --- | --- |
| Usable devices | `sum(emuctl_device_state{state="booted"})` |
| Busy devices | `sum(emuctl_device_leased)` |
| Queue pressure | `emuctl_queue_oldest_wait_seconds`, `sum(emuctl_queue_length)` |
| Wait time p90 | `histogram_quantile(0.9, sum by (le) (rate(emuctl_lease_queue_wait_seconds_bucket[1h])))` |
| Flaky devices | `increase(emuctl_device_restarts_total[1h])`, `increase(emuctl_emulator_exits_total[1h])` |
| Boot time | `emuctl_emulator_boot_seconds` |
| Store hit rate | `rate(emuctl_apk_installs_total{result="skipped"}[1h]) / rate(emuctl_apk_installs_total[1h])` |

Alert rules: `contrib/prometheus/emuctl-alerts.yml`. Full metric list in the
top-level README.

## Files

Under `state_dir` (default `~/.local/share/emuctl`):

| Path | |
| --- | --- |
| `devices/NAME/avd/` | the AVD emuctl created |
| `devices/NAME/emulator.log` | emulator output, `emuctl logs NAME` |
| `apks/KEY.apk` | the APK store; delete old files to free space |
| `serve.lock` | held by the running `serve` |

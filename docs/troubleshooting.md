# Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `401 ... missing or invalid token` | No token, or the wrong one | Set `EMUCTL_TOKEN` or `~/.config/emuctl/token`. |
| `connection refused` on the API | Server down, wrong `EMUCTL_URL`, or the API binds loopback | Check `EMUCTL_URL`. For a loopback-only server use `EMUCTL_URL=ssh://host`. |
| Stuck at `queued at position N` | All devices are leased | `emuctl queue` and `emuctl devices` show who has them (see the `note`). Wait, or ask. |
| Lease is `active` but never `ready` | Device is booting or restarting | `emuctl devices`. If `crashed` or `starting` for minutes, `emuctl logs DEVICE`. |
| `lease lost during run` | Heartbeats stopped reaching the server (network, laptop sleep) | Rerun. Your command was interrupted. |
| `409 lease is queued, not active` | Using a lease before it was granted | Wait for `state=active` and `ready=true`. |
| `403 lease belongs to X` | Another user's lease id | Use your own lease. |
| `device X has no expose_port` | `run` needs remote adb and the device doesn't expose it | Use `emuctl shell`/MCP tools, or ask the operator to set `expose_port`. |
| `adb connect` refused / adb drops | Not the lease holder's IP, or the lease ended | Only the holder's IP can connect. Behind NAT or SSH, use `EMUCTL_URL=ssh://`. |
| `adb: device offline` | Host overloaded, or the device is restarting | `emuctl devices`. Repeated: host sizing, see `emuctl doctor`. |
| `INSTALL_FAILED_UPDATE_INCOMPATIBLE` | APK signed with a different key than the installed app | Sign every build with the same keystore, or `emuctl reset LEASE -wipe`. |
| `INSTALL_FAILED_NO_MATCHING_ABIS` | APK built for another CPU (x86_64 vs arm64) | Build for the server's ABI (`emuctl devices` → ask the operator, or check `getprop ro.product.cpu.abi`). |
| `run -apk-key` exits 3 | Key not in the store | Build, `emuctl apk put KEY FILE`, retry. See [apks.md](apks.md). |
| App shows a previous user's data | Server uses `release: none` | Clear it yourself (`emuctl shell LEASE pm clear PKG`), or ask the operator for `release: clear`. |
| App signed out after every lease | Server uses `release: clear` or `wipe` | Expected. Sign in inside your lease. |
| Taps land on the wrong element | Screen was still changing | Screenshot again before acting; prefer `uiautomator dump` bounds. |
| `emuctl serve`: `another emuctl serve is using ...` | A server is already running on that `state_dir` | Stop it, or use a different `state_dir`. |
| `emuctl serve`: `port N already in use` | Something else (not emuctl's AVD) holds the device's port | Find it with `lsof -i :N`. Change `console_port` or stop that process. |
| Device keeps restarting | Emulator crashing, or health checks failing | `emuctl logs DEVICE`; `emuctl_device_restarts_total` by `reason` in metrics. |

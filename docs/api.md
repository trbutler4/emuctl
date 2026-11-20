# HTTP API

JSON over HTTP. Send `Authorization: Bearer <token>` (not needed when the
server has no users, which is loopback-only). Errors are
`{"error": "..."}` with a meaningful status code.

## Lease lifecycle

```sh
B=http://emu-host:7600; H="Authorization: Bearer $EMUCTL_TOKEN"

# 1. Request. device "" = any. ttl_seconds optional (max max_lease_ttl).
L=$(curl -s -H "$H" -X POST $B/v1/leases -d '{"device":"","note":"ci job 42"}')
ID=$(jq -r .id <<<"$L")

# 2. Long-poll until active and ready (each call waits up to 60s for a change).
until curl -s -H "$H" "$B/v1/leases/$ID?wait=30s" | jq -e '.state=="active" and .ready' >/dev/null; do :; done

# 3. Heartbeat every ~20s while you work, or the lease expires (default 2m).
while sleep 20; do curl -s -H "$H" -X POST $B/v1/leases/$ID/renew >/dev/null; done &

# 4. Use it.
curl -s -H "$H" -X POST $B/v1/leases/$ID/shell -d '{"command":"getprop ro.product.model"}'

# 5. Release.
curl -s -H "$H" -X DELETE $B/v1/leases/$ID
```

Lease fields: `id`, `user`, `want`, `device`, `state` (`queued`, `active`,
`released`, `expired`), `position` (while queued), `expires_at`, `ready`,
`adb` (`host:port` for `adb connect`, if the device exposes adb),
`serial`.

## Endpoints

| Method | Path | Body / query | Returns |
| --- | --- | --- | --- |
| GET | `/v1/devices` | | devices: `name`, `state`, `adb`, `apk`, `holder`, `restarts`, `last_error`, `checked`, `unverified` |
| GET | `/v1/queue` | | queued leases in order |
| POST | `/v1/leases` | `{"device","note","ttl_seconds"}` | lease |
| GET | `/v1/leases/{id}` | `?wait=30s` (max 60s) | lease, after it changes or wait elapses |
| POST | `/v1/leases/{id}/renew` | | lease |
| DELETE | `/v1/leases/{id}` | | lease (also leaves the queue) |
| POST | `/v1/leases/{id}/install` | APK bytes | `{"installed","bytes","output"}` |
| POST | `/v1/leases/{id}/install?apk=KEY` | `&force=1` to reinstall | `{"installed": false}` if the device already has KEY; 404 if not stored |
| POST | `/v1/leases/{id}/reset` | `?wipe=1` | lease; device reboots, poll until `ready` |
| POST | `/v1/leases/{id}/shell` | `{"command"}` | `{"output","exit_code"}` (2 min limit, 1 MiB output) |
| GET | `/v1/leases/{id}/screenshot` | | `image/png` |
| GET | `/v1/devices/{name}/logs` | `?bytes=N` | `{"log"}` tail of the emulator log |
| POST | `/v1/devices/{name}/restart` | `?wipe=1` | restart a device not leased by someone else |
| GET | `/v1/apks` | | stored APKs: `key`, `bytes`, `last_used` |
| PUT | `/v1/apks/{key}` | APK bytes | `{"key","bytes"}`; 409 if the key exists |
| GET | `/metrics` | | Prometheus text format |

## Status codes

| Code | Meaning |
| --- | --- |
| 400 | Bad request body or key. |
| 401 | Missing or invalid token. |
| 403 | The lease belongs to another user. |
| 404 | Unknown lease, device or APK key. |
| 409 | Lease not active (e.g. still queued), or APK key already stored. |
| 413 | APK empty or over 1 GiB. |
| 422 | `adb install` failed; `error` has adb's message. |
| 502 / 503 | Device not reachable over adb right now; retry after checking `ready`. |

The Go client in `pkg/client` wraps all of this, including `Acquire`
(request plus long-poll) and `Heartbeat`.

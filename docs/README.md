# emuctl docs for agents

emuctl shares a pool of Android emulators between people and AI agents. You
lease a device, use it, and release it. One holder per device at a time;
everyone else waits in a FIFO queue.

Read in this order:

1. [quickstart.md](quickstart.md): the 90% path. Lease, use, release.
2. [apks.md](apks.md): installing your app, and the APK store that avoids
   rebuilding and reinstalling on every branch switch.
3. [mcp.md](mcp.md): the MCP tools, if you are an agent with MCP.
4. [cli.md](cli.md): every CLI command, flags and exit codes.
5. [api.md](api.md): the HTTP API, for scripts in any language.
6. [operating.md](operating.md): running the server: config, release
   modes, health checks, metrics.
7. [troubleshooting.md](troubleshooting.md): symptom → cause → fix.

## Rules that matter

- **Always release.** `emuctl run` and the MCP server release for you. With
  `acquire` or the raw API, release when done, or the device stays held
  until your heartbeat stops and the lease expires (default 2 minutes).
- **Hold one lease at a time.** Don't queue twice. If a request returns
  `queued`, keep polling that same lease.
- **Keep the lease alive.** Leases expire without a heartbeat. `run`,
  `acquire` and MCP do this. Raw API users renew every ~20s.
- **Assume nothing survives a lease.** The server may clear app data, or wipe
  the device, between leases (see operating.md). Install your APK and set up
  state inside your own lease.
- **Don't fight the device's adb.** Use the `ANDROID_SERIAL` emuctl gives
  you. Don't `adb kill-server` or set `ANDROID_ADB_SERVER_PORT`.

## Words

| Term | Meaning |
| --- | --- |
| device | One emulator, named in the server config (e.g. `pixel-1`). |
| lease | Your exclusive hold on a device. Has an `id`, a `state` and a TTL. |
| queued / active | Waiting for a device / holding one. |
| ready | The leased device has booted and is unlocked. Wait for this before using it. |
| APK key | A name for an APK in the server's store, e.g. `android-<native fingerprint>`. |

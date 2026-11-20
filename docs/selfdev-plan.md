# Self-dev: plan (proposal, not implemented)

Status: **proposal**. Nothing here exists yet. This PR is for deciding
whether to build it.

## Goal

Let an agent working on emuctl's own source build, run and verify its change
end to end against a real emulator, without touching the live emuctl that
people and CI depend on.

Inspired by jcode's self-dev mode (build, smoke-test, reload, continue). The
key difference: **self-dev never touches the live server.** It runs a
separate, disposable dev instance. That removes the hardest and riskiest
part of jcode's design for emuctl: hot-swapping a server that owns running
emulators and in-memory leases.

## Non-goals

- Zero-downtime reload of the live server. Live updates stay a normal
  deploy: install the binary, restart `emuctl serve`. Restarts are already
  tolerable (leftover emulators are reaped, clients retry). Revisit only if
  restarts prove disruptive.
- Remote triggering. Self-dev runs on the machine where the agent edits code.
  No API endpoint builds or runs code.
- Build queues across agents. Each worktree gets its own instance (below);
  Go builds are seconds, so there is nothing to dedupe yet.

## User-facing shape

```sh
emuctl selfdev up            # build this checkout, start a dev instance, print EMUCTL_URL
emuctl selfdev reload        # vet + test + build + smoke; restart the instance only if all pass
emuctl selfdev status        # running build (git hash, dirty), PID, URL, devices
emuctl selfdev logs [-f]     # dev server log
emuctl selfdev down          # stop the instance and its emulator
emuctl selfdev env           # print `export EMUCTL_URL=...` for this worktree
```

Flags on `up`/`reload`:

| Flag | Default | |
| --- | --- | --- |
| `-devices N` | 1 | Emulators in the dev instance. `0` for API/CLI-only work: starts in seconds. |
| `-skip-tests` | false | Skip `go test` (vet and smoke still run). |
| `-keep-data` | false | Keep the dev state dir across `down`/`up` (AVD, APK store). |

Agent loop (documented in `docs/selfdev.md`):

1. `emuctl selfdev up` once per session.
2. Edit code.
3. `emuctl selfdev reload`. On failure it prints the failing step and the
   previous build keeps running.
4. Verify with the normal client against the dev instance:
   `eval "$(emuctl selfdev env)"; emuctl run -- adb shell getprop`, MCP, curl.
5. Commit. `emuctl selfdev down` when done.

MCP exposure: optional `selfdev_*` tools in `emuctl mcp` only when
`EMUCTL_SELFDEV=1`, so normal agents never see them.

## Isolation

Each dev instance is keyed by its **worktree** (absolute repo root), so
parallel agents in different worktrees never collide:

| Resource | Live default | Dev instance |
| --- | --- | --- |
| State dir | `~/.local/share/emuctl` | `~/.local/share/emuctl-dev/<worktree-hash>/` |
| API | `127.0.0.1:7600` | `127.0.0.1:<7700 + slot>` |
| Console ports | 5600 + 2i | `5650 + 2*(slot*N + i)`, inside the allowed 5554–5682 range. That range only has room for 16 dev devices in total, so `up` fails clearly when it is full. |
| Expose ports | per config | none: agents use `emuctl shell` or `run` through the dev API |
| Config | `~/.config/emuctl/config.json` | generated into the dev state dir |
| Auth | per config | none (loopback only, enforced by existing validation) |

`slot` is allocated in `~/.local/share/emuctl-dev/slots.json` (file-locked),
not hashed, so two worktrees can never pick the same ports. Free slots are
reclaimed on `down` or when the recorded PID is dead.

The existing `serve.lock` per state dir already prevents two servers on one
state; the existing stale-emulator reaper only matches `emuctl-<name>` AVDs
with the instance's own console port, so a dev instance can never reap a
live emulator, and vice versa. Dev AVD names get a `dev<slot>-` prefix to
make this explicit.

## `reload` pipeline

All steps run before the running instance is touched:

1. `gofmt -l` (fail if output), `go vet ./...`.
2. `go test ./...` (unless `-skip-tests`). Race detector off by default for
   speed; `-race` flag to opt in.
3. `go build -o <devstate>/builds/<hash>[-dirty]/emuctl ./cmd/emuctl` with
   `-X main.version=<hash>`.
4. Smoke the new binary: `version`, then `serve` on a throwaway port with
   zero devices, wait for `/metrics` to return 200 and `emuctl_build_info`
   to show the new hash, then stop it.
5. Only now: stop the old dev server (SIGTERM, wait), start the new one,
   wait until every dev device is `booted` or a timeout (`boot_timeout`).
6. If step 5 fails, restart the previous build and report both errors.

Output is short and step-labelled so an agent can act on it:

```
vet      ok
test     ok (8 packages, 6.1s)
build    ok 3f2a1c9-dirty
smoke    ok
restart  ok, pixel-dev booted in 21s
EMUCTL_URL=http://127.0.0.1:7701
```

Emulator boot is the slow part (~20s on this Mac, minutes cold). Two
mitigations, both cheap:

- `-devices 0` for changes that don't need an emulator.
- Restart keeps the dev AVD (no wipe), so warm boots are the norm.

## Promotion to live

Out of scope for the tool. Documented path:

1. Merge to master; CI runs.
2. On the emulator host: install the new binary (Nix, `go install`, or copy).
3. Restart `emuctl serve` at a quiet moment (`emuctl queue` empty).

A later, optional `emuctl selfdev promote` could automate 2 and 3 with a
"wait until no active leases" gate. Not in v1.

## Implementation sketch

New package `pkg/selfdev` (~400 lines) plus `cmd` wiring:

- `instance.go`: slot allocation, paths, generated config, PID file.
- `pipeline.go`: the reload steps, each returning a short status line.
- `smoke.go`: throwaway `serve` + `/metrics` probe.
- Tests: slot allocation under concurrency, config generation (ports in
  range, no collision with live defaults), pipeline step ordering with
  fakes (a failing step must not stop the running instance).

No changes to the live server code paths except one: `serve` gains an
`-instance-label` flag that is added to `emuctl_build_info` so dev and live
metrics are distinguishable if both are scraped.

## Open questions

1. **Is this worth it?** The manual equivalent today is about five commands
   (`go build`, write a dev config, `serve -config`, export `EMUCTL_URL`,
   kill and repeat). Self-dev mostly buys safety (can't hit live by
   accident) and a consistent loop agents can follow.
2. Dev emulators share CPU with live ones on the same host. Should `up`
   refuse when `emuctl doctor` sizing would be exceeded with live + dev
   devices combined?
3. Should `reload` default to `-race`? Slower, catches more.
4. MCP `selfdev_*` tools in v1, or CLI only?
5. Console port space: live and dev share 5554–5682 (64 even ports). Dev
   starts at 5650, so a live pool past 25 devices would collide. Widen the
   allowed range (the emulator accepts up to 5682 only for adb's auto-scan;
   emuctl talks to its own adb server, so a wider range may be fine) or
   cap dev instances.

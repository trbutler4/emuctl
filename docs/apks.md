# APKs

Two ways to get your app onto a leased device.

## Ad hoc: upload this file

```sh
emuctl run -apk app.apk -- maestro test flows/
emuctl install <lease-id> app.apk
```

Uploads the APK every time. Fine for one-offs.

## Stored: install by key (use this when switching branches)

The server keeps APKs by a key you choose, and remembers which key each
device last had. Installing a key the device already has is a no-op, so
switching between branches that share a native build costs nothing.

Pick a key that changes exactly when the native app changes. For Expo/React
Native, the native fingerprint:

```sh
key=android-$(npx @expo/fingerprint fingerprint:generate --platform android | jq -r .hash)
# in the retail repo: key=android-$(node scripts/dev-client.mjs --hash)
```

Then:

```sh
emuctl run -apk-key "$key" -- maestro test flows/
```

If the server doesn't have the key, `run` exits **3** before queueing, so a
miss never holds a device. Since `run` also passes through your command's
exit code, check the store first when scripting:

```sh
if ! emuctl apk has "$key"; then                 # exit 3 = not stored
  ./gradlew assembleDebug                       # or your build
  emuctl apk put "$key" app/build/outputs/apk/debug/app-debug.apk
fi
emuctl run -apk-key "$key" -- maestro test flows/
```

Other commands:

```sh
emuctl apk list                  # keys, sizes, last used
emuctl apk has "$key"            # exit 0 stored, 3 not stored
emuctl install <lease-id> -key "$key"
```

Rules:

- **Keys are immutable.** `apk put` on an existing key succeeds without
  replacing it. If you need different bytes, use a different key. Never
  reuse a key for a different build.
- **JS-only changes don't need a new APK.** With a dev client, load JS from
  Metro. The key only changes when native code changes.
- **Every builder must sign with the same keystore**, or installing over the
  existing app fails with `INSTALL_FAILED_UPDATE_INCOMPATIBLE`.
- An ad hoc `-apk` install or a device wipe makes the device forget its key,
  so the next `-apk-key` reinstalls. That's expected.
- Keys: letters, digits, `.`, `_`, `-`; up to 128 characters.

# AGS Android Capture

A local browser bridge for Android Game View. It runs the packaged ADB and a patched scrcpy client on the browser's computer, then sends the encoded H.264 packets directly to the page over loopback WebSocket. It does not open a desktop video window or relay video through the AGS server.

## Build

Initialize the pinned sources, then run the platform build script:

```sh
git submodule update --init --recursive
./scripts/build-linux.sh
```

Linux builds need Go, Meson, Ninja, pkg-config, a C compiler, FFmpeg development libraries, and SDL3 development files. Linux artifacts use the host's FFmpeg and SDL runtime libraries; build on the target distribution so the library ABI matches. Windows builds run from an MSYS2 MINGW64 shell and are also built by `.github/workflows/build.yml`.

The build downloads pinned releases of scrcpy's matching Android server and verifies SHA-256. Linux x86-64 includes Google Platform Tools ADB; Linux ARM64 includes the runner's ADB, while the bridge prefers a system ADB when installed and falls back to its bundled ADB. Source pins and the local scrcpy patch are kept in this repository. The workflow publishes separate Linux x86-64, Linux ARM64, and Windows x86-64 artifacts.

## Linux user service

After building, the output bundle includes an installer. For a downloaded GitHub artifact, extract it and run:

```sh
./install.sh
```

When installing from a source checkout, run `./scripts/install-linux.sh` instead. The installer uses system `adb` when it is on `PATH`; otherwise it uses the bundled copy.

The user socket listens only on `127.0.0.1:17653`. systemd starts the bridge when the browser connects; the bridge exits after one minute without a stream. The per-user socket stays ready for the next request. Set the exact AGS page origins allowed to connect in `~/.config/ags-android-capture/environment`, then restart the socket if you change the listener configuration.

Check it with:

```sh
systemctl --user status ags-android-capture.socket
curl http://127.0.0.1:17653/healthz
```

## Windows

The Windows output bundle includes `install.ps1`; run it from PowerShell after extracting the GitHub artifact. From a source checkout, run `scripts/install-windows.ps1`. It installs a per-user logon task for the loopback listener.

## Browser behavior

The AGS page requests `ws://127.0.0.1:17653/v1/stream` with the selected ADB serial, bitrate, and FPS. The bridge checks the exact browser `Origin`, allows one active stream, and rejects non-loopback listeners. ADB uses a separate private local ADB server for the stream, leaving the machine's normal ADB server untouched.

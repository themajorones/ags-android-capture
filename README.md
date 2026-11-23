# AGS Android Capture

A local browser bridge for Android Game View. It runs system ADB and the AGS-patched scrcpy client on the browser's computer, then sends encoded H.264 packets directly to the page over loopback WebSocket. It does not open a desktop video window or relay video through the AGS server.

## Build

Initialize the pinned sources, then run the platform build script:

```sh
git submodule update --init --recursive
./scripts/build-linux.sh
```

Linux builds need Go, Meson, Ninja, pkg-config, a C compiler, FFmpeg development libraries, and SDL3 development files. Linux installs require the system FFmpeg runtime and SDL3 runtime. The installer checks that FFmpeg starts and that all dynamic libraries required by scrcpy resolve before it stops or replaces an existing installation. Linux artifacts use the host's FFmpeg and SDL runtime libraries; build on the target distribution so the library ABI matches. Windows builds run from an MSYS2 MINGW64 shell and are also built by `.github/workflows/build.yml`.

The build downloads the pinned scrcpy Android server and verifies its SHA-256. ADB is not bundled: install Android Platform Tools on the computer running the browser. The installer checks that system `adb` is available and records its executable path for the bridge. The helper uses the AGS-patched scrcpy client and its matching Android server; ordinary system scrcpy builds do not implement the browser stream protocol. The workflow publishes separate Linux x86-64, Linux ARM64, and Windows x86-64 artifacts.

## Linux user service

After building, the output bundle includes an installer. For a downloaded GitHub artifact, extract it and run:

```sh
./install.sh
```

When installing from a source checkout, run `./scripts/install-linux.sh` instead. The installer requires system `adb` on `PATH` and verifies it before changing the existing installation.

The user socket listens only on `127.0.0.1:17653`. systemd starts the bridge when the browser connects; the bridge exits after one minute without a stream. The per-user socket stays ready for the next request. The bridge accepts requests from any browser origin so the same helper works with AGS on different machines and hostnames.

Check it with:

```sh
systemctl --user status ags-android-capture.socket
curl http://127.0.0.1:17653/healthz
```

If the browser reports that it cannot reach the local capture service, the user socket is not listening on the computer running the browser. Re-run `./install.sh` (or `scripts/install-linux.sh` from a checkout), then check `systemctl --user status ags-android-capture.socket`. If `systemctl --user` reports that it cannot connect to the bus, log into a normal desktop session with a systemd user manager; in minimal/headless sessions, start the user manager before installing. A healthy `/healthz` response is `ok`.

## Windows

The Windows output bundle includes `install.ps1`; run it from PowerShell after extracting the GitHub artifact. From a source checkout, run `scripts/install-windows.ps1`. It installs a per-user logon task for the loopback listener.

## Browser behavior

The AGS page requests `ws://127.0.0.1:17653/v1/stream` with the selected ADB serial, bitrate, and FPS. The bridge accepts any browser `Origin`, permits concurrent streams, and rejects non-loopback listeners. Each stream gets an isolated ADB server using the system ADB executable, leaving the machine's normal ADB server untouched.

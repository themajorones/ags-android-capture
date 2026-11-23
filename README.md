# AGS Android Capture

Streams Android Game View directly from the browser computer using system ADB and the AGS capture bridge.

## Install

Download and extract the matching platform bundle, then run `install.sh` on Linux or `install.ps1` on Windows. System ADB must be installed and available on `PATH`.

## Build

```sh
git submodule update --init --recursive
./scripts/build-linux.sh
```

Windows builds use MSYS2 MINGW64 and `scripts/build-windows.sh`. Build artifacts are published for Linux x86-64, Linux ARM64, and Windows x86-64.

## Stream

The bridge listens on `127.0.0.1:17653`. AGS connects to `ws://127.0.0.1:17653/v1/stream`; health is available at `http://127.0.0.1:17653/healthz`.

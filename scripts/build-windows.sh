#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRCPY_VERSION="5.0.1"
ADB_VERSION="37.0.3"
SCRCPY_SERVER_SHA256="764eb6f79811d5211fe9df341120882ba9994c7a61b897d7bf3fb662e53bc536"
ADB_SHA256="89d108360a3bf2924d3bea837239edaccc22fd6ff5befd3787c4348a9fe4f09d"
BUILD="$ROOT/.build"
ASSETS="$BUILD/assets"
TOOLS="$BUILD/platform-tools-win"
OUT="$ROOT/dist/windows-x86_64"
MESON_BUILD="$BUILD/scrcpy-windows"

for command in git go meson ninja pkg-config curl sha256sum unzip; do
  command -v "$command" >/dev/null || { echo "Missing required command: $command" >&2; exit 1; }
done

mkdir -p "$ASSETS"
git -C "$ROOT" submodule update --init --recursive

patch="$ROOT/patches/scrcpy-browser-stream.patch"
if ! git -C "$ROOT/third_party/scrcpy" apply --reverse --check "$patch" >/dev/null 2>&1; then
  git -C "$ROOT/third_party/scrcpy" apply --check "$patch"
  git -C "$ROOT/third_party/scrcpy" apply "$patch"
fi

server="$ASSETS/scrcpy-server-v$SCRCPY_VERSION"
if [[ ! -f "$server" ]]; then
  curl --fail --location --silent --show-error \
    "https://github.com/Genymobile/scrcpy/releases/download/v$SCRCPY_VERSION/scrcpy-server-v$SCRCPY_VERSION" \
    --output "$server"
fi
echo "$SCRCPY_SERVER_SHA256  $server" | sha256sum --check --status || { echo "scrcpy server checksum mismatch" >&2; exit 1; }

adb_zip="$ASSETS/platform-tools_r$ADB_VERSION-win.zip"
if [[ ! -f "$adb_zip" ]]; then
  curl --fail --location --silent --show-error \
    "https://dl.google.com/android/repository/platform-tools_r$ADB_VERSION-win.zip" \
    --output "$adb_zip"
fi
echo "$ADB_SHA256  $adb_zip" | sha256sum --check --status || { echo "Android Platform Tools checksum mismatch" >&2; exit 1; }

rm -rf "$TOOLS" "$MESON_BUILD" "$OUT"
mkdir -p "$TOOLS" "$OUT"
unzip -q "$adb_zip" -d "$TOOLS"

pkg-config --exists 'libavformat >= 60.3' 'libavcodec >= 60.3' 'libavutil >= 58.2' libswresample 'sdl3 >= 3.2.0' || {
  echo "Install the scrcpy MSYS2 MINGW64 FFmpeg and SDL3 packages before building." >&2
  exit 1
}

meson setup "$MESON_BUILD" "$ROOT/third_party/scrcpy" \
  --buildtype=release --strip \
  -Dcompile_server=false \
  -Dprebuilt_server="$server" \
  -Dportable=true -Dstatic=false -Dusb=false -Dv4l2=false -Dvaapi=false \
  -Dd3d11va=false -Dvideotoolbox=false
ninja -C "$MESON_BUILD"

go build -trimpath -ldflags='-s -w' -o "$OUT/ags-android-capture.exe" "$ROOT/cmd/ags-capture"
cp "$TOOLS/platform-tools/adb.exe" "$OUT/adb.exe"
cp "$TOOLS/platform-tools/AdbWinApi.dll" "$TOOLS/platform-tools/AdbWinUsbApi.dll" "$OUT/"
cp "$MESON_BUILD/app/scrcpy.exe" "$OUT/scrcpy.exe"
cp "$server" "$OUT/scrcpy-server"

# MSYS2 runtime DLLs are required beside scrcpy.exe on a clean Windows install.
ldd "$OUT/scrcpy.exe" | awk '$3 ~ /^\/mingw64\/bin\/.*\.dll$/ { print $3 }' | sort -u | while read -r dll; do
  cp -n "$dll" "$OUT/"
done

"$OUT/ags-android-capture.exe" 2>&1 | grep -q '^usage:' || true
cp "$ROOT/scripts/install-windows.ps1" "$OUT/install.ps1"
cp "$ROOT/README.md" "$OUT/README.md"
echo "Windows bundle built at $OUT"

#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRCPY_VERSION="5.0.1"
SCRCPY_SERVER_SHA256="764eb6f79811d5211fe9df341120882ba9994c7a61b897d7bf3fb662e53bc536"
BUILD="$ROOT/.build"
ASSETS="$BUILD/assets"
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64) PACKAGE_ARCH=x86_64 ;;
  aarch64|arm64) PACKAGE_ARCH=arm64 ;;
  *) echo "Unsupported Linux architecture: $HOST_ARCH" >&2; exit 1 ;;
esac
OUT="$ROOT/dist/linux-$PACKAGE_ARCH"
MESON_BUILD="$BUILD/scrcpy-linux-$PACKAGE_ARCH"

for command in git go meson ninja pkg-config curl sha256sum readelf ldd; do
  command -v "$command" >/dev/null || { echo "Missing required command: $command" >&2; exit 1; }
done

mkdir -p "$ASSETS"
git -C "$ROOT" submodule update --init --recursive

apply_scrcpy_patch() {
  local patch="$ROOT/patches/scrcpy-browser-stream.patch"
  if git -C "$ROOT/third_party/scrcpy" apply --reverse --check "$patch" >/dev/null 2>&1; then
    return
  fi
  git -C "$ROOT/third_party/scrcpy" apply --check "$patch"
  git -C "$ROOT/third_party/scrcpy" apply "$patch"
}

apply_scrcpy_patch

server="$ASSETS/scrcpy-server-v$SCRCPY_VERSION"
if [[ ! -f "$server" ]]; then
  curl --fail --location --silent --show-error \
    "https://github.com/Genymobile/scrcpy/releases/download/v$SCRCPY_VERSION/scrcpy-server-v$SCRCPY_VERSION" \
    --output "$server"
fi
echo "$SCRCPY_SERVER_SHA256  $server" | sha256sum --check --status || {
  echo "scrcpy server checksum mismatch" >&2
  exit 1
}

rm -rf "$MESON_BUILD" "$OUT"
mkdir -p "$OUT/lib"

pkg-config --exists 'libavformat >= 60.3' 'libavcodec >= 60.3' 'libavutil >= 58.2' libswresample 'sdl3 >= 3.2.0' || {
  echo "Install FFmpeg development libraries and SDL3 development files before building." >&2
  exit 1
}

meson setup "$MESON_BUILD" "$ROOT/third_party/scrcpy" \
  --buildtype=release --strip \
  -Dcompile_server=false \
  -Dprebuilt_server="$server" \
  -Dportable=true -Dstatic=false -Dusb=false -Dv4l2=false -Dvaapi=false \
  -Dd3d11va=false -Dvideotoolbox=false
ninja -C "$MESON_BUILD"

go build -trimpath -ldflags='-s -w' -o "$OUT/ags-android-capture" "$ROOT/cmd/ags-capture"
install -m 755 "$MESON_BUILD/app/scrcpy" "$OUT/scrcpy"
install -m 644 "$server" "$OUT/scrcpy-server"

"$MESON_BUILD/app/scrcpy" --version >/dev/null

"$OUT/scrcpy" --version >/dev/null
install -m 755 "$ROOT/scripts/install-linux.sh" "$OUT/install.sh"
install -m 644 "$ROOT/README.md" "$OUT/README.md"
mkdir -p "$OUT/systemd"
install -m 644 "$ROOT/systemd/ags-android-capture.socket" "$ROOT/systemd/ags-android-capture.service" "$OUT/systemd/"
echo "Linux bundle built at $OUT"

#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRCPY_VERSION="5.0.1"
ADB_VERSION="37.0.3"
SCRCPY_SERVER_SHA256="764eb6f79811d5211fe9df341120882ba9994c7a61b897d7bf3fb662e53bc536"
ADB_SHA256="214548146cadb2ca3508143d8a1d4be4c59de5bb837253e3ede89ed74cf450d1"
BUILD="$ROOT/.build"
ASSETS="$BUILD/assets"
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in
  x86_64) PACKAGE_ARCH=x86_64; ADB_BUNDLE=google ;;
  aarch64|arm64) PACKAGE_ARCH=arm64; ADB_BUNDLE=system ;;
  *) echo "Unsupported Linux architecture: $HOST_ARCH" >&2; exit 1 ;;
esac
TOOLS="$BUILD/platform-tools-linux-$PACKAGE_ARCH"
OUT="$ROOT/dist/linux-$PACKAGE_ARCH"
MESON_BUILD="$BUILD/scrcpy-linux-$PACKAGE_ARCH"

for command in git go meson ninja pkg-config curl sha256sum unzip readelf ldd; do
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

rm -rf "$TOOLS" "$MESON_BUILD" "$OUT"
mkdir -p "$TOOLS" "$OUT/lib"
if [[ "$ADB_BUNDLE" == google ]]; then
  adb_zip="$ASSETS/platform-tools_r$ADB_VERSION-linux.zip"
  if [[ ! -f "$adb_zip" ]]; then
    curl --fail --location --silent --show-error \
      "https://dl.google.com/android/repository/platform-tools_r$ADB_VERSION-linux.zip" --output "$adb_zip"
  fi
  echo "$ADB_SHA256  $adb_zip" | sha256sum --check --status || { echo "Android Platform Tools checksum mismatch" >&2; exit 1; }
  unzip -q "$adb_zip" -d "$TOOLS"
  ADB_SOURCE="$TOOLS/platform-tools/adb"
else
  ADB_SOURCE="$(command -v adb || true)"
  [[ -n "$ADB_SOURCE" ]] || { echo "Install system adb before building the ARM64 bundle." >&2; exit 1; }
fi

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
install -m 755 "$ADB_SOURCE" "$OUT/adb"
install -m 755 "$MESON_BUILD/app/scrcpy" "$OUT/scrcpy"
install -m 644 "$server" "$OUT/scrcpy-server"
if [[ -d "$TOOLS/platform-tools/lib64" ]]; then
  cp -a "$TOOLS/platform-tools/lib64" "$OUT/"
fi

# Bundle scrcpy's non-core shared libraries recursively. The host provides
# only the Linux loader and glibc itself.
scrcpy_libdir="$(pkg-config --variable=libdir libavcodec)"
build_ld_path="$scrcpy_libdir${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
LD_LIBRARY_PATH="$build_ld_path" ldd "$OUT/scrcpy" | awk '$2 == "=>" && $3 ~ /^\// { print $3 }' | sort -u | while read -r source; do
  [[ -n "$source" ]] || continue
  soname="$(readelf -d "$source" | sed -n 's/.*(SONAME).*\[\(.*\)\].*/\1/p' | head -1)"
  [[ -n "$soname" ]] || soname="$(basename "$source")"
  case "$soname" in
    libc.so.*|libm.so.*|libpthread.so.*|libdl.so.*|librt.so.*|ld-linux*.so.*|libresolv.so.*|libutil.so.*|libnss_*.so.*) continue ;;
  esac
  cp -L "$source" "$OUT/lib/$soname"
done

unresolved="$(LD_LIBRARY_PATH="$OUT/lib:$build_ld_path" ldd "$OUT/scrcpy" | grep 'not found' || true)"
[[ -z "$unresolved" ]] || { echo "Unbundled scrcpy libraries remain:" >&2; echo "$unresolved" >&2; exit 1; }

"$OUT/adb" version >/dev/null
LD_LIBRARY_PATH="$OUT/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}" "$OUT/scrcpy" --version >/dev/null
install -m 755 "$ROOT/scripts/install-linux.sh" "$OUT/install.sh"
install -m 644 "$ROOT/README.md" "$OUT/README.md"
mkdir -p "$OUT/systemd"
install -m 644 "$ROOT/systemd/ags-android-capture.socket" "$ROOT/systemd/ags-android-capture.service" "$OUT/systemd/"
echo "Linux bundle built at $OUT"

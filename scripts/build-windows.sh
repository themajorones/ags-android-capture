#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRCPY_VERSION="5.0.1"
SCRCPY_SERVER_SHA256="764eb6f79811d5211fe9df341120882ba9994c7a61b897d7bf3fb662e53bc536"
BUILD="$ROOT/.build"
ASSETS="$BUILD/assets"
OUT="$ROOT/dist/windows-x86_64"
MESON_BUILD="$BUILD/scrcpy-windows"

for command in git go meson ninja pkg-config curl sha256sum ldd; do
  command -v "$command" >/dev/null || { echo "Missing required command: $command" >&2; exit 1; }
done

mkdir -p "$ASSETS"
git -C "$ROOT" submodule update --init --recursive
(cd "$ROOT/third_party/scrcpy" && git ls-files -z -- '*.c' '*.h' '*.build' | xargs -0 -r sed -i 's/\r$//')

patch="$BUILD/scrcpy-browser-stream.patch"
tr -d '\r' < "$ROOT/patches/scrcpy-browser-stream.patch" > "$patch"
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

rm -rf "$MESON_BUILD" "$OUT"
mkdir -p "$OUT"

pkg-config --exists 'libavformat >= 60.3' 'libavcodec >= 60.3' 'libavutil >= 58.2' libswresample 'sdl3 >= 3.2.0' || {
  echo "Install the scrcpy MSYS2 UCRT64 or MINGW64 FFmpeg and SDL3 packages before building." >&2
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
cp "$MESON_BUILD/app/scrcpy.exe" "$OUT/scrcpy.exe"
cp "$server" "$OUT/scrcpy-server"

# MSYS2 runtime DLLs are required beside scrcpy.exe on a clean Windows install.
# The CI toolchain is UCRT64; also accept MINGW64 for local builds. The previous
# filter only matched MINGW64, so CI silently produced bundles missing every
# FFmpeg/SDL runtime DLL and scrcpy exited with STATUS_DLL_NOT_FOUND.
ldd_output="$(ldd "$OUT/scrcpy.exe")"
if grep -Eqi 'not found' <<< "$ldd_output"; then
  printf '%s\n' "$ldd_output" >&2
  echo "scrcpy has unresolved Windows runtime dependencies." >&2
  exit 1
fi
runtime_dlls="$(awk '$2 == "=>" && $3 ~ /\/(ucrt64|mingw64)\/bin\/[^[:space:]]+\.dll$/ { print $3 }' <<< "$ldd_output" | sort -u)"
if [[ -z "$runtime_dlls" ]]; then
  printf '%s\n' "$ldd_output" >&2
  echo "Could not locate scrcpy runtime DLLs under the active MSYS2 environment." >&2
  exit 1
fi
while IFS= read -r dll; do
  [[ -n "$dll" ]] && cp -n "$dll" "$OUT/"
done <<< "$runtime_dlls"

for pattern in SDL3.dll avcodec-*.dll avformat-*.dll avutil-*.dll; do
  compgen -G "$OUT/$pattern" >/dev/null || {
    echo "The Windows bundle is missing required scrcpy runtime dependency: $pattern" >&2
    exit 1
  }
done

"$OUT/ags-android-capture.exe" 2>&1 | grep -q '^usage:' || true
cp "$ROOT/scripts/install-windows.ps1" "$OUT/install.ps1"
cp "$ROOT/README.md" "$OUT/README.md"
echo "Windows bundle built at $OUT"

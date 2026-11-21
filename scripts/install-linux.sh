#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -x "$SCRIPT_DIR/ags-android-capture" ]]; then
  SOURCE="$SCRIPT_DIR"
  UNIT_SOURCE="$SCRIPT_DIR/systemd"
else
  ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
  case "$(uname -m)" in
    x86_64) PACKAGE_ARCH=x86_64 ;;
    aarch64|arm64) PACKAGE_ARCH=arm64 ;;
    *) echo "Unsupported Linux architecture: $(uname -m)" >&2; exit 1 ;;
  esac
  SOURCE="$ROOT/dist/linux-$PACKAGE_ARCH"
  UNIT_SOURCE="$ROOT/systemd"
fi
DEST="$HOME/.local/lib/ags-android-capture"
UNIT_DIR="$HOME/.config/systemd/user"

[[ -x "$SOURCE/ags-android-capture" && -x "$SOURCE/adb" && -x "$SOURCE/scrcpy" ]] || {
  echo "Build the Linux bundle first with scripts/build-linux.sh" >&2
  exit 1
}

# Linux scrcpy is linked against the target machine's FFmpeg and SDL runtime.
# Check the bundle before stopping/replacing a working installation so an
# incompatible or incomplete artifact cannot leave the user without a bridge.
if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "Missing runtime requirement: FFmpeg. Install FFmpeg (including its runtime libraries), then run this installer again." >&2
  exit 1
fi
if ! ffmpeg -version >/dev/null 2>&1; then
  echo "FFmpeg is installed but could not start. Repair the FFmpeg runtime before installing the capture bridge." >&2
  exit 1
fi
if ! command -v ldd >/dev/null 2>&1; then
  echo "Missing installer requirement: ldd (provided by the system C runtime tools). Cannot verify scrcpy runtime libraries." >&2
  exit 1
fi
SCRCPY_DEPS="$(ldd "$SOURCE/scrcpy" 2>&1)" || {
  echo "Could not inspect the scrcpy runtime dependencies:" >&2
  echo "$SCRCPY_DEPS" >&2
  exit 1
}
if grep -q 'not found' <<<"$SCRCPY_DEPS"; then
  echo "The scrcpy bundle is missing runtime libraries required by this system:" >&2
  grep 'not found' <<<"$SCRCPY_DEPS" >&2
  echo "Install the matching FFmpeg and SDL3 runtime packages, or rebuild the bundle on this Linux distribution." >&2
  exit 1
fi

mkdir -p "$DEST" "$UNIT_DIR" "$HOME/.config/ags-android-capture" "$HOME/.android"
systemctl --user stop ags-android-capture.socket ags-android-capture.service >/dev/null 2>&1 || true
if [[ -d "$DEST/lib" ]]; then find "$DEST/lib" -mindepth 1 -maxdepth 1 -type f -delete; fi
cp -a "$SOURCE/." "$DEST/"
install -m 644 "$UNIT_SOURCE/ags-android-capture.socket" "$UNIT_DIR/ags-android-capture.socket"
install -m 644 "$UNIT_SOURCE/ags-android-capture.service" "$UNIT_DIR/ags-android-capture.service"
if [[ ! -f "$HOME/.config/ags-android-capture/environment" ]]; then
  cat > "$HOME/.config/ags-android-capture/environment" <<'EOF'
# Comma-separated exact origins of AGS web frontends allowed to request a local stream.
AGS_CAPTURE_ALLOWED_ORIGINS=http://localhost:7530,http://127.0.0.1:7530,http://bigscreen:7530,http://devnode:7530
EOF
fi

systemctl --user daemon-reload
if ! systemctl --user enable --now ags-android-capture.socket; then
  cat >&2 <<'EOF'
Could not start the AGS Android capture user socket.
The browser error "Could not reach the local Android capture service" means
127.0.0.1:17653 is not listening. Check that this Linux session has a working
systemd user manager with: systemctl --user status
EOF
  exit 1
fi
if ! systemctl --user is-active --quiet ags-android-capture.socket; then
  systemctl --user status --no-pager ags-android-capture.socket >&2 || true
  echo "AGS Android capture socket did not become active." >&2
  exit 1
fi
echo "Installed. The bridge process starts on a browser request; systemd keeps only its loopback socket listening."
echo "Allowed web origins are in $HOME/.config/ags-android-capture/environment"

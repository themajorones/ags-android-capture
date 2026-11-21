#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -x "$SCRIPT_DIR/ags-android-capture" ]]; then
  SOURCE="$SCRIPT_DIR"
  UNIT_SOURCE="$SCRIPT_DIR/systemd"
else
  ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
  SOURCE="$ROOT/dist/linux-x86_64"
  UNIT_SOURCE="$ROOT/systemd"
fi
DEST="$HOME/.local/lib/ags-android-capture"
UNIT_DIR="$HOME/.config/systemd/user"

[[ -x "$SOURCE/ags-android-capture" && -x "$SOURCE/adb" && -x "$SOURCE/scrcpy" ]] || {
  echo "Build the Linux bundle first with scripts/build-linux.sh" >&2
  exit 1
}

mkdir -p "$DEST" "$UNIT_DIR" "$HOME/.config/ags-android-capture" "$HOME/.android"
systemctl --user stop ags-android-capture.socket ags-android-capture.service >/dev/null 2>&1 || true
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
systemctl --user enable --now ags-android-capture.socket
echo "Installed. The bridge process starts on a browser request; systemd keeps only its loopback socket listening."
echo "Allowed web origins are in $HOME/.config/ags-android-capture/environment"

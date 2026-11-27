#!/bin/sh
# Installs the latest wake release binary for this machine.
#   curl -fsSL https://raw.githubusercontent.com/SupermodularAI/agents-wake/main/install.sh | sh
# Set WAKE_INSTALL_DIR to change the target (default: $HOME/.local/bin). Never uses sudo.
set -eu

base="https://github.com/SupermodularAI/agents-wake/releases/latest/download"

die() {
  echo "wake install: $1" >&2
  exit 1
}

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "unsupported OS: $(uname -s)" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=x64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac
asset="wake-$os-$arch"

if command -v sha256sum > /dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum > /dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
  die "need sha256sum or shasum to verify the download"
fi

if [ -n "${WAKE_INSTALL_DIR:-}" ]; then
  dir="$WAKE_INSTALL_DIR"
elif [ -n "${HOME:-}" ]; then
  dir="$HOME/.local/bin"
else
  die "HOME is not set; set WAKE_INSTALL_DIR"
fi

tmp=$(mktemp -d) || die "cannot create a temporary directory"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM

curl -fsSL -o "$tmp/$asset" "$base/$asset" || die "download of $asset failed"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "download of checksums.txt failed"

want=$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1; exit }' "$tmp/checksums.txt")
[ -n "$want" ] || die "no checksum listed for $asset"
got=$(sha256 "$tmp/$asset")
[ "$want" = "$got" ] || die "checksum mismatch for $asset; nothing installed"

mkdir -p "$dir" || die "cannot create $dir"
chmod 0755 "$tmp/$asset"
# Same directory as the target, so the final mv is an atomic rename.
cp "$tmp/$asset" "$dir/.wake.$$" || die "cannot write to $dir"
chmod 0755 "$dir/.wake.$$"
mv -f "$dir/.wake.$$" "$dir/wake" || {
  rm -f "$dir/.wake.$$"
  die "cannot install to $dir/wake"
}

echo "Installed wake to $dir/wake"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add it to your PATH: export PATH=\"$dir:\$PATH\"" ;;
esac
echo "Next: wake install"

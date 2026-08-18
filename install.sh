#!/bin/sh

set -eu

repo="SupermodularAI/agents-wake"
install_dir="${WAKE_INSTALL_DIR:-$HOME/.local/bin}"

fail() {
  printf '%s\n' "wake install: $*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

latest_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") || fail "could not resolve the latest release"
version=${latest_url##*/}
case "$version" in
  v*) ;;
  *) fail "could not determine the latest release version" ;;
esac

archive="wake_${version#v}_${os}_${arch}.tar.gz"
download="https://github.com/$repo/releases/download/$version"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/wake.XXXXXX") || fail "could not create a temporary directory"
trap 'rm -rf "$tmp_dir"' 0 1 2 3 15

curl -fsSL -o "$tmp_dir/$archive" "$download/$archive" || fail "could not download $archive"
curl -fsSL -o "$tmp_dir/checksums.txt" "$download/checksums.txt" || fail "could not download checksums"

checksum=$(grep " $archive\$" "$tmp_dir/checksums.txt" || true)
[ -n "$checksum" ] || fail "checksum for $archive is missing"

if command -v shasum >/dev/null 2>&1; then
  printf '%s\n' "$checksum" | (cd "$tmp_dir" && shasum -a 256 --check -) || fail "checksum verification failed"
elif command -v sha256sum >/dev/null 2>&1; then
  printf '%s\n' "$checksum" | (cd "$tmp_dir" && sha256sum --check -) || fail "checksum verification failed"
else
  fail "shasum or sha256sum is required to verify the download"
fi

tar -xzf "$tmp_dir/$archive" -C "$tmp_dir" wake || fail "could not unpack $archive"
mkdir -p "$install_dir" || fail "could not create $install_dir"
install -m 0755 "$tmp_dir/wake" "$install_dir/wake" || fail "could not install wake"

printf '%s\n' "wake $version installed to $install_dir/wake"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf '%s\n' "add $install_dir to PATH to run wake without its full path" ;;
esac

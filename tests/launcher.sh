#!/bin/sh
# Tests for package/bin/wake: picks the binary by uname, never fails the hook.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/path" "$tmp/pkg"
cp "$here/package/bin/wake" "$tmp/pkg/wake"
chmod +x "$tmp/pkg/wake"

# A fake uname that answers from $FAKE_S / $FAKE_M.
cat > "$tmp/path/uname" <<'STUB'
#!/bin/sh
case "$1" in
  -s) echo "$FAKE_S" ;;
  -m) echo "$FAKE_M" ;;
esac
STUB
chmod +x "$tmp/path/uname"

fail() { echo "FAIL: $1" >&2; exit 1; }
run() { PATH="$tmp/path:$PATH" FAKE_S="$1" FAKE_M="$2" "$tmp/pkg/wake" emit stop < /dev/null; }

# Unknown platform: exit 0, no output.
out=$(run Plan9 x) || fail "unknown platform exited non-zero"
[ -z "$out" ] || fail "unknown platform printed: $out"

# Known platform, binary missing: exit 0, no output.
out=$(run Darwin arm64) || fail "missing binary exited non-zero"
[ -z "$out" ] || fail "missing binary printed: $out"

# Known platform, binary present: it runs and gets the arguments.
printf '#!/bin/sh\necho ok "$@"\n' > "$tmp/pkg/wake-darwin-arm64"
chmod +x "$tmp/pkg/wake-darwin-arm64"
out=$(run Darwin arm64) || fail "darwin arm64 exited non-zero"
[ "$out" = "ok emit stop" ] || fail "darwin arm64 printed: $out"

# Name mapping: aarch64 -> arm64, amd64 -> x64, Linux -> linux.
cp "$tmp/pkg/wake-darwin-arm64" "$tmp/pkg/wake-linux-arm64"
out=$(run Linux aarch64) || fail "linux aarch64 exited non-zero"
[ "$out" = "ok emit stop" ] || fail "linux aarch64 printed: $out"
cp "$tmp/pkg/wake-darwin-arm64" "$tmp/pkg/wake-linux-x64"
out=$(run Linux amd64) || fail "linux amd64 exited non-zero"
[ "$out" = "ok emit stop" ] || fail "linux amd64 printed: $out"
cp "$tmp/pkg/wake-darwin-arm64" "$tmp/pkg/wake-darwin-x64"
out=$(run Darwin x86_64) || fail "darwin x86_64 exited non-zero"
[ "$out" = "ok emit stop" ] || fail "darwin x86_64 printed: $out"

# A non-executable binary is skipped quietly.
chmod -x "$tmp/pkg/wake-linux-x64"
out=$(run Linux x86_64) || fail "non-executable exited non-zero"
[ -z "$out" ] || fail "non-executable printed: $out"

# Stdin is drained when nothing runs, so the caller never sees a broken pipe.
yes | head -c 1000000 | PATH="$tmp/path:$PATH" FAKE_S=Plan9 FAKE_M=x "$tmp/pkg/wake" emit stop || fail "stdin not drained"

echo "launcher: ok"

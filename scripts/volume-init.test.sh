#!/bin/sh
# Tests of scripts/volume-init.sh. Needs root (it changes owners): make
# test-scripts runs it as root in the pinned Alpine image, on a temporary
# folder. Production grants only CHOWN and DAC_READ_SEARCH; the test also
# gets DAC_OVERRIDE and FOWNER, needed only by the test itself to build and
# remove its trees and fake commands, not by the script under test. As
# another user it is skipped.
set -eu

if [ "$(id -u)" != 0 ]; then
	echo "skip volume-init.test.sh (needs root; make test-scripts runs it as root)"
	exit 0
fi
script=$(pwd)/scripts/volume-init.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0
pass() { printf 'ok   %s\n' "$*"; }
fail() {
	printf 'FAIL %s\n' "$*" >&2
	failures=$((failures + 1))
}
owners_left() { find "$1" \( ! -user "$2" -o ! -group "$2" \) | wc -l | tr -d ' '; }
tree() {
	rm -rf "$1"
	mkdir -p "$1/a/b" "$1/c"
	for f in a/1 a/b/2 a/b/3 c/4 c/5 6; do echo x >"$1/$f"; done
	chown -R 0:0 "$1"
}

# 1. A root-owned volume is converted entirely, root last.
tree "$tmp/v1"
if sh "$script" "$tmp/v1" 70 >/dev/null && [ "$(owners_left "$tmp/v1" 70)" = 0 ]; then
	pass "converts a root-owned volume"
else fail "root-owned volume not converted"; fi

# 2. Idempotent: on a converted volume nothing is changed (a chown that
#    would fail is never called).
#    The fake chown leaves a marker file: the script hides chown's output.
mkdir -p "$tmp/bin"
printf '#!/bin/sh\ntouch "%s"\nexit 1\n' "$tmp/chown-called" >"$tmp/bin/chown"
chmod +x "$tmp/bin/chown"
if PATH="$tmp/bin:$PATH" sh "$script" "$tmp/v1" 70 2>"$tmp/err"; then
	if [ -e "$tmp/chown-called" ]; then fail "changed a converted volume"; else pass "changes nothing on a converted volume"; fi
else fail "failed on a converted volume: $(cat "$tmp/err")"; fi
# The same fake chown on an unconverted volume: the marker appears (the
# check above can fail) and the step fails.
tree "$tmp/v0"
if PATH="$tmp/bin:$PATH" sh "$script" "$tmp/v0" 70 >/dev/null 2>&1; then
	fail "a chown that fails did not fail the step"
elif [ -e "$tmp/chown-called" ]; then pass "a failing chown fails the step (and the marker check works)"
else fail "the marker was not written: the idempotence check cannot fail"; fi
rm -f "$tmp/chown-called"
# A missing volume directory fails.
if sh "$script" "$tmp/missing" 70 >/dev/null 2>&1; then fail "a missing directory passed"; else pass "a missing directory fails"; fi

# 3. A file vanishing during the walk (WAL recycling) is not an error.
tree "$tmp/v2"
cat >"$tmp/bin/chown" <<'SH'
#!/bin/sh
# Removes one file before changing owners: the real chown then fails on it.
rm -f "$VANISH"
exec /bin/chown "$@"
SH
chmod +x "$tmp/bin/chown"
if VANISH=$tmp/v2/a/b/2 PATH="$tmp/bin:$PATH" sh "$script" "$tmp/v2" 10001 >/dev/null 2>"$tmp/err" &&
	[ "$(owners_left "$tmp/v2" 10001)" = 0 ]; then
	pass "tolerates a file that vanished during the walk"
else fail "vanished file failed the step: $(cat "$tmp/err")"; fi

# 4. A file that really kept another owner fails the step, and the root is
#    left unchanged so the next run walks the volume again.
tree "$tmp/v3"
cat >"$tmp/bin/chown" <<'SH'
#!/bin/sh
# Silently skips one file (as if its owner could not be changed).
args=
for a in "$@"; do [ "$a" = "$SKIP" ] || args="$args $a"; done
# shellcheck disable=SC2086
exec /bin/chown $args
SH
chmod +x "$tmp/bin/chown"
if SKIP=$tmp/v3/c/4 PATH="$tmp/bin:$PATH" sh "$script" "$tmp/v3" 70 >/dev/null 2>"$tmp/err"; then
	fail "a file left with another owner did not fail the step"
else
	if [ "$(stat -c %u "$tmp/v3")" = 0 ]; then pass "fails and keeps the root unconverted"; else fail "root converted although a file was left"; fi
fi

# 5. A volume whose root already has the right owner (Docker copies the
#    image's mount point owner into a new volume) but whose files do not (an
#    older root-running stack) is still converted.
tree "$tmp/v6"
chown 70:70 "$tmp/v6"
if sh "$script" "$tmp/v6" 70 >/dev/null && [ "$(owners_left "$tmp/v6" 70)" = 0 ]; then
	pass "converts the files under a root that already has the right owner"
else fail "files under a correctly owned root left unconverted"; fi

# 6. Several volumes in one call; usage error on an odd argument count.
tree "$tmp/v4"
tree "$tmp/v5"
if sh "$script" "$tmp/v4" 70 "$tmp/v5" 10001 >/dev/null && [ "$(owners_left "$tmp/v4" 70)" = 0 ] && [ "$(owners_left "$tmp/v5" 10001)" = 0 ]; then
	pass "several volumes"
else fail "several volumes"; fi
if sh "$script" "$tmp/v4" 2>/dev/null; then fail "odd argument count accepted"; else pass "usage error"; fi

if [ "$failures" -gt 0 ]; then
	echo "volume-init.test.sh: $failures failure(s)" >&2
	exit 1
fi
echo "volume-init.test.sh: all checks passed"

#!/bin/sh
# Tests of scripts/init-env.sh and scripts/check-env.sh (make test-scripts;
# also runs under any POSIX sh from the repository root). Works in a
# temporary folder; never touches .env.
set -eu

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0
pass() { printf 'ok   %s\n' "$*"; }
fail() {
	printf 'FAIL %s\n' "$*" >&2
	failures=$((failures + 1))
}
secret_keys="POSTGRES_PASSWORD DB_WORKER_PASSWORD DB_API_PASSWORD MINIO_ROOT_PASSWORD KEYCLOAK_ADMIN_PASSWORD"
value() { sed -n "s/^$1=//p" "$2" | tail -n 1; }

# 1. A new file: every placeholder replaced, mode 0600, URL-safe distinct values.
out=$(sh scripts/init-env.sh "$tmp/env" .env.example)
[ -f "$tmp/env" ] && pass "creates the file" || fail "file not created"
mode=$(stat -c %a "$tmp/env")
[ "$mode" = 600 ] && pass "mode 0600" || fail "mode is $mode, want 600"
if grep -q '^[A-Za-z_][A-Za-z0-9_]*=.*change-me' "$tmp/env"; then fail "a placeholder is left"; else pass "no placeholder left"; fi
seen=
for key in $secret_keys; do
	v=$(value "$key" "$tmp/env")
	case $v in
	"" | *[!A-Za-z0-9_-]*) fail "$key is empty or not URL-safe" ;;
	*)
		if [ "${#v}" -ge 40 ]; then pass "$key is URL-safe, ${#v} characters"; else fail "$key has only ${#v} characters"; fi
		;;
	esac
	case " $seen " in
	*" $v "*) fail "$key repeats another secret" ;;
	esac
	seen="$seen $v"
done
case $out in
*KEYCLOAK_ADMIN_PASSWORD*) pass "tells where the Keycloak admin password is" ;;
*) fail "message does not point to the Keycloak admin password: $out" ;;
esac
for key in $secret_keys; do
	v=$(value "$key" "$tmp/env")
	case $out in
	*"$v"*) fail "the message prints the value of $key" ;;
	esac
done
# Non-secret lines are copied unchanged.
for key in POSTGRES_USER POSTGRES_DB MINIO_ROOT_USER S3_BUCKET KEYCLOAK_ADMIN; do
	[ "$(value "$key" "$tmp/env")" = "$(value "$key" .env.example)" ] && pass "$key copied" || fail "$key changed"
done

# 2. Idempotent: never overwrites.
before=$(cksum <"$tmp/env")
sh scripts/init-env.sh "$tmp/env" .env.example >/dev/null
[ "$(cksum <"$tmp/env")" = "$before" ] && pass "second run changes nothing" || fail "second run changed the file"

# 3. --add-missing: appends only the missing secrets of an older file.
grep -v '^DB_' "$tmp/env" >"$tmp/old"
chmod 0644 "$tmp/old"
pg_before=$(value POSTGRES_PASSWORD "$tmp/old")
sh scripts/init-env.sh --add-missing "$tmp/old" .env.example >/dev/null
[ -n "$(value DB_WORKER_PASSWORD "$tmp/old")" ] && [ -n "$(value DB_API_PASSWORD "$tmp/old")" ] &&
	pass "--add-missing adds the DB role passwords" || fail "--add-missing did not add them"
[ "$(value POSTGRES_PASSWORD "$tmp/old")" = "$pg_before" ] && pass "--add-missing keeps existing values" || fail "--add-missing changed a value"
[ "$(stat -c %a "$tmp/old")" = 600 ] && pass "--add-missing leaves mode 0600" || fail "--add-missing mode"
lines=$(wc -l <"$tmp/old")
sh scripts/init-env.sh --add-missing "$tmp/old" .env.example >/dev/null
[ "$(wc -l <"$tmp/old")" = "$lines" ] && pass "--add-missing is idempotent" || fail "--add-missing added twice"

# 4. check-env: accepts the generated file, refuses everything else.
sh scripts/check-env.sh "$tmp/env" 2>/dev/null && pass "check-env accepts a generated file" || fail "check-env refused a generated file"
refuses() {
	if sh scripts/check-env.sh "$1" 2>"$tmp/err"; then
		fail "check-env accepted: $2"
	elif grep -q "^check-env: " "$tmp/err"; then
		pass "check-env refuses: $2"
	else
		fail "check-env failed without its message: $2"
	fi
}
refuses .env.example "the placeholders of .env.example"
grep -v '^DB_API_PASSWORD=' "$tmp/env" >"$tmp/missing"
refuses "$tmp/missing" "a missing DB_API_PASSWORD"
sed 's/^MINIO_ROOT_PASSWORD=.*/MINIO_ROOT_PASSWORD=has@and:in-it-0123456789/' "$tmp/env" >"$tmp/chars"
refuses "$tmp/chars" "a password with URL characters"
sed 's/^DB_WORKER_PASSWORD=.*/DB_WORKER_PASSWORD=short/' "$tmp/env" >"$tmp/short"
refuses "$tmp/short" "a short password"
{
	cat "$tmp/env"
	echo "KEYCLOAK_ADMIN_PASSWORD=change-me-keycloak"
} >"$tmp/later"
refuses "$tmp/later" "a placeholder repeated later in the file"
refuses "$tmp/nonexistent" "a missing file"

if [ "$failures" -gt 0 ]; then
	printf 'init-env.test.sh: %s failure(s)\n' "$failures" >&2
	exit 1
fi
printf 'init-env.test.sh: all passed\n'

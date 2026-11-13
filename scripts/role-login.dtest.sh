#!/bin/sh
# Test of scripts/role-login.sh against a real, throwaway PostgreSQL of the
# pinned image (make test-scripts runs it on the host: it needs Docker). The
# server sits on its own internal network (no internet, nothing published).
# A right password must log in; a wrong one must fail (the check must not
# pass through the image's trusted loopback); a container with only
# loopback must refuse.
set -eu

PG_IMAGE=$(sed -n 's/^    image: \(postgres:[^ ]*\)$/\1/p' deploy/compose.yml | head -n 1)
name=geoint-role-login-test-$$
net=$name-net
failures=0
pass() { printf 'ok   %s\n' "$*"; }
fail() {
	printf 'FAIL %s\n' "$*" >&2
	failures=$((failures + 1))
}
cleanup() {
	docker rm -f "$name" "$name-lo" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
}
trap cleanup EXIT
superpw=$(dd if=/dev/urandom bs=18 count=1 2>/dev/null | base64 | tr -d '/+=')
rolepw=$(dd if=/dev/urandom bs=18 count=1 2>/dev/null | base64 | tr -d '/+=')
docker network create --internal "$net" >/dev/null
docker run -d --name "$name" --network "$net" -e POSTGRES_PASSWORD="$superpw" -e POSTGRES_DB=geoint "$PG_IMAGE" >/dev/null
i=0
until docker exec "$name" pg_isready -U postgres -d geoint >/dev/null 2>&1; do
	i=$((i + 1))
	[ $i -lt 60 ] || {
		echo "role-login test: postgres did not start" >&2
		exit 1
	}
	sleep 1
done
sleep 2 # the entrypoint restarts the server once after initdb
until docker exec "$name" pg_isready -U postgres -d geoint >/dev/null 2>&1; do sleep 1; done
docker exec -i "$name" psql -q -U postgres -d geoint -v ON_ERROR_STOP=1 -v pw="$rolepw" >/dev/null <<'SQL'
CREATE ROLE role_test LOGIN PASSWORD :'pw';
SQL
script=$(cat scripts/role-login.sh)
login() { printf '%s' "$2" | docker exec -i "$1" sh -c "$script" sh role_test 2>/dev/null; }

if [ "$(login "$name" "$rolepw")" = role_test ]; then pass "right password logs in"; else fail "right password did not log in"; fi
if out=$(login "$name" "wrong-$rolepw"); then fail "wrong password logged in ($out): the check passes through trust"; else pass "wrong password refused"; fi
if out=$(login "$name" ""); then fail "empty password logged in ($out)"; else pass "empty password refused"; fi
# A server with only loopback: the script must refuse rather than pass.
docker run -d --name "$name-lo" --network none -e POSTGRES_PASSWORD="$superpw" -e POSTGRES_DB=geoint "$PG_IMAGE" >/dev/null
until docker exec "$name-lo" pg_isready -U postgres -d geoint >/dev/null 2>&1; do sleep 1; done
if printf '%s' "$superpw" | docker exec -i "$name-lo" sh -c "$script" sh postgres >/dev/null 2>&1; then
	fail "loopback-only container passed"
else pass "loopback-only container refused"; fi

if [ "$failures" -gt 0 ]; then
	echo "role-login.dtest.sh: $failures failure(s)" >&2
	exit 1
fi
echo "role-login.dtest.sh: all checks passed"

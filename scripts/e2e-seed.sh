#!/bin/sh
# Starts the e2e stack (deploy/compose.yml + deploy/compose.e2e.yml, images
# already built) and loads the fixtures of testdata/ into it, the way the
# owner would: migrations first with the loaders stopped, then api and
# worker, then one full `geoint collect <source>` per source, each awaited
# until its collection is done and the search tables are rebuilt.
#
#   E2E_PROJECT=geoint-e2e E2E_ENV_FILE=<settings file> sh scripts/e2e-seed.sh   (from the repo root)
#
# E2E_ENV_FILE defaults to .env; make e2e passes a throwaway file of random
# secrets (scripts/init-env.sh). Migrations run as the database owner (the
# migrate service), the api and worker as their own roles (geoint_api,
# geoint_worker), as in production.
#
# Used by `make e2e`; the later measurement step can reuse it as is. Every
# wait is bounded (E2E_WAIT seconds per step, default 180) and a timeout or
# a failed collection prints the worker's last log lines. No secret is
# printed: credentials stay in .env and inside the containers.
set -eu

PROJECT=${E2E_PROJECT:-geoint-e2e}
WAIT=${E2E_WAIT:-180}
ENV_FILE=${E2E_ENV_FILE:-.env}
SOURCES="receita tse camara senado cvm"
# A fixture company every search rebuild must contain (testdata/receita).
KNOWN_CNPJ=98990001

dc() {
	docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f deploy/compose.yml -f deploy/compose.e2e.yml --profile tools "$@"
}

log() {
	printf 'e2e-seed: %s\n' "$*" >&2
}

fail() {
	log "FAILED: $*"
	log "last worker log lines:"
	dc logs --no-color --tail 60 worker >&2 || true
	exit 1
}

# sql QUERY: runs one query in the postgres container (credentials from the
# container's own environment) and prints the unaligned result.
sql() {
	printf '%s\n' "$1" | dc exec -T postgres sh -c \
		'psql -X -q -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
}

# probe URL: GET from inside the network (busybox wget of the web image).
probe() {
	dc exec -T web wget -q -O /dev/null "$1" >/dev/null 2>&1
}

# wait_until WHAT CMD...: runs CMD every 2 s until it succeeds, at most WAIT s.
wait_until() {
	what=$1
	shift
	start=$(date +%s)
	until "$@"; do
		if [ $(($(date +%s) - start)) -ge "$WAIT" ]; then
			fail "$what: not ready after ${WAIT}s"
		fi
		sleep 2
	done
	log "$what: ready after $(($(date +%s) - start))s"
}

# The CVM connector collects this year and the one before (a missing current
# year is accepted, a missing previous one fails): the fixtures cover runs up
# to the year after the newest fixture year.
newest=0
for f in testdata/cvm/fre_cia_aberta_posicao_acionaria_*.csv; do
	y=${f##*_}
	y=${y%.csv}
	[ "$y" -gt "$newest" ] && newest=$y
done
if [ "$(date -u +%Y)" -gt $((newest + 1)) ]; then
	log "FAILED: fixtures da CVM cobrem até $((newest + 1)); acrescente os anos novos em testdata/cvm"
	exit 1
fi

log "starting postgres, nats, minio, keycloak, web and the source simulator"
dc up -d --wait postgres nats minio sourcesim web keycloak

wait_until "keycloak realm geoint-e2e" probe http://keycloak:8080/realms/geoint-e2e/.well-known/openid-configuration

log "migrating (api and worker not started yet)"
dc run --rm --no-deps migrate

log "starting api and worker"
dc up -d --no-deps api worker
wait_until "api /healthz" probe http://api:8080/healthz

# last_id SOURCE: id of the newest collection of SOURCE (0 when none).
last_id() {
	sql "SELECT coalesce(max(c.id), 0) FROM collection c JOIN source s ON s.id = c.source_id WHERE s.key = '$1'"
}

# collection_state SOURCE AFTER: "<id>|<status>" of the newest full
# collection of SOURCE with id > AFTER, or nothing.
collection_state() {
	sql "SELECT c.id || '|' || c.status FROM collection c JOIN source s ON s.id = c.source_id
		WHERE s.key = '$1' AND c.id > $2 AND (c.scope ->> 'full')::boolean IS TRUE ORDER BY c.id DESC LIMIT 1"
}

# search_current: the search tables are built and not dirty (the negation of
# internal/search's dirty rule: no rebuild requested after the last one
# started reading, and it covered every finished collection).
search_current() {
	[ "$(sql "SELECT s.generation > 0
		AND NOT coalesce(s.requested_at > coalesce(s.rebuilt_from, '-infinity'), false)
		AND coalesce(s.finished_collections, 0) >= (SELECT count(*) FROM collection WHERE status <> 'running')
		FROM search_state s")" = t ]
}

for src in $SOURCES; do
	before=$(last_id "$src")
	log "collecting $src (full scope)"
	dc run --rm --no-deps api collect "$src" >/dev/null
	start=$(date +%s)
	while :; do
		state=$(collection_state "$src" "$before")
		case $state in
		*'|done') break ;;
		*'|failed')
			err=$(sql "SELECT error FROM collection WHERE id = ${state%%|*}")
			fail "$src collection ${state%%|*} failed: $err"
			;;
		esac
		if [ $(($(date +%s) - start)) -ge "$WAIT" ]; then
			fail "$src: no done collection after ${WAIT}s (last state: ${state:-none})"
		fi
		sleep 2
	done
	log "$src: collection ${state%%|*} done after $(($(date +%s) - start))s"
	wait_until "search rebuilt after $src" search_current
done

found=$(sql "SELECT count(*) FROM search_company sc JOIN entity e ON e.id = sc.entity_id
	WHERE e.kind = 'company' AND e.natural_key = '$KNOWN_CNPJ'")
[ "$found" = 1 ] || fail "search tables do not contain the fixture company $KNOWN_CNPJ"

log "seeded: $(sql "SELECT string_agg(s.key || '=' || c.status, ' ' ORDER BY s.id) FROM source s
	JOIN LATERAL (SELECT status FROM collection WHERE source_id = s.id ORDER BY id DESC LIMIT 1) c ON true")"

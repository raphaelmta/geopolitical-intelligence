#!/bin/sh
# Samples a running stack once every INTERVAL seconds (default 60) while a
# load runs, and appends one tab-separated line per sample to OUT (default
# results/<project>/samples.tsv, git-ignored). Changes nothing; reads only
# sizes, counters and statuses (never data rows). Stop it with Ctrl-C or
# kill. Columns: see the header line written on the first sample.
#
#   PROJECT=geoint-national ENV_FILE=.env.national sh scripts/load-sampler.sh
set -eu

PROJECT=${PROJECT:-geoint}
ENV_FILE=${ENV_FILE:-.env}
INTERVAL=${INTERVAL:-60}
OUT=${OUT:-results/$PROJECT/samples.tsv}
mkdir -p "$(dirname "$OUT")"
dc() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f deploy/compose.yml "$@"; }
psql_q() {
	printf '%s\n' "$1" | dc exec -T postgres sh -c 'psql -X -q -A -t -F "	" -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' 2>/dev/null | tr '\n' ' '
}
root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)
[ -s "$OUT" ] || printf 'ts\tdisk_used_gb\tdisk_free_gb\tshm_used_mb\tpg_mem\tworker_mem\tminio_mem\tapi_mem\twal_mb\tdb_mb\treltuples(entity,person_record,relationship,company_version)\tvacuums\tcheckpoints(timed,req)\tactive\tcollections\n' >"$OUT"
while :; do
	ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	disk=$(df -Pk "$root" | awk 'NR == 2 { printf "%.1f\t%.1f", $3 / 1048576, $4 / 1048576 }')
	shm=$(dc exec -T postgres df -Pk /dev/shm 2>/dev/null | awk 'NR == 2 { printf "%d", $3 / 1024 }')
	mem() {
		id=$(dc ps -q "$1" 2>/dev/null)
		[ -n "$id" ] && docker stats --no-stream --format '{{.MemUsage}}' "$id" 2>/dev/null | cut -d/ -f1 | tr -d ' ' || echo -
	}
	pg=$(mem postgres) wk=$(mem worker) mn=$(mem minio) ap=$(mem api)
	wal=$(psql_q "SELECT (coalesce(sum(size), 0) / 1048576)::bigint FROM pg_ls_waldir()")
	db=$(psql_q "SELECT pg_database_size(current_database()) / 1048576")
	rel=$(psql_q "SELECT string_agg(coalesce(c.reltuples::bigint, -1)::text, ',' ORDER BY t.o)
		FROM (VALUES (1, 'entity'), (2, 'person_record'), (3, 'relationship'), (4, 'company_version')) t(o, n)
		LEFT JOIN pg_class c ON c.relname = t.n AND c.relkind = 'r'")
	vac=$(psql_q "SELECT count(*) FROM pg_stat_progress_vacuum")
	ckp=$(psql_q "SELECT num_timed || ',' || num_requested FROM pg_stat_checkpointer")
	act=$(psql_q "SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND backend_type = 'client backend'")
	cols=$(psql_q "SELECT coalesce(string_agg(c.id || ':' || s.key || ':' || c.status, ',' ORDER BY c.id), '-')
		FROM collection c JOIN source s ON s.id = c.source_id")
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$ts" "$disk" "${shm:--}" "$pg" "$wk" "$mn" "$ap" \
		"$wal" "$db" "$rel" "$vac" "$ckp" "$act" "$cols" | tr -s ' ' >>"$OUT"
	sleep "$INTERVAL"
done

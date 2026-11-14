#!/bin/sh
# Load benchmark (make bench-load; docs/performance/benchmark.md): runs the
# real collection path (geoint collect -> worker: fetch, load, CloseUnseen,
# Resolve, search rebuild) on a sample of a Receita edition, twice: the first
# load (edition 2026-10 of the sample) and the monthly re-load (edition
# 2026-11 of the sample), in a throwaway compose project with fresh volumes
# and no published port. Records, per run, the configuration, the worker's own
# step timings, the top statements (pg_stat_statements), sampled wait events,
# I/O per table and index, WAL volume and sizes, under results/bench/
# (git-ignored). Counts and timings only: no data row is read or printed.
#
#   sh scripts/bench-load.sh                       (from the repo root)
#
# Environment (all optional):
#   LABEL=baseline          name of the run (results/bench/<UTC time>-<label>)
#   STEP=all                all | up | first | reload | report | down
#                           (up .. down one at a time keep the stack for
#                           experiments; all = up first reload report down)
#   KEEP=1                  with STEP=all: leave the stack running at the end
#   BENCH_SAMPLE_DIR        sample editions (default data/bench/receita/GO)
#   BENCH_PROFILE=roomy     roomy | pressure (PostgreSQL memory, below); any
#                           PG_* value and BENCH_PG_MEM_LIMIT override it
#   BENCH_NOCOW=1           set the No_COW attribute (chattr +C) on the empty
#                           pgdata volume before initdb (btrfs only)
#   BENCH_PG_EXTRA          "name=value;name=value": ALTER SYSTEM settings
#   BENCH_WORKER_SET        "name=value;...": ALTER ROLE geoint_worker SET ...
#   BENCH_SQL_AFTER_MIGRATE a .sql file run as the database owner after the
#                           migrations (experiments: drop a trigger, an index)
#   BENCH_WAIT=14400        seconds to wait for one collection
#
# The project name is fixed (geoint-bench, or geoint-bench-<suffix> through
# BENCH_PROJECT): the script refuses any other name, so `down --volumes` can
# never reach the stacks in use (geoint, geoint-national).
set -eu

PROJECT=${BENCH_PROJECT:-geoint-bench}
case "$PROJECT" in
geoint-bench | geoint-bench-*) ;;
*)
	echo "bench-load: refusing project name '$PROJECT' (must be geoint-bench or geoint-bench-<suffix>)" >&2
	exit 2
	;;
esac
STEP=${STEP:-all}
LABEL=${LABEL:-run}
WAIT=${BENCH_WAIT:-14400}
BASE=results/bench
ENV_FILE=$BASE/env
HOST_UID=$(id -u)
HOST_GID=$(id -g)
BENCH_SAMPLE_DIR=${BENCH_SAMPLE_DIR:-data/bench/receita/GO}
case "$BENCH_SAMPLE_DIR" in /*) ;; *) BENCH_SAMPLE_DIR=$PWD/$BENCH_SAMPLE_DIR ;; esac
export HOST_UID HOST_GID BENCH_SAMPLE_DIR

# PostgreSQL profile. The machine is shared (the national stack keeps 8 GB of
# shared_buffers), so both are far below production: results are RELATIVE.
#   roomy:    everything fits in memory (CPU, WAL and trigger costs show).
#   pressure: memory scaled to the sample as the national run was to the
#             national base (about one fifth of the database), by a small
#             shared_buffers and a container memory limit that also bounds the
#             page cache: random reads become real I/O.
BENCH_PROFILE=${BENCH_PROFILE:-roomy}
case "$BENCH_PROFILE" in
roomy)
	: "${PG_SHARED_BUFFERS:=4GB}" "${PG_EFFECTIVE_CACHE_SIZE:=8GB}" "${PG_MAINTENANCE_WORK_MEM:=1GB}"
	: "${PG_MAX_WAL_SIZE:=4GB}" "${PG_SHM_SIZE:=2g}" "${BENCH_PG_MEM_LIMIT:=64g}"
	;;
pressure)
	: "${PG_SHARED_BUFFERS:=320MB}" "${PG_EFFECTIVE_CACHE_SIZE:=700MB}" "${PG_MAINTENANCE_WORK_MEM:=128MB}"
	: "${PG_MAX_WAL_SIZE:=2GB}" "${PG_SHM_SIZE:=1g}" "${BENCH_PG_MEM_LIMIT:=1100m}"
	: "${PG_AUTOVACUUM_WORK_MEM:=64MB}" "${PG_AUTOVACUUM_MAX_WORKERS:=3}"
	;;
*)
	echo "bench-load: unknown BENCH_PROFILE '$BENCH_PROFILE' (roomy | pressure)" >&2
	exit 2
	;;
esac
: "${PG_WORK_MEM:=64MB}" "${PG_AUTOVACUUM_WORK_MEM:=256MB}" "${PG_AUTOVACUUM_MAX_WORKERS:=3}"
export PG_SHARED_BUFFERS PG_EFFECTIVE_CACHE_SIZE PG_MAINTENANCE_WORK_MEM PG_MAX_WAL_SIZE PG_SHM_SIZE \
	PG_WORK_MEM PG_AUTOVACUUM_WORK_MEM PG_AUTOVACUUM_MAX_WORKERS BENCH_PG_MEM_LIMIT

log() { printf 'bench-load: %s\n' "$*" >&2; }

dc() {
	docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f deploy/compose.yml -f deploy/compose.bench.yml --profile tools "$@"
}

# sql: reads SQL on stdin, runs it as the database owner in the bench postgres.
sql() {
	dc exec -T postgres sh -c 'psql -X -q -A -t -F "	" -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
}

# The run directory is chosen by `up` and remembered for the later steps.
run_dir() {
	if [ ! -s "$BASE/current" ]; then
		echo "bench-load: no run in progress (run STEP=up first)" >&2
		exit 2
	fi
	cat "$BASE/current"
}

fail() {
	log "FAILED: $*"
	dc logs --no-color --tail 40 worker >&2 || true
	exit 1
}

# set_list "a=b;c=d" PREFIX: prints "PREFIX a = 'b';" for every item.
set_list() {
	printf '%s' "$1" | tr ';' '\n' | while IFS='=' read -r k v; do
		[ -n "$k" ] && printf "%s %s = '%s';\n" "$2" "$k" "$v"
	done
}

step_up() {
	[ -d "$BENCH_SAMPLE_DIR/2026-10" ] && [ -d "$BENCH_SAMPLE_DIR/2026-11" ] || {
		echo "bench-load: no sample editions in $BENCH_SAMPLE_DIR (see docs/performance/benchmark.md: make bench-sample)" >&2
		exit 2
	}
	mkdir -p "$BASE"
	[ -f "$ENV_FILE" ] || sh scripts/init-env.sh "$ENV_FILE" >/dev/null
	docker volume inspect geoint-gocache >/dev/null 2>&1 || {
		echo "bench-load: run 'make cache' first (the shared Go build cache volume)" >&2
		exit 2
	}
	dir=$BASE/$(date -u +%Y%m%dT%H%M%SZ)-$LABEL
	mkdir -p "$dir"
	printf '%s\n' "$dir" >"$BASE/current"

	log "project $PROJECT: removing any earlier bench stack and its volumes"
	dc down --volumes --remove-orphans >/dev/null 2>&1 || true
	dc build worker migrate api >"$dir/build.log" 2>&1 || fail "image build (see $dir/build.log)"

	# Volumes are created (and given to their users) before anything is written.
	dc run --rm --no-deps volume-init >/dev/null 2>&1 || fail "volume-init"
	root=$(docker info --format '{{.DockerRootDir}}')
	fstype=$(df -PT "$root" | awk 'NR == 2 { print $2 }')
	nocow=no
	if [ "${BENCH_NOCOW:-0}" = 1 ]; then
		[ "$fstype" = btrfs ] || fail "BENCH_NOCOW=1 needs a btrfs Docker root (found $fstype)"
		docker run --rm -u 0 --cap-drop ALL --cap-add FOWNER --network none -v "${PROJECT}_pgdata:/v" \
			alpine:3.24 sh -c 'chattr +C /v && lsattr -d /v' >"$dir/nocow.txt" 2>&1 || fail "chattr +C on the pgdata volume"
		nocow=yes
	fi

	dc up -d --wait postgres nats minio share >/dev/null 2>&1 || fail "infrastructure did not start"
	dc run --rm --no-deps migrate >"$dir/migrate.log" 2>&1 || fail "migration (see $dir/migrate.log)"
	{
		echo "ALTER SYSTEM SET shared_preload_libraries = 'pg_stat_statements';"
		echo "ALTER SYSTEM SET track_io_timing = on;"
		echo "ALTER SYSTEM SET track_wal_io_timing = on;"
		echo "ALTER SYSTEM SET track_functions = 'pl';"
		echo "ALTER SYSTEM SET log_autovacuum_min_duration = 0;"
		set_list "${BENCH_PG_EXTRA:-}" "ALTER SYSTEM SET"
		set_list "${BENCH_WORKER_SET:-}" "ALTER ROLE geoint_worker SET"
	} | sql || fail "benchmark settings"
	dc restart postgres >/dev/null 2>&1
	dc up -d --wait postgres >/dev/null 2>&1 || fail "postgres restart"
	echo "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;" | sql
	if [ -n "${BENCH_SQL_AFTER_MIGRATE:-}" ]; then
		sql <"$BENCH_SQL_AFTER_MIGRATE" >"$dir/after-migrate.out" 2>&1 || fail "BENCH_SQL_AFTER_MIGRATE (see $dir/after-migrate.out)"
		cp "$BENCH_SQL_AFTER_MIGRATE" "$dir/after-migrate.sql"
	fi
	dc up -d --wait worker >/dev/null 2>&1 || fail "worker did not start"

	{
		echo "label=$LABEL"
		echo "project=$PROJECT"
		echo "started=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "commit=$(git rev-parse HEAD 2>/dev/null || echo unknown)"
		echo "tree=$(git status --porcelain 2>/dev/null | wc -l) changed files"
		echo "sample=$BENCH_SAMPLE_DIR"
		echo "profile=$BENCH_PROFILE"
		echo "pg_mem_limit=$BENCH_PG_MEM_LIMIT"
		echo "docker_root=$root ($fstype; $(findmnt -no OPTIONS -T "$root" 2>/dev/null || echo '?'))"
		echo "pgdata_nocow=$nocow"
		echo "pg_extra=${BENCH_PG_EXTRA:-}"
		echo "worker_set=${BENCH_WORKER_SET:-}"
		echo "sql_after_migrate=${BENCH_SQL_AFTER_MIGRATE:-}"
		echo "host=$(nproc) cores, $(awk '/MemTotal/ { printf "%.0f GB RAM", $2 / 1048576 }' /proc/meminfo), $(awk '/MemAvailable/ { printf "%.0f GB available", $2 / 1048576 }' /proc/meminfo)"
		echo "other_containers=$(docker ps --format '{{.Names}}' | grep -vc "^$PROJECT-" || true)"
		echo "--- pg_settings (not default) ---"
		echo "SELECT name || '=' || current_setting(name) FROM pg_settings WHERE source NOT IN ('default', 'override') ORDER BY name;" | sql
	} >"$dir/config.txt"
	log "stack up; run directory $dir"
}

# Samplers: two psql sessions that print one batch of lines per interval.
WAITS_SQL="SELECT /* bench-sampler */ to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS'), backend_type,
	coalesce(wait_event_type, 'CPU'), coalesce(wait_event, '-'),
	left(regexp_replace(regexp_replace(query, '''[^'']*''', '?', 'g'), '\\s+', ' ', 'g'), 60)
FROM pg_stat_activity
WHERE pid <> pg_backend_pid() AND datname = current_database() AND query NOT LIKE '%bench-sampler%'
	AND ((state = 'active' AND backend_type IN ('client backend', 'parallel worker')) OR backend_type = 'autovacuum worker')"
COUNTERS_SQL="WITH t AS (SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS') AS ts)
SELECT /* bench-sampler */ t.ts, 'wal', '', wal_bytes::text, wal_fpi::text, wal_records::text, '' FROM pg_stat_wal, t
UNION ALL SELECT t.ts, 'db', '', pg_database_size(current_database())::text, '', '', '' FROM t
UNION ALL SELECT t.ts, 'ckpt', '', num_timed::text, num_requested::text, buffers_written::text, '' FROM pg_stat_checkpointer, t
UNION ALL SELECT t.ts, 'heap', relname, heap_blks_read::text, heap_blks_hit::text, coalesce(toast_blks_read, 0)::text,
		(n_tup_ins || '/' || n_tup_upd || '/' || n_tup_hot_upd)
	FROM pg_statio_user_tables JOIN pg_stat_user_tables USING (relid, relname), t
	WHERE relname IN ('entity', 'person_record', 'relationship', 'company_version', 'collection', 'identity_decision', 'identity_proposal')
UNION ALL SELECT t.ts, 'idx', indexrelname, idx_blks_read::text, idx_blks_hit::text, '', '' FROM pg_statio_user_indexes, t
	WHERE relname IN ('entity', 'person_record', 'relationship', 'company_version')
UNION ALL SELECT t.ts, 'io', backend_type || '/' || object || '/' || context, coalesce(reads, 0)::text,
		coalesce(read_time, 0)::bigint::text, coalesce(writes, 0)::text, coalesce(extends, 0)::text
	FROM pg_stat_io, t WHERE coalesce(reads, 0) + coalesce(writes, 0) + coalesce(extends, 0) > 0"

start_samplers() {
	dir=$1
	printf '%s\n\\watch 1\n' "$WAITS_SQL" | sql >>"$dir/waits.tsv" 2>/dev/null &
	echo $! >"$dir/.waits.pid"
	printf '%s\n\\watch 10\n' "$COUNTERS_SQL" | sql >>"$dir/counters.tsv" 2>/dev/null &
	echo $! >"$dir/.counters.pid"
}

stop_samplers() {
	dir=$1
	for f in "$dir/.waits.pid" "$dir/.counters.pid"; do
		[ -s "$f" ] && kill "$(cat "$f")" 2>/dev/null || true
		rm -f "$f"
	done
	# The psql sessions inside the container end with their client.
	echo "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE '%bench-sampler%' AND pid <> pg_backend_pid();" | sql >/dev/null 2>&1 || true
}

# snapshot NAME: statements, sizes and counters since the last snapshot.
snapshot() {
	dir=$(run_dir)
	name=$1
	echo "SELECT calls, round(total_exec_time)::bigint AS ms, round(mean_exec_time::numeric, 1), rows, shared_blks_hit, shared_blks_read,
			shared_blks_dirtied, shared_blks_written, round(shared_blk_read_time)::bigint, round(shared_blk_write_time)::bigint,
			temp_blks_written, wal_records, wal_fpi, wal_bytes,
			left(regexp_replace(regexp_replace(query, '''[^'']*''', '?', 'g'), '\\s+', ' ', 'g'), 110)
		FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		ORDER BY total_exec_time DESC LIMIT 60;" | sql >"$dir/statements-$name.tsv"
	sed -i '1i calls\tms\tmean_ms\trows\tblks_hit\tblks_read\tblks_dirtied\tblks_written\tread_ms\twrite_ms\ttemp_written\twal_records\twal_fpi\twal_bytes\tquery' "$dir/statements-$name.tsv"
	echo "SELECT c.relname, c.relkind, pg_relation_size(c.oid), c.reltuples::bigint,
			coalesce((SELECT pg_relation_size(c.reltoastrelid) WHERE c.reltoastrelid <> 0), 0)
		FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'i') AND pg_relation_size(c.oid) > 1048576
		ORDER BY 3 DESC;" | sql >"$dir/sizes-$name.tsv"
	sed -i '1i relation\tkind\tbytes\treltuples\ttoast_bytes' "$dir/sizes-$name.tsv"
	echo "SELECT funcname, calls, round(total_time)::bigint, round(self_time)::bigint FROM pg_stat_user_functions ORDER BY total_time DESC;" |
		sql >"$dir/functions-$name.tsv"
	sed -i '1i function\tcalls\ttotal_ms\tself_ms' "$dir/functions-$name.tsv"
	echo "SELECT relname, n_tup_ins, n_tup_upd, n_tup_hot_upd, n_tup_newpage_upd, n_live_tup, n_dead_tup, vacuum_count + autovacuum_count,
			analyze_count + autoanalyze_count
		FROM pg_stat_user_tables WHERE n_tup_ins + n_tup_upd > 0 ORDER BY relname;" | sql >"$dir/tables-$name.tsv"
	sed -i '1i table\tins\tupd\thot_upd\tnewpage_upd\tlive\tdead\tvacuums\tanalyzes' "$dir/tables-$name.tsv"
	echo "SELECT 'db_bytes', pg_database_size(current_database())
		UNION ALL SELECT 'wal_bytes', wal_bytes FROM pg_stat_wal UNION ALL SELECT 'wal_fpi', wal_fpi FROM pg_stat_wal
		UNION ALL SELECT 'wal_records', wal_records FROM pg_stat_wal
		UNION ALL SELECT 'wal_dir_bytes', coalesce(sum(size), 0) FROM pg_ls_waldir()
		UNION ALL SELECT 'entity', count(*) FROM entity UNION ALL SELECT 'person_record', count(*) FROM person_record
		UNION ALL SELECT 'relationship', count(*) FROM relationship
		UNION ALL SELECT 'relationship_open', count(*) FROM relationship WHERE closed_collection_id IS NULL
		UNION ALL SELECT 'company_version', count(*) FROM company_version
		UNION ALL SELECT 'company_version_open', count(*) FROM company_version WHERE closed_collection_id IS NULL
		UNION ALL SELECT 'identity_decision', count(*) FROM identity_decision
		UNION ALL SELECT 'identity_proposal_pending', count(*) FROM identity_proposal WHERE status = 'pending'
		UNION ALL SELECT 'person_entities_empty', count(*) FROM entity e WHERE e.kind = 'person'
			AND NOT EXISTS (SELECT 1 FROM person_record p WHERE p.entity_id = e.id);" | sql >"$dir/totals-$name.tsv"
	echo "SELECT pg_stat_statements_reset(); SELECT pg_stat_reset_shared('wal'); SELECT pg_stat_reset();" | sql >/dev/null
}

# collect NAME REF: queues one full Receita collection and waits for its end
# (collection done or failed, then the search rebuild of the Finished hook).
collect() {
	dir=$(run_dir)
	name=$1
	ref=$2
	before=$(dc logs --no-color worker 2>/dev/null | grep -c '"msg":"collection finished"\|"msg":"collection failed"' || true)
	start_samplers "$dir"
	t0=$(date +%s)
	dc run --rm --no-deps api collect receita --ref "$ref" >>"$dir/collect.log" 2>&1 || {
		stop_samplers "$dir"
		fail "collect $ref (see $dir/collect.log)"
	}
	log "$name: collection requested (ref $ref)"
	while :; do
		now=$(dc logs --no-color worker 2>/dev/null | grep -c '"msg":"collection finished"\|"msg":"collection failed"' || true)
		[ "$now" -gt "$before" ] && break
		if [ $(($(date +%s) - t0)) -ge "$WAIT" ]; then
			stop_samplers "$dir"
			fail "$name: not finished after ${WAIT}s"
		fi
		sleep 5
	done
	stop_samplers "$dir"
	log "$name: ended after $(($(date +%s) - t0))s"
	dc logs --no-color --no-log-prefix worker >"$dir/worker.log" 2>/dev/null
	dc logs --no-color --no-log-prefix postgres >"$dir/postgres.log" 2>&1
	snapshot "$name"
	if grep -q '"msg":"collection failed"' "$dir/worker.log"; then
		fail "$name: the collection failed"
	fi
}

step_report() {
	dir=$(run_dir)
	dc logs --no-color --no-log-prefix worker >"$dir/worker.log" 2>/dev/null || true
	awk -f scripts/bench-report.awk "$dir/worker.log" >"$dir/phases.tsv"
	{
		echo "# Benchmark run $(basename "$dir")"
		echo
		echo '```'
		sed -n '1,/^--- pg_settings/p' "$dir/config.txt" | sed '$d'
		echo '```'
		echo
		echo "## Phases (from the worker's log)"
		echo
		echo '| collection | phase | step | rows | seconds |'
		echo '|---|---|---|---|---|'
		awk -F '\t' '{ printf "| %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $5 }' "$dir/phases.tsv"
		echo
		echo "## Wait events of the loading backends (1 s samples)"
		echo
		echo '```'
		if [ -s "$dir/waits.tsv" ]; then
			awk -F '\t' 'NF >= 4 && $2 != "autovacuum worker" { n[$3 "/" $4]++; t++ }
				END { for (k in n) printf "%6d %5.1f%% %s\n", n[k], 100 * n[k] / t, k }' "$dir/waits.tsv" | sort -rn | head -15
		fi
		echo '```'
		for f in "$dir"/totals-*.tsv; do
			[ -f "$f" ] || continue
			echo
			echo "## Totals after $(basename "$f" .tsv | sed 's/totals-//')"
			echo
			echo '```'
			cat "$f"
			echo '```'
		done
	} >"$dir/summary.md"
	log "report: $dir/summary.md"
}

step_down() {
	log "project $PROJECT: removing the bench stack and its volumes"
	dc down --volumes --remove-orphans >/dev/null 2>&1 || true
	left=$(docker volume ls -q --filter "label=com.docker.compose.project=$PROJECT" | wc -l)
	log "volumes left for $PROJECT: $left"
	rm -f "$BASE/current"
}

case "$STEP" in
up) step_up ;;
first) collect first 2026-10-31 ;;
reload) collect reload "$(date -u +%Y-%m-%d)" ;;
snapshot) snapshot "${NAME:-manual}" ;;
report) step_report ;;
down) step_down ;;
all)
	step_up
	collect first 2026-10-31
	collect reload "$(date -u +%Y-%m-%d)"
	step_report
	[ "${KEEP:-0}" = 1 ] || step_down
	;;
*)
	echo "bench-load: unknown STEP '$STEP' (all | up | first | reload | snapshot | report | down)" >&2
	exit 2
	;;
esac

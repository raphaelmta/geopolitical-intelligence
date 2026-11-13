#!/bin/sh
# Measures a loaded stack THROUGH ITS AUTHENTICATED API (make measure;
# docs/performance/): search (M1), entity page, neighbours (M2), paths (M3)
# and control group (M4), and collects the database figures that go with
# them (sizes, counts per source, identity buckets, collections). Changes no
# data: the samples and the figures are read-only SQL as the database owner;
# the API calls are reads plus background jobs (paths, control group), as an
# analyst would make them.
#
#   PROJECT=geoint-national ENV_FILE=.env.national PASS=cold sh scripts/measure.sh
#
# STEP: all (default: samples if missing, then run, then stats), samples,
# run, stats, criteria (the success-criteria walk; CNPJ, A, B), cleanup
# (deletes temporary users a killed run left; also run automatically at the
# end of run and criteria, on success, failure or interrupt). PASS names the run's output files (cold, warm, ...). ONLY
# limits the run to some measures (m1,entity,m2,m3,m4). OUT: output folder
# (default results/<project>/measure, git-ignored).
#
# The run creates a temporary Keycloak user with role analyst (admin REST
# API, the bootstrap admin of the settings file, passed to the measuring
# program through its environment, never on a command line), logs in with
# authorization code + PKCE like the web app and deletes the user at the end
# (cmd/measure). The measuring program reaches the stack through its
# published ports (127.0.0.1:8080 and 8081), so it runs with the host network.
#
# Samples (ids only) are chosen once by deterministic SQL (fixed seeds,
# TABLESAMPLE ... REPEATABLE and hashint8extended order) and kept in
# OUT/samples, so every pass measures the same entities. Outputs: OUT/<pass>.json,
# .csv and .md (timings and ids), OUT/stats.txt (counts and sizes only: no
# names, CPFs or other personal values).
set -eu

PROJECT=${PROJECT:-geoint}
ENV_FILE=${ENV_FILE:-.env}
STEP=${STEP:-all}
PASS=${PASS:-run}
ONLY=${ONLY:-}
OUT=${OUT:-results/$PROJECT/measure}
GO_IMAGE=golang:1.27.4-alpine3.24@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf
ALPINE_IMAGE=alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
SAMPLES=$OUT/samples
mkdir -p "$OUT"

dc() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f deploy/compose.yml "$@"; }
env_value() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1; }
# psql as the owner, inside the postgres container; SQL on standard input.
psql_owner() {
	dc exec -T postgres sh -c 'psql -X -q -A -t -F " " -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
}
log() { printf 'measure.sh: %s\n' "$*" >&2; }

build() {
	log "building cmd/measure"
	docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e GOFLAGS=-buildvcs=false -e GOCACHE=/cache/build \
		-e GOMODCACHE=/cache/mod -e CGO_ENABLED=0 -v "$(pwd):/src" -v geoint-gocache:/cache -w /src "$GO_IMAGE" \
		go build -o "$OUT/bin/measure" ./cmd/measure
}

samples() {
	if [ -s "$SAMPLES/pairs.txt" ]; then
		log "samples already in $SAMPLES (delete the folder to choose new ones)"
		return 0
	fi
	mkdir -p "$SAMPLES.tmp"
	start=$(date +%s)
	log "choosing samples (deterministic SQL)"
	sql() { printf "SET statement_timeout = '20min';\n%s\n" "$1" | psql_owner >"$SAMPLES.tmp/$2"; }
	# 200 random entities: a block sample of entity, ordered by a seeded hash.
	sql "SELECT id FROM entity TABLESAMPLE SYSTEM (0.2) REPEATABLE (20261112)
		ORDER BY hashint8extended(id, 20261112) LIMIT 200;" entities.txt
	# The 20 highest-degree entities (degree as the search tables store it).
	sql "SELECT entity_id FROM (
			SELECT entity_id, grau FROM search_company UNION ALL
			SELECT entity_id, grau FROM search_person UNION ALL
			SELECT entity_id, grau FROM search_other) t
		WHERE entity_id IS NOT NULL GROUP BY entity_id ORDER BY max(grau) DESC, entity_id LIMIT 20;" top_degree.txt
	# 25 names to search for (first two words, read through the API), 10 CNPJs.
	sql "SELECT id FROM entity TABLESAMPLE SYSTEM (0.2) REPEATABLE (7)
		WHERE kind IN ('person', 'company') AND name LIKE '% %'
		ORDER BY hashint8extended(id, 7) LIMIT 25;" search_sampled.txt
	sql "SELECT id FROM entity TABLESAMPLE SYSTEM (0.2) REPEATABLE (11)
		WHERE kind = 'company' AND natural_key ~ '^[0-9]{8}\$'
		ORDER BY hashint8extended(id, 11) LIMIT 10;" cnpj.txt
	# Path pairs. Politicians: holders of a mandate (Câmara, Senado; found
	# through the collections' index). Groups: politician x top-degree
	# company, politician x random company, politician x a company of one of
	# their donors (a path of length 2 exists), ends of 6-step random walks
	# (from persons and companies), random pairs.
	sql "CREATE TEMP TABLE pol AS
			SELECT r.src_entity_id AS id, row_number() OVER (ORDER BY hashint8extended(r.src_entity_id, 3)) AS n
			FROM (SELECT DISTINCT src_entity_id FROM relationship
				WHERE rel_type = 'holds_mandate' AND first_collection_id IN (
					SELECT c.id FROM collection c JOIN source s ON s.id = c.source_id WHERE s.key IN ('camara', 'senado'))) r;
		CREATE TEMP TABLE topco AS
			SELECT entity_id AS id, row_number() OVER (ORDER BY grau DESC, entity_id) AS n
			FROM (SELECT entity_id, max(grau) AS grau FROM search_company WHERE entity_id IS NOT NULL
				GROUP BY entity_id ORDER BY 2 DESC, 1 LIMIT 8) t;
		CREATE TEMP TABLE randco AS
			SELECT entity_id AS id, row_number() OVER (ORDER BY hashint8extended(entity_id, 5)) AS n
			FROM search_company TABLESAMPLE SYSTEM (0.2) REPEATABLE (5) WHERE grau >= 1 AND entity_id IS NOT NULL;
		CREATE TEMP TABLE randany AS
			SELECT entity_id AS id, row_number() OVER (ORDER BY hashint8extended(entity_id, 17)) AS n
			FROM (SELECT entity_id FROM search_person TABLESAMPLE SYSTEM (0.2) REPEATABLE (17) WHERE grau >= 1
				UNION ALL SELECT entity_id FROM search_company TABLESAMPLE SYSTEM (0.2) REPEATABLE (17) WHERE grau >= 1) t
			WHERE entity_id IS NOT NULL;
		CREATE TEMP TABLE walk_start AS
			SELECT id, row_number() OVER (ORDER BY k, h) AS w FROM (
				(SELECT entity_id AS id, 1 AS k, hashint8extended(entity_id, 13) AS h
					FROM search_person TABLESAMPLE SYSTEM (0.2) REPEATABLE (13) WHERE grau >= 2 AND entity_id IS NOT NULL
					ORDER BY 3 LIMIT 3)
				UNION ALL
				(SELECT entity_id, 2, hashint8extended(entity_id, 13)
					FROM search_company TABLESAMPLE SYSTEM (0.2) REPEATABLE (13) WHERE grau >= 2 AND entity_id IS NOT NULL
					ORDER BY 3 LIMIT 3)) t;
		SELECT p.id, c.id, 'politician-topcompany' FROM pol p JOIN topco c ON c.n = p.n WHERE p.n <= 8;
		SELECT p.id, c.id, 'politician-company' FROM pol p JOIN randco c ON c.n = p.n - 8 WHERE p.n BETWEEN 9 AND 15;
		SELECT p.id, x.company, 'politician-donorcompany'
			FROM pol p CROSS JOIN LATERAL (
				SELECT pr.dst_entity_id AS company FROM relationship d
				JOIN relationship pr ON pr.src_entity_id = d.src_entity_id AND pr.rel_type = 'partner_of'
				WHERE d.dst_entity_id = p.id AND d.rel_type = 'donated_to'
				ORDER BY hashint8extended(pr.dst_entity_id, 19) LIMIT 1) x
			ORDER BY p.n LIMIT 5;
		WITH RECURSIVE walk(w, step, cur, visited) AS (
			SELECT w, 0, id, ARRAY[id] FROM walk_start
			UNION ALL
			SELECT walk.w, walk.step + 1, nx.n, walk.visited || nx.n
			FROM walk CROSS JOIN LATERAL (
				SELECT nb.n FROM (
					SELECT dst_entity_id AS n FROM relationship WHERE src_entity_id = walk.cur
					UNION SELECT src_entity_id FROM relationship WHERE dst_entity_id = walk.cur) nb
				WHERE nb.n <> ALL (walk.visited)
				ORDER BY hashint8extended(nb.n, 13 + walk.step) LIMIT 1) nx
			WHERE walk.step < 6)
		SELECT DISTINCT ON (w) visited[1], cur, 'walk' || step FROM walk WHERE step > 0 ORDER BY w, step DESC;
		SELECT a.id, b.id, 'random' FROM randany a JOIN randany b ON b.n = a.n + 100 WHERE a.n <= 5;" pairs.txt
	# Control group companies: 5 holdings (most company partners' links),
	# 5 CVM-listed companies, 5 random companies that are partners of
	# others, 5 random companies.
	sql "CREATE TEMP TABLE pjpartner AS
			SELECT r.src_entity_id AS id, count(*) AS links FROM relationship r
			WHERE r.rel_type = 'partner_of' AND r.src_record_id IS NULL GROUP BY 1;
		SELECT p.id, 'holding' FROM pjpartner p JOIN entity e ON e.id = p.id AND e.kind = 'company'
			ORDER BY p.links DESC, p.id LIMIT 5;
		SELECT id, 'cvm-listed' FROM (SELECT DISTINCT dst_entity_id AS id FROM relationship
			WHERE rel_type IN ('shareholder_of', 'controls') AND first_collection_id IN (
				SELECT c.id FROM collection c JOIN source s ON s.id = c.source_id WHERE s.key = 'cvm')) t
			ORDER BY hashint8extended(id, 23) LIMIT 5;
		SELECT p.id, 'partner-company' FROM pjpartner p JOIN entity e ON e.id = p.id AND e.kind = 'company'
			ORDER BY hashint8extended(p.id, 29) LIMIT 5;
		SELECT id, 'small' FROM entity TABLESAMPLE SYSTEM (0.2) REPEATABLE (31) WHERE kind = 'company'
			ORDER BY hashint8extended(id, 31) LIMIT 5;" control.txt
	mv "$SAMPLES.tmp" "$SAMPLES"
	log "samples chosen in $(($(date +%s) - start)) s: $(wc -l <"$SAMPLES/entities.txt") entities, $(wc -l <"$SAMPLES/top_degree.txt") top-degree, $(wc -l <"$SAMPLES/pairs.txt") pairs, $(wc -l <"$SAMPLES/control.txt") companies"
}

# The measuring program, with the Keycloak admin in its environment and the
# state file of the temporary users it created (cleanup deletes exactly
# those, never by name).
measure_tool() {
	docker run --rm --network host -u "$(id -u):$(id -g)" -e KC_ADMIN -e KC_ADMIN_PASSWORD \
		-e MEASURE_STATE=/out/measure-users.state -v "$(pwd)/$OUT:/out" "$ALPINE_IMAGE" /out/bin/measure "$@"
}

# Runs `measure cleanup` when the script ends, also on a failure or an
# interrupt: a temporary user left by a killed run is deleted.
with_cleanup() {
	KC_ADMIN=$(env_value KEYCLOAK_ADMIN)
	KC_ADMIN_PASSWORD=$(env_value KEYCLOAK_ADMIN_PASSWORD)
	export KC_ADMIN KC_ADMIN_PASSWORD
	trap 'measure_tool cleanup || log "WARNING: cleanup failed; run STEP=cleanup"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
}

run() {
	build
	with_cleanup
	log "pass $PASS${ONLY:+ ($ONLY)}"
	measure_tool run -samples /out/samples -out /out -pass "$PASS" ${ONLY:+-only "$ONLY"}
	log "results: $OUT/$PASS.md"
}

# The success-criteria walk through the API (cmd/measure criteria): CNPJ
# (default 33000167, Petrobras), A (a public office holder) and B (a company)
# for the path. Output: OUT/criteria.txt (counts, kinds, ids; names only of
# companies, public bodies and the two path ends).
criteria() {
	build
	with_cleanup
	measure_tool criteria -cnpj "${CNPJ:-33000167}" -a "${A:-0}" -b "${B:-0}" >"$OUT/criteria.txt"
	log "criteria: $OUT/criteria.txt"
}

stats() {
	f=$OUT/stats.txt
	log "collecting figures into $f"
	{
		echo "# measured $(date -u +%Y-%m-%dT%H:%M:%SZ), commit $(git rev-parse --short HEAD 2>/dev/null || echo ?)"
		printf "SET statement_timeout = '30min';\n%s\n" "
\\echo '## settings in effect'
SELECT name || ' = ' || current_setting(name) FROM pg_settings WHERE name IN ('shared_buffers', 'effective_cache_size',
  'maintenance_work_mem', 'work_mem', 'max_wal_size', 'checkpoint_timeout', 'random_page_cost', 'effective_io_concurrency',
  'max_worker_processes', 'max_parallel_workers', 'max_parallel_workers_per_gather', 'max_parallel_maintenance_workers',
  'max_connections', 'autovacuum_max_workers', 'autovacuum_work_mem', 'autovacuum_vacuum_cost_limit', 'jit', 'wal_compression',
  'server_version') ORDER BY name;
\\echo '## database size (MB)'
SELECT pg_database_size(current_database()) / 1048576;
\\echo '## 20 largest relations (total MB, incl. indexes and TOAST)'
SELECT c.relname || ' ' || pg_total_relation_size(c.oid) / 1048576 FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'm')
  ORDER BY pg_total_relation_size(c.oid) DESC LIMIT 20;
\\echo '## search tables (MB) and state'
SELECT c.relname || ' ' || pg_total_relation_size(c.oid) / 1048576 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = current_schema() AND c.relkind = 'r' AND c.relname LIKE 'search\\_%' ORDER BY 1;
SELECT 'generation ' || generation || ' rebuilt_at ' || coalesce(rebuilt_at::text, '-') || ' requested_at ' || coalesce(requested_at::text, '-')
  || ' rebuilt_from ' || coalesce(rebuilt_from::text, '-') FROM search_state;
\\echo '## entities by kind'
SELECT kind || ' ' || count(*) FROM entity GROUP BY kind ORDER BY kind;
\\echo '## person records by source'
SELECT s.key || ' ' || count(*) FROM person_record p JOIN source s ON s.id = p.source_id GROUP BY s.key ORDER BY s.key;
\\echo '## relationships by type, source and state (open/closed)'
SELECT r.rel_type || ' ' || s.key || ' ' || CASE WHEN r.closed_collection_id IS NULL THEN 'open' ELSE 'closed' END || ' ' || count(*)
  FROM relationship r JOIN collection c ON c.id = r.first_collection_id JOIN source s ON s.id = c.source_id
  GROUP BY r.rel_type, s.key, r.closed_collection_id IS NULL ORDER BY 1;
\\echo '## company versions (all, open)'
SELECT count(*) || ' ' || count(*) FILTER (WHERE closed_collection_id IS NULL) FROM company_version;
\\echo '## identity: person entities, with more than one record, share'
SELECT count(*) || ' ' || count(*) FILTER (WHERE n > 1) || ' ' || round(100.0 * count(*) FILTER (WHERE n > 1) / greatest(count(*), 1), 3) || '%'
  FROM (SELECT entity_id, count(*) AS n FROM person_record WHERE entity_id IS NOT NULL GROUP BY entity_id) t;
\\echo '## identity: person entities with records from more than one source'
SELECT count(*) FROM (SELECT entity_id FROM person_record WHERE entity_id IS NOT NULL GROUP BY entity_id HAVING count(DISTINCT source_id) > 1) t;
\\echo '## identity: proposals by status and reason'
SELECT x || ' | ' || count(*) FROM (SELECT status || ' | ' || regexp_replace(left(reason, 80), '[0-9]+', 'N', 'g') AS x FROM identity_proposal) t GROUP BY x ORDER BY 1;
SELECT 'pending proposals ' || count(*) || ' records ' || coalesce(sum(cardinality(record_ids)), 0) FROM identity_proposal WHERE status = 'pending';
\\echo '## identity: resolve done per collection'
SELECT collection_id || ' ' || coalesce(resolved_at::text, '') FROM identity_resolve_done ORDER BY collection_id;
\\echo '## identity: names by number of distinct CPF fragments (n-bucket): names, records'
WITH n AS (SELECT name_norm, count(DISTINCT cpf_fragment) FILTER (WHERE cpf_fragment <> '') AS n, count(*) AS recs
  FROM person_record GROUP BY name_norm)
SELECT b || ' ' || count(*) || ' ' || sum(recs) FROM (SELECT CASE WHEN n = 0 THEN '0' WHEN n = 1 THEN '1' WHEN n <= 10 THEN '2-10'
  WHEN n <= 50 THEN '11-50' WHEN n <= 100 THEN '51-100' WHEN n <= 300 THEN '101-300' WHEN n <= 1000 THEN '301-1000' ELSE '>1000' END AS b,
  recs FROM n) t GROUP BY b ORDER BY min(CASE b WHEN '0' THEN 0 WHEN '1' THEN 1 WHEN '2-10' THEN 2 WHEN '11-50' THEN 3 WHEN '51-100' THEN 4
  WHEN '101-300' THEN 5 WHEN '301-1000' THEN 6 ELSE 7 END);
\\echo '## identity: groups (name + fragment) with 2+ records: auto-merge vs proposal at thresholds 50/100/300/1000'
WITH n AS (SELECT name_norm, count(DISTINCT cpf_fragment) FILTER (WHERE cpf_fragment <> '') AS n FROM person_record GROUP BY name_norm),
g AS (SELECT p.name_norm, p.cpf_fragment, count(*) AS recs, count(DISTINCT p.source_id) AS srcs FROM person_record p
  WHERE p.cpf_fragment <> '' GROUP BY 1, 2 HAVING count(*) > 1)
SELECT t || ' auto_groups ' || count(*) FILTER (WHERE n.n <= t) || ' auto_records ' || coalesce(sum(g.recs) FILTER (WHERE n.n <= t), 0)
  || ' proposal_groups ' || count(*) FILTER (WHERE n.n > t) || ' proposal_records ' || coalesce(sum(g.recs) FILTER (WHERE n.n > t), 0)
  || ' cross_source_auto_groups ' || count(*) FILTER (WHERE n.n <= t AND g.srcs > 1)
  FROM g JOIN n USING (name_norm) CROSS JOIN (VALUES (50), (100), (300), (1000)) v(t) GROUP BY t ORDER BY t;
\\echo '## identity: expected hidden collisions (two people, same name and fragment) among auto-merged names: sum C(n,2)/space, space 1e5 and 1e6'
WITH n AS (SELECT count(DISTINCT cpf_fragment) FILTER (WHERE cpf_fragment <> '')::numeric AS n FROM person_record GROUP BY name_norm)
SELECT t || ' names_auto ' || count(*) FILTER (WHERE n >= 1 AND n <= t) || ' expected_1e5 ' || round(coalesce(sum(n * (n - 1) / 2) FILTER (WHERE n <= t), 0) / 1e5, 2)
  || ' expected_1e6 ' || round(coalesce(sum(n * (n - 1) / 2) FILTER (WHERE n <= t), 0) / 1e6, 3)
  FROM n CROSS JOIN (VALUES (50), (100), (300), (1000)) v(t) GROUP BY t ORDER BY t;
\\echo '## collections: id source status reference observed started finished duration_s full'
SELECT c.id || ' ' || s.key || ' ' || c.status || ' ' || c.reference_date || ' ' || c.observed_date || ' ' || c.started_at || ' '
  || coalesce(c.finished_at::text, '-') || ' ' || coalesce(round(extract(epoch FROM c.finished_at - c.started_at))::text, '-') || ' '
  || coalesce(c.scope ->> 'full', '?') || ' ' || coalesce(left(c.error, 200), '') FROM collection c JOIN source s ON s.id = c.source_id ORDER BY c.id;
\\echo '## collection stats (counters per collection)'
SELECT c.id || ' ' || s.key || ' ' || coalesce(c.stats::text, '{}') FROM collection c JOIN source s ON s.id = c.source_id ORDER BY c.id;
\\echo '## source_check'
SELECT s.key || ' ' || k.last_checked_at FROM source_check k JOIN source s ON s.id = k.source_id ORDER BY 1;
\\echo '## raw files by source: distinct objects, MB (collection_file)'
SELECT s.key || ' ' || count(DISTINCT f.raw_key) || ' ' || round(sum(f.size_bytes) / 1048576.0) FROM (SELECT DISTINCT ON (raw_key) raw_key, size_bytes, collection_id
  FROM collection_file ORDER BY raw_key, collection_id) f JOIN collection c ON c.id = f.collection_id JOIN source s ON s.id = c.source_id GROUP BY s.key ORDER BY 1;
\\echo '## autovacuum state of the big tables: relname n_live n_dead last_autovacuum last_autoanalyze last_analyze'
SELECT relname || ' ' || n_live_tup || ' ' || n_dead_tup || ' ' || coalesce(last_autovacuum::text, '-') || ' ' || coalesce(last_autoanalyze::text, '-')
  || ' ' || coalesce(last_analyze::text, '-') FROM pg_stat_user_tables WHERE relname IN ('entity', 'person_record', 'relationship', 'company_version')
  ORDER BY relname;
\\echo '## WAL / checkpoints'
SELECT 'checkpoints timed ' || num_timed || ' requested ' || num_requested || ' buffers ' || buffers_written FROM pg_stat_checkpointer;
SELECT 'wal bytes ' || wal_bytes || ' since ' || stats_reset FROM pg_stat_wal;
" | psql_owner
		echo "## identity: resolver counters (worker log, 'identities resolved')"
		dc logs --no-log-prefix worker 2>/dev/null | grep '"identities resolved"' | sed 's/"time":"[^"]*",//' || true
		echo "## docker volumes of the project (docker system df -v)"
		docker system df -v 2>/dev/null | awk -v p="${PROJECT}_" 'index($1, p) == 1 { print $1, $NF }' || true
	} >"$f"
	log "figures: $f"
}

case $STEP in
all)
	samples
	run
	stats
	;;
samples) samples ;;
run) run ;;
stats) stats ;;
criteria) criteria ;;
cleanup)
	with_cleanup
	;;
build) build ;;
*)
	echo "measure.sh: STEP must be all, samples, run, stats, criteria, cleanup or build" >&2
	exit 2
	;;
esac

#!/bin/sh
# Checks the starting conditions of a national load (README, "Carga
# nacional") on a running stack, and changes nothing:
#   1. the settings file has no placeholder secret (scripts/check-env.sh);
#   2. the PostgreSQL settings in effect are those of the documented profile
#      (deploy/TUNING.md, with the PG_* overrides of the settings file);
#   3. the database holds no collection at all (a national load starts from
#      fresh volumes: nothing can be deleted by design), unless
#      ALLOW_EXISTING=1, and in any case none is `running`;
#   4. the worker and API roles log in with the passwords of the settings
#      file and hold (and lack) a few key grants;
#   5. free disk >= NATIONAL_DISK_MIN_GB (default 200: the logical footprint
#      of a national load, README "Disco");
#   6. reports the scheduler setting.
# Prints the settings in effect, to be recorded next to every measurement.
#
#   PROJECT=geoint-national ENV_FILE=.env.national sh scripts/preflight.sh   (make national-check)
set -eu

PROJECT=${PROJECT:-geoint}
ENV_FILE=${ENV_FILE:-.env}
ALLOW_EXISTING=${ALLOW_EXISTING:-}

failures=0
ok() { printf 'national-check: ok    %s\n' "$*"; }
bad() {
	printf 'national-check: FALHA %s\n' "$*" >&2
	failures=$((failures + 1))
}
note() { printf 'national-check:       %s\n' "$*"; }
dc() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f deploy/compose.yml --profile tools "$@"; }
psql_q() {
	printf '%s\n' "$1" | dc exec -T postgres sh -c 'psql -X -q -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
}
env_value() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1; }

note "projeto $PROJECT, configuração $ENV_FILE"
if sh scripts/check-env.sh "$ENV_FILE"; then ok "senhas próprias no $ENV_FILE"; else bad "o $ENV_FILE não serve (acima)"; fi

if ! dc ps --status running --services 2>/dev/null | grep -qx postgres; then
	bad "o PostgreSQL do projeto $PROJECT não está rodando: rode make up DEV_PROJECT=$PROJECT ENV_FILE=$ENV_FILE"
	printf 'national-check: %s problema(s); não comece a carga.\n' "$failures" >&2
	exit 1
fi

# setting NAME VARIABLE DEFAULT KIND: KIND size compares as bytes.
setting() {
	want=$(env_value "$2")
	want=${want:-$3}
	have=$(psql_q "SELECT current_setting('$1')")
	if [ "$4" = size ]; then
		same=$(psql_q "SELECT pg_size_bytes('$want') = pg_size_bytes(current_setting('$1'))") || same=f
	else
		[ "$have" = "$want" ] && same=t || same=f
	fi
	if [ "$same" = t ]; then
		ok "$1 = $have"
	else
		bad "$1 = $have, o perfil pede $want ($2)"
	fi
}
setting shared_buffers PG_SHARED_BUFFERS 8GB size
setting effective_cache_size PG_EFFECTIVE_CACHE_SIZE 20GB size
setting maintenance_work_mem PG_MAINTENANCE_WORK_MEM 2GB size
setting work_mem PG_WORK_MEM 64MB size
setting max_wal_size PG_MAX_WAL_SIZE 32GB size
setting autovacuum_work_mem PG_AUTOVACUUM_WORK_MEM 1GB size
setting checkpoint_timeout PG_CHECKPOINT_TIMEOUT 30min text
setting random_page_cost PG_RANDOM_PAGE_COST 1.1 text
setting effective_io_concurrency PG_EFFECTIVE_IO_CONCURRENCY 200 text
setting max_parallel_workers PG_MAX_PARALLEL_WORKERS 6 text
setting max_parallel_workers_per_gather PG_MAX_PARALLEL_WORKERS_PER_GATHER 4 text
setting max_parallel_maintenance_workers PG_MAX_PARALLEL_MAINTENANCE_WORKERS 4 text
setting max_connections PG_MAX_CONNECTIONS 60 text
setting autovacuum_max_workers PG_AUTOVACUUM_MAX_WORKERS 4 text
for fixed in jit=off fsync=on synchronous_commit=on full_page_writes=on autovacuum=on; do
	name=${fixed%%=*}
	have=$(psql_q "SELECT current_setting('$name')")
	if [ "$have" = "${fixed#*=}" ]; then ok "$name = $have"; else bad "$name = $have, deveria ser ${fixed#*=}"; fi
done
shm=$(dc exec -T postgres df -Pk /dev/shm | awk 'NR == 2 { print int($2 / 1024 / 1024) }')
note "/dev/shm do PostgreSQL: ${shm} GB (PG_SHM_SIZE)"

collections=$(psql_q "SELECT count(*) FROM collection")
running=$(psql_q "SELECT count(*) FROM collection WHERE status = 'running'")
if [ "$collections" = 0 ]; then
	ok "banco sem nenhuma coleta (volumes novos)"
else
	note "coletas já gravadas neste banco:"
	psql_q "SELECT '  ' || c.id || ' ' || s.key || ' ' || c.status || ' ref ' || c.reference_date
		|| CASE WHEN (c.scope ->> 'full')::boolean THEN ' completa' ELSE ' recorte' END
		FROM collection c JOIN source s ON s.id = c.source_id ORDER BY c.id"
	if [ "$ALLOW_EXISTING" = 1 ]; then
		note "ALLOW_EXISTING=1: aceito um banco que já tem coletas (a medição não é de uma primeira carga)"
	else
		bad "o banco já tem $collections coleta(s): uma carga nacional começa em volumes novos (README, \"Carga nacional\")"
	fi
fi
if [ "$running" = 0 ]; then ok "nenhuma coleta em andamento"; else bad "$running coleta(s) com status running"; fi

# Each runtime role: a real login with the password of the settings file
# (sent on standard input, never on a command line) through the container's
# non-loopback address, where pg_hba asks for the password (loopback is
# trusted by the image: a check there passes with any password;
# scripts/role-login.sh), and a few of its grants (has_table_privilege; the
# full matrix is in 0021_roles.sql). A live session is not required: the
# pools close idle connections.
login_as() {
	printf '%s' "$2" | dc exec -T postgres sh -c "$(cat scripts/role-login.sh)" sh "$1" 2>/dev/null
}
check_role() {
	role=$1 pw=$(env_value "$2")
	if [ "$(login_as "$role" "$pw")" = "$role" ]; then ok "$role entra com a senha de $2"; else bad "$role não entra com a senha de $2 (rode make up para aplicá-la)"; fi
	got=$(psql_q "SELECT string_agg(t || ':' || has_table_privilege('$role', t, p)::text, ' ' ORDER BY t, p)
		FROM (VALUES $3) v(t, p)")
	case " $got " in
	*:false*) bad "$role sem a permissão esperada: $got" ;;
	*) ok "$role com as permissões esperadas ($got)" ;;
	esac
	denied=$(psql_q "SELECT count(*) FROM (VALUES $4) v(t, p) WHERE has_table_privilege('$role', t, p)")
	if [ "$denied" = 0 ]; then ok "$role sem permissões que não deveria ter"; else bad "$role tem $denied permissão(ões) que não deveria ter"; fi
}
check_role geoint_worker DB_WORKER_PASSWORD "('relationship', 'INSERT'), ('person_record', 'UPDATE'), ('collection', 'UPDATE')" \
	"('relationship', 'DELETE'), ('entity', 'TRUNCATE'), ('audit_log', 'DELETE')"
check_role geoint_api DB_API_PASSWORD "('entity', 'SELECT'), ('audit_log', 'INSERT'), ('collection_request', 'INSERT')" \
	"('relationship', 'INSERT'), ('relationship', 'DELETE'), ('collection', 'UPDATE')"
owner_conns=$(psql_q "SELECT count(*) FROM pg_stat_activity WHERE usename = current_user AND pid <> pg_backend_pid() AND datname = current_database()")
[ "$owner_conns" = 0 ] || note "há $owner_conns conexão(ões) do dono do banco além desta verificação"

min=$(env_value NATIONAL_DISK_MIN_GB)
if sh scripts/disk-check.sh "${min:-200}"; then ok "espaço em disco"; else bad "espaço em disco abaixo de ${min:-200} GB"; fi

sched=$(env_value SCHEDULER_ENABLED)
case ${sched:-true} in
false) ok "SCHEDULER_ENABLED=false: só coletas pedidas à mão (recomendado para a medição)" ;;
*) note "SCHEDULER_ENABLED=${sched:-vazio (ligado)}: o agendador não inicia a primeira coleta de uma fonte, mas repete as que vencerem; para uma medição controlada use false" ;;
esac
threshold=$(env_value IDENTITY_COMMON_NAME_THRESHOLD)
note "IDENTITY_COMMON_NAME_THRESHOLD=${threshold:-vazio (100)}"
note "fixe a edição da Receita: make collect SOURCE=receita ARGS=\"--ref AAAA-MM-DD\" DEV_PROJECT=$PROJECT ENV_FILE=$ENV_FILE"

if [ "$failures" -gt 0 ]; then
	printf 'national-check: %s problema(s); não comece a carga.\n' "$failures" >&2
	exit 1
fi
printf 'national-check: tudo certo para a carga nacional.\n'

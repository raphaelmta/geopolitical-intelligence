#!/bin/sh
# Restores a backup made by scripts/backup.sh into the volumes of a compose
# project, then leaves the stack for `make up` to start (make restore does).
#
#   PROJECT=geoint ENV_FILE=.env [FORCE=1] sh scripts/restore.sh SRC
#
# 1. Verifies every checksum of SRC/SHA256SUMS; stops at the first mismatch.
# 2. Refuses a project whose database already holds tables, or whose MinIO or
#    Keycloak volume holds files, unless FORCE=1: then it stops the project
#    and DELETES its volumes (pgdata, miniodata, kcdata, natsdata, spool)
#    before restoring. Nothing is deleted without FORCE=1.
# 3. Starts a fresh PostgreSQL, recreates the application roles (NOLOGIN; the
#    next `geoint migrate` gives them LOGIN and the passwords of ENV_FILE),
#    runs pg_restore (owners and grants as in the backup) and compares the
#    row counts with the manifest.
# 4. Unpacks the MinIO and Keycloak archives into their fresh volumes.
# The database owner, the MinIO root user and the Keycloak admin are those of
# ENV_FILE (Keycloak's admin and users come back from the backup itself).
set -eu

src=${1:-}
PROJECT=${PROJECT:-geoint}
ENV_FILE=${ENV_FILE:-.env}
FORCE=${FORCE:-}
ALPINE=alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

log() { printf 'restore: %s\n' "$*" >&2; }
fail() {
	log "FALHOU: $*"
	exit 1
}
dc() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f deploy/compose.yml --profile tools "$@"; }
psql_q() {
	printf '%s\n' "$1" | dc exec -T postgres sh -c 'psql -X -q -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
}
manifest_value() { sed -n "s/^$1: //p" "$src/manifest.txt" | head -n 1; }

[ -n "$src" ] || fail "uso: make restore SRC=<pasta do backup> [FORCE=1]"
[ -d "$src" ] || fail "$src não é uma pasta"
src=$(cd "$src" && pwd)
for f in database.dump miniodata.tar kcdata.tar manifest.txt SHA256SUMS; do
	[ -r "$src/$f" ] || fail "falta $src/$f"
done

log "conferindo as somas SHA-256"
docker run --rm --network none --cap-drop ALL --security-opt no-new-privileges -u "$(id -u):$(id -g)" \
	-v "$src:/b:ro" -w /b "$ALPINE" sha256sum -c SHA256SUMS >&2 || fail "o backup está corrompido ou incompleto"

volume_exists() { docker volume inspect "${PROJECT}_$1" >/dev/null 2>&1; }
volume_has_files() {
	volume_exists "$1" || return 1
	[ -n "$(docker run --rm --network none --cap-drop ALL --cap-add DAC_READ_SEARCH --security-opt no-new-privileges \
		-v "${PROJECT}_$1:/v:ro" "$ALPINE" ls -A /v)" ]
}

occupied=
if volume_exists pgdata; then
	dc up -d --wait postgres >/dev/null 2>&1 || fail "não consegui ligar o PostgreSQL do projeto $PROJECT"
	tables=$(psql_q "SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')")
	[ "$tables" = 0 ] || occupied="$occupied banco($tables tabelas)"
fi
for v in miniodata kcdata; do
	if volume_has_files "$v"; then occupied="$occupied $v"; fi
done
if [ -n "$occupied" ]; then
	if [ "$FORCE" != 1 ]; then
		fail "o projeto $PROJECT já tem dados:$occupied. Para restaurar por cima, APAGANDO esses dados, use FORCE=1"
	fi
	log "FORCE=1: parando o projeto $PROJECT e apagando os volumes dele"
	dc down --remove-orphans >/dev/null 2>&1
	for v in pgdata miniodata kcdata natsdata spool; do
		if volume_exists "$v"; then docker volume rm "${PROJECT}_$v" >/dev/null; fi
	done
fi

log "ligando um PostgreSQL novo"
dc up -d --wait postgres >/dev/null
psql_q "DO \$\$ BEGIN
	IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'geoint_worker') THEN CREATE ROLE geoint_worker NOLOGIN; END IF;
	IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'geoint_api') THEN CREATE ROLE geoint_api NOLOGIN; END IF;
END \$\$" >/dev/null
log "pg_restore"
dc exec -T postgres sh -c 'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --exit-on-error' <"$src/database.dump" ||
	fail "pg_restore falhou (mensagem acima)"
for pair in "coletas:collection" "entidades:entity" "vínculos:relationship" "decisões de identidade:identity_decision" "auditoria:audit_log"; do
	label=${pair%%:*}
	table=${pair#*:}
	want=$(manifest_value "$label")
	got=$(psql_q "SELECT count(*) FROM $table")
	[ "$want" = "$got" ] || fail "$label: o backup diz $want, o banco restaurado tem $got"
	log "$label: $got (confere com o manifesto)"
done

# unpack VOLUME: into the fresh volume (created by compose, so it carries the
# project's labels), as root with only the capabilities tar needs to restore
# owners and times, no network.
unpack() {
	docker run --rm -i --network none --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
		--security-opt no-new-privileges -v "${PROJECT}_$1:/v" "$ALPINE" tar -C /v -xpf - <"$src/$1.tar" ||
		fail "não consegui restaurar $1"
}
dc create minio keycloak >/dev/null 2>&1
log "arquivos brutos (MinIO)"
unpack miniodata
log "Keycloak"
unpack kcdata
log "pronto. Próximo passo: make up (migra, dá às funções do banco as senhas do $ENV_FILE e liga tudo)"

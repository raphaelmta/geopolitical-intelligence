#!/bin/sh
# Backs up what cannot be collected again, into a NEW folder:
#   database.dump   pg_dump -Fc of the database (claims and their history,
#                   identity decisions, audit trail, collections, jobs);
#   roles.sql       the cluster's roles, without passwords (reference only:
#                   restore.sh recreates the two application roles itself);
#   miniodata.tar   the raw-file store (MinIO volume): the evidence;
#   kcdata.tar      Keycloak's embedded store (users, realm, keys);
#   manifest.txt    date, git commit, versions, row counts, sizes;
#   SHA256SUMS      checksums of every file above.
# Not backed up: NATS (the queue holds only pending requests and job
# messages, disposable: a lost request is asked again) and the download
# spool (temporary).
#
#   PROJECT=geoint ENV_FILE=.env sh scripts/backup.sh DEST      (make backup DEST=...)
#
# api, worker (so no load runs), keycloak and minio are stopped during the
# copy, for a consistent snapshot, and the ones that were running are
# started again at the end, also on failure. The folder and files are
# private to the current user (0700/0600): the backup holds personal data
# as published by the sources (raw files with full CPFs where the source
# publishes them) and the Keycloak users. Keep it encrypted and offline.
set -eu

dest=${1:-}
PROJECT=${PROJECT:-geoint}
ENV_FILE=${ENV_FILE:-.env}
ALPINE=alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

log() { printf 'backup: %s\n' "$*" >&2; }
fail() {
	log "FALHOU: $*"
	exit 1
}
dc() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f deploy/compose.yml --profile tools "$@"; }
psql_q() {
	printf '%s\n' "$1" | dc exec -T postgres sh -c 'psql -X -q -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
}

[ -n "$dest" ] || fail "uso: make backup DEST=<pasta nova>"
if [ -e "$dest" ] && [ -n "$(ls -A "$dest" 2>/dev/null)" ]; then
	fail "$dest já existe e não está vazia; escolha uma pasta nova"
fi
umask 077
mkdir -p "$dest"
chmod 0700 "$dest"
dest=$(cd "$dest" && pwd)

running=$(dc ps --status running --services 2>/dev/null || true)
printf '%s\n' "$running" | grep -qx postgres || fail "o PostgreSQL do projeto $PROJECT não está rodando (rode make up antes)"

restart=
for s in api worker keycloak minio; do
	if printf '%s\n' "$running" | grep -qx "$s"; then
		restart="$restart $s"
	fi
done
finish() {
	status=$?
	if [ -n "$restart" ]; then
		log "religando:$restart"
		# shellcheck disable=SC2086 # list of service names
		dc up -d --no-deps $restart >/dev/null 2>&1 || log "não consegui religar$restart: rode make up"
	fi
	[ "$status" -eq 0 ] || log "o backup em $dest está INCOMPLETO; apague a pasta e tente de novo"
	exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log "parando api, worker, keycloak e minio (nenhuma carga roda durante o backup)"
dc stop api worker keycloak minio >/dev/null 2>&1

log "banco: pg_dump"
dc exec -T postgres sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' >"$dest/database.dump"
dc exec -T postgres sh -c 'pg_dumpall -U "$POSTGRES_USER" --roles-only --no-role-passwords' >"$dest/roles.sql"

# archive VOLUME: tar of a named volume, mounted read-only in a throwaway
# container with no network; written by this shell (owned by the current user).
archive() {
	docker volume inspect "${PROJECT}_$1" >/dev/null 2>&1 || fail "o volume ${PROJECT}_$1 não existe"
	docker run --rm --network none --cap-drop ALL --cap-add DAC_READ_SEARCH --security-opt no-new-privileges \
		-v "${PROJECT}_$1:/v:ro" "$ALPINE" tar -C /v -cf - . >"$dest/$1.tar"
}
log "arquivos brutos (MinIO)"
archive miniodata
log "Keycloak"
archive kcdata

commit=$(git rev-parse HEAD 2>/dev/null || echo desconhecido)
if [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then
	commit="$commit (com mudanças locais)"
fi
{
	printf 'backup da plataforma geoint\n'
	printf 'data (UTC): %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	printf 'projeto: %s\n' "$PROJECT"
	printf 'commit: %s\n' "$commit"
	printf 'geoint: %s\n' "$(dc run --rm --no-deps -T migrate version 2>/dev/null || echo desconhecido)"
	printf 'postgres: %s\n' "$(psql_q 'SELECT version()')"
	printf 'migração: %s\n' "$(psql_q 'SELECT max(version_id) FROM goose_db_version WHERE is_applied')"
	printf 'coletas: %s\n' "$(psql_q 'SELECT count(*) FROM collection')"
	printf 'entidades: %s\n' "$(psql_q 'SELECT count(*) FROM entity')"
	printf 'vínculos: %s\n' "$(psql_q 'SELECT count(*) FROM relationship')"
	printf 'decisões de identidade: %s\n' "$(psql_q 'SELECT count(*) FROM identity_decision')"
	printf 'auditoria: %s\n' "$(psql_q 'SELECT count(*) FROM audit_log')"
	printf 'imagens:\n'
	dc config --images 2>/dev/null | sed 's/^/  /'
	printf 'arquivos (bytes):\n'
	for f in database.dump roles.sql miniodata.tar kcdata.tar; do
		printf '  %s %s\n' "$f" "$(wc -c <"$dest/$f" | tr -d ' ')"
	done
} >"$dest/manifest.txt"

docker run --rm --network none --cap-drop ALL --security-opt no-new-privileges -u "$(id -u):$(id -g)" \
	-v "$dest:/b" -w /b "$ALPINE" sh -c 'sha256sum database.dump roles.sql miniodata.tar kcdata.tar manifest.txt >SHA256SUMS'
chmod 0600 "$dest"/*
log "pronto: $dest ($(du -sh "$dest" | cut -f1)). Contém dados pessoais publicados pelas fontes: guarde com cuidado."

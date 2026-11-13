#!/bin/sh
# Refuses (exit 1, message in Portuguese) a settings file the stack must not
# start with: a public placeholder (change-me…) still in place, a missing or
# empty secret, or a password with characters that would break a connection
# URL. Used by `make up`, `make migrate` and scripts/preflight.sh.
#
#   sh scripts/check-env.sh [FILE]      (default .env)
#
# Never prints a value.
set -eu

file=${1:-.env}
secrets="POSTGRES_PASSWORD DB_WORKER_PASSWORD DB_API_PASSWORD MINIO_ROOT_PASSWORD KEYCLOAK_ADMIN_PASSWORD"
names="POSTGRES_USER POSTGRES_DB MINIO_ROOT_USER S3_BUCKET KEYCLOAK_ADMIN"

if [ ! -r "$file" ]; then
	printf 'check-env: %s não existe. Crie-o com: sh scripts/init-env.sh\n' "$file" >&2
	exit 1
fi

# value KEY: the last KEY=… line of the file (as docker compose reads it).
value() {
	sed -n "s/^$1=//p" "$file" | tail -n 1
}

problems=0
problem() {
	printf 'check-env: %s\n' "$*" >&2
	problems=$((problems + 1))
}

placeholders=$(sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*change-me.*$/\1/p' "$file" | tr '\n' ' ')
if [ -n "$placeholders" ]; then
	problem "$file ainda tem senhas de exemplo (change-me…) em: $placeholders"
	problem "  essas senhas são públicas. Veja \"Trocar senhas\" no README: numa instalação nova basta"
	problem "  apagar o .env e rodar make up de novo; numa que já subiu, troque cada senha como o README explica."
fi

missing=
for key in $secrets $names; do
	if ! grep -q "^$key=." "$file"; then
		missing="$missing $key"
	fi
done
if [ -n "$missing" ]; then
	problem "faltam no $file:$missing"
	problem "  para acrescentar as senhas que faltam (valores aleatórios, nada mais muda): sh scripts/init-env.sh --add-missing $file"
fi

for key in $secrets; do
	v=$(value "$key")
	[ -n "$v" ] || continue
	case $v in
	*change-me*) continue ;;
	*[!A-Za-z0-9._~-]*)
		problem "$key tem caracteres fora de A-Z a-z 0-9 . _ ~ - (a senha vai dentro de um endereço de conexão)"
		continue
		;;
	esac
	if [ "${#v}" -lt 16 ]; then
		problem "$key tem menos de 16 caracteres"
	fi
done

if [ "$problems" -gt 0 ]; then
	printf 'check-env: a pilha não foi iniciada.\n' >&2
	exit 1
fi

#!/bin/sh
# Creates the stack's settings file (.env) from .env.example, replacing every
# placeholder value (a value starting with "change-me") with a new random
# secret: 32 bytes from /dev/urandom in URL-safe base64 without "/", "+" or
# "=", so it can sit inside a connection URL as is.
#
#   sh scripts/init-env.sh [TARGET] [EXAMPLE]          (defaults: .env .env.example)
#   sh scripts/init-env.sh --add-missing [TARGET] [EXAMPLE]
#
# Never overwrites an existing TARGET. --add-missing (for a .env created by an
# older version) appends, with new random values, only the secret keys of
# EXAMPLE that TARGET lacks, and changes nothing else. The file is created
# with mode 0600. No secret is ever printed.
set -eu

add_missing=0
if [ "${1:-}" = "--add-missing" ]; then
	add_missing=1
	shift
fi
target=${1:-.env}
example=${2:-.env.example}

fail() {
	printf 'init-env: %s\n' "$*" >&2
	exit 1
}

[ -r "$example" ] || fail "não encontrei $example"
[ -r /dev/urandom ] || fail "/dev/urandom não está disponível"
command -v base64 >/dev/null 2>&1 || fail "o comando base64 não está instalado"

umask 077

# secret: one new random value (43 characters, A-Z a-z 0-9 - _).
secret() {
	v=$(dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr '+/' '-_' | tr -d '=\n')
	[ "${#v}" -ge 40 ] || fail "não consegui gerar um valor aleatório"
	printf '%s' "$v"
}

# placeholder_keys FILE: the keys whose value starts with change-me.
placeholder_keys() {
	sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=change-me.*$/\1/p' "$1"
}

if [ "$add_missing" = 1 ]; then
	[ -f "$target" ] || fail "$target não existe; rode sem --add-missing para criá-lo"
	added=
	for key in $(placeholder_keys "$example"); do
		if ! grep -q "^$key=" "$target"; then
			printf '%s=%s\n' "$key" "$(secret)" >>"$target"
			added="$added $key"
		fi
	done
	chmod 0600 "$target"
	if [ -n "$added" ]; then
		printf 'init-env: acrescentei a %s, com valores aleatórios:%s\n' "$target" "$added"
	else
		printf 'init-env: %s já tem todas as senhas\n' "$target"
	fi
	exit 0
fi

if [ -e "$target" ]; then
	printf 'init-env: %s já existe; nada foi mudado\n' "$target"
	exit 0
fi

dir=$(dirname -- "$target")
tmp=$(mktemp "$dir/.env-init.XXXXXX")
trap 'rm -f "$tmp"' EXIT
while IFS= read -r line || [ -n "$line" ]; do
	case $line in
	[A-Za-z_]*=change-me*)
		printf '%s=%s\n' "${line%%=*}" "$(secret)"
		;;
	*)
		printf '%s\n' "$line"
		;;
	esac
done <"$example" >"$tmp"
if grep -q '=change-me' "$tmp"; then
	fail "sobrou um valor de exemplo em $tmp"
fi
chmod 0600 "$tmp"
# ln fails if TARGET appeared meanwhile: never overwrites.
ln "$tmp" "$target" 2>/dev/null || fail "$target apareceu durante a criação; nada foi mudado"
printf 'init-env: %s criado com senhas aleatórias; a senha do administrador do Keycloak está nesse arquivo (KEYCLOAK_ADMIN_PASSWORD)\n' "$target"

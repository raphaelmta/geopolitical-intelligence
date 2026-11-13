#!/bin/sh
# Runs INSIDE the postgres container (scripts/preflight.sh pipes it to `sh -c`):
# logs in as role $1 to database $2 (default $POSTGRES_DB) with the password
# read from standard input, and prints current_user. It connects through the
# container's own non-loopback address: the image's pg_hba.conf trusts local
# sockets and 127.0.0.1/::1 (any password passes there), while every other
# address needs the password (scram-sha-256), so only this proves the
# password. Without a non-loopback address it refuses (exit 2) instead of
# passing.
set -eu
addr=
for a in $(hostname -i 2>/dev/null); do
	case $a in
	127.* | ::1 | "") ;;
	*)
		addr=$a
		break
		;;
	esac
done
if [ -z "$addr" ]; then
	echo "role-login: the container has no non-loopback address; cannot check a password" >&2
	exit 2
fi
PGPASSWORD=$(cat)
export PGPASSWORD
exec psql -X -q -A -t -w -h "$addr" -U "$1" -d "${2:-$POSTGRES_DB}" -c 'SELECT current_user'

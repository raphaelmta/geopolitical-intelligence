#!/bin/sh
# Prints the free space of the file system where Docker keeps volumes and
# images, and warns (exit 1) when it is below a minimum in GB.
#
#   sh scripts/disk-check.sh [MIN_GB]
#
# MIN_GB: the argument, else DISK_WARN_GB from the environment or from the
# settings file (ENV_FILE, default .env), else 200 (a national install: about
# 165 GB of logical data at the peak, README "Disco").
# Changes nothing. When the Docker data folder cannot be read from the host
# (Docker Desktop, rootless setups), it says so and exits 0.
set -eu

env_file=${ENV_FILE:-.env}
min=${1:-${DISK_WARN_GB:-}}
if [ -z "$min" ] && [ -r "$env_file" ]; then
	min=$(sed -n 's/^DISK_WARN_GB=//p' "$env_file" | tail -n 1)
fi
min=${min:-200}
case $min in
"" | *[!0-9]*)
	printf 'disk-check: mínimo inválido: %s (use um número inteiro de GB)\n' "$min" >&2
	exit 2
	;;
esac

root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)
avail=
if [ -n "$root" ]; then
	avail=$(df -Pk "$root" 2>/dev/null | awk 'NR == 2 { print $4 }') || avail=
fi
case $avail in
"" | *[!0-9]*)
	printf 'disk-check: não consegui medir o espaço livre do Docker (%s); veja com: docker system df -v\n' "${root:-?}"
	exit 0
	;;
esac
gb=$((avail / 1024 / 1024))
printf 'disk-check: %s GB livres em %s (onde o Docker guarda volumes e imagens)\n' "$gb" "$root"
if [ "$gb" -lt "$min" ]; then
	printf 'disk-check: AVISO: menos de %s GB livres. Uma carga nacional precisa de ~165 GB no pico, sem compressão (README, "Disco").\n' "$min" >&2
	printf 'disk-check: com o disco cheio o PostgreSQL para de gravar; veja o uso com: docker system df -v\n' >&2
	exit 1
fi

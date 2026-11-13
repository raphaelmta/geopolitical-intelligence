#!/bin/sh
# Gives each data volume to the unprivileged user its service runs as
# (deploy/compose.yml, service volume-init, at every `make up`):
#   sh volume-init.sh DIR UID [DIR UID ...]
# Only files with another owner are changed, so a volume created by an older,
# root-running stack is converted once and later runs change nothing. A
# later `make up` runs this while the data services are running, when files
# come and go during the walk (WAL recycling, temporary files): a file that
# vanishes before its chown is not an error. What counts is a second walk
# finding nothing left with another owner (a file created meanwhile belongs
# to the service already). The volume's root is changed last, only then.
# Runs as root with CHOWN and DAC_READ_SEARCH (busybox find, stat, chown).
set -eu

# Called as `fix … || status=1`, where `set -e` does not apply: every step
# that matters returns 1 on failure explicitly.
fix() {
	dir=$1 id=$2
	if [ ! -d "$dir" ]; then
		printf 'volume-init: %s is not a directory\n' "$dir" >&2
		return 1
	fi
	find "$dir" -xdev -mindepth 1 \( ! -user "$id" -o ! -group "$id" \) -exec chown -h "$id:$id" {} + 2>/dev/null || true
	left=$(find "$dir" -xdev -mindepth 1 \( ! -user "$id" -o ! -group "$id" \) 2>/dev/null | head -n 1)
	if [ -n "$left" ]; then
		printf 'volume-init: %s still has files not owned by %s (first: %s)\n' "$dir" "$id" "$left" >&2
		return 1
	fi
	owner=$(stat -c %u:%g "$dir") || return 1
	if [ "$owner" != "$id:$id" ]; then
		chown -h "$id:$id" "$dir" || return 1
		printf 'volume-init: %s given to %s\n' "$dir" "$id"
	fi
}

[ $(($# % 2)) -eq 0 ] && [ $# -gt 0 ] || {
	echo "usage: volume-init.sh DIR UID [DIR UID ...]" >&2
	exit 2
}
status=0
while [ $# -gt 0 ]; do
	fix "$1" "$2" || status=1
	shift 2
done
exit $status

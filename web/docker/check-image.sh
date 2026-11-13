#!/bin/sh
# Checks a built web image: the bundle's index.html is CSP-clean, the start-up
# config script passes its tests (inside the image, with its busybox shell),
# a bad setting stops the container, and a running container serves the
# security headers and /config.json.
# Usage: check-image.sh IMAGE CONTAINER_NAME
set -eu
image=$1
name=$2
here=$(cd "$(dirname "$0")" && pwd)
fail() { echo "check-image: $*" >&2; exit 1; }

cleanup() {
    docker rm -f "$name" "$name-bad" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

# --- index.html: whole file on one line, so a tag split across lines counts.
index=$(docker run --rm --entrypoint cat "$image" /usr/share/nginx/html/index.html | tr '\n\r\t' '   ')
tags=$(printf '%s' "$index" | grep -Eio '<script[^>]*>' || true)
[ -n "$tags" ] || fail "index.html has no script"
printf '%s\n' "$tags" | while IFS= read -r tag; do
    printf '%s' "$tag" | grep -Eiq '[[:space:]]src=' || { echo "check-image: <script> without src: $tag" >&2; exit 1; }
done || exit 1
if printf '%s' "$index" | grep -Eiq '<style'; then fail "<style> in index.html"; fi
if printf '%s' "$index" | grep -Eiq '[[:space:]]style[[:space:]]*='; then fail "style attribute in index.html"; fi
echo "check-image: index.html has no inline script or style"

# --- built JS: constructs the CSP blocks (inline styles, eval) may only
# appear the number of times known and reviewed below. A dependency upgrade
# that adds one fails here until it is reviewed and this list is updated.
js=$(docker run --rm --entrypoint sh "$image" -c 'cat /usr/share/nginx/html/assets/*.js')
[ -n "$js" ] || fail "no built JS found"
q='["'\''`]'
check_count() { # pattern allowed why
    n=$(printf '%s' "$js" | grep -Eo -- "$1" | wc -l | tr -d ' ')
    [ "$n" -le "$2" ] || fail "built JS has $n× /$1/ (allowed $2: $3)"
}
check_count "setAttribute\\([[:space:]]*${q}style${q}" 0 "no style attributes"
# "style" as the second argument, after a variable: setAttribute(n, "style").
check_count "setAttribute\\([A-Za-z_\$][A-Za-z0-9_\$]*,[[:space:]]*${q}style${q}" 0 "no style attribute through a variable"
check_count 'cssText' 0 "no style text"
check_count 'insertRule' 0 "no stylesheet rules"
check_count '(^|[^A-Za-z0-9_$.])eval\(' 0 "no eval"
check_count 'new Function\(' 0 "no new Function"
# Indirect eval: (0,eval)(…), globalThis.eval, window["eval"].
check_count '\(0,[[:space:]]*eval\)' 0 "no indirect eval"
check_count '(globalThis|window|self)\.eval' 0 "no global eval"
check_count "\\[${q}eval${q}\\]" 0 "no eval by name"
# 1: lodash's global-object fallback inside cytoscape,
#    freeGlobal || freeSelf || Function('return this')(); never reached in a
#    browser (self is defined).
check_count "Function\\(${q}" 1 "lodash global fallback in cytoscape"
# 2: React's hoistable <style precedence> (the app renders none), and
#    cytoscape's renderer stylesheet, skipped because graphAdapter.ts puts an
#    element with its id (__________cytoscape_stylesheet) in <head> first.
check_count "createElement\\(${q}style${q}\\)" 2 "React hoistable style; cytoscape (placeholder guard)"
# Per chunk: at most one, and only in the chunk that holds React's
# hoistable-style code (data-precedence) or cytoscape's guarded stylesheet.
docker run --rm --entrypoint sh "$image" -c '
    cd /usr/share/nginx/html/assets || exit 1
    for f in *.js; do
        n=$(grep -Eo "createElement\(.style.\)" "$f" | wc -l)
        [ "$n" -eq 0 ] && continue
        [ "$n" -le 1 ] || { echo "$f: $n createElement(style)"; exit 1; }
        grep -q -e data-precedence -e __________cytoscape_stylesheet "$f" ||
            { echo "$f: createElement(style) outside the React and cytoscape chunks"; exit 1; }
    done' >&2 || fail "createElement(style) per chunk (above)"
printf '%s' "$js" | grep -q '__________cytoscape_stylesheet' || fail "cytoscape stylesheet id not found in the bundle"
echo "check-image: built JS has no new inline-style or eval constructs"

# --- start-up config script, tested with the image's own shell and tools.
docker run --rm -v "$here:/t:ro" --entrypoint sh "$image" /t/web-config.test.sh /docker-entrypoint.d/05-web-config.sh

# --- an invalid setting stops the container (bounded wait: never hangs).
docker run -d --name "$name-bad" -e "WEB_OIDC_AUTHORITY=http://localhost:8081
; script-src *" "$image" >/dev/null
i=0
while [ "$(docker inspect -f '{{.State.Running}}' "$name-bad")" = true ]; do
    i=$((i + 1))
    [ "$i" -lt 15 ] || fail "container still running with an invalid WEB_OIDC_AUTHORITY"
    sleep 1
done
[ "$(docker inspect -f '{{.State.ExitCode}}' "$name-bad")" != 0 ] || fail "invalid setting exited 0"
echo "check-image: invalid setting refused at start"

# --- running container: headers, /config.json, routes.
docker run -d --rm --name "$name" -p 127.0.0.1::5173 "$image" >/dev/null
port=$(docker port "$name" 5173/tcp | head -n1 | sed 's/.*://')
base="http://127.0.0.1:$port"
i=0
until curl -fsS -o /dev/null "$base/config.json" 2>/dev/null; do
    i=$((i + 1)); [ "$i" -lt 30 ] || fail "container did not answer"; sleep 1
done
headers=$(curl -fsSI "$base/")
for h in \
    "Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'" \
    "connect-src 'self' http://localhost:8080 http://localhost:8081;" \
    "frame-ancestors 'none'" \
    'X-Content-Type-Options: nosniff' \
    'Referrer-Policy: no-referrer' \
    'X-Frame-Options: DENY' \
    'Permissions-Policy: camera=(), microphone=(), geolocation=()'; do
    printf '%s' "$headers" | grep -qiF -- "$h" || fail "missing header: $h"
done
config=$(curl -fsS "$base/config.json")
[ "$config" = '{"apiBaseUrl":"http://localhost:8080","oidcAuthority":"http://localhost:8081/realms/geoint","oidcClientId":"geoint-web"}' ] ||
    fail "unexpected /config.json: $config"
curl -fsSI "$base/config.json" | grep -qi '^content-type: application/json' || fail "config.json content type"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$base/entidade/12")" = 200 ] || fail "SPA fallback"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$base/assets/missing.js")" = 404 ] || fail "missing asset is not 404"
echo "check-image: headers, /config.json and routes ok"

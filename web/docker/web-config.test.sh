#!/bin/sh
# Tests web-config.sh. Usage: web-config.test.sh [path/to/web-config.sh]
# make check-web-image runs it inside the final image, with that image's
# shell and tools (busybox ash, as the entrypoint runs it).
set -u
script=${1:-$(dirname "$0")/web-config.sh}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0
n=0

API_OK=http://localhost:8080
IDP_OK=http://localhost:8081/realms/geoint
CLIENT_OK=geoint-web

# run API AUTHORITY CLIENT: renders into a fresh empty directory $out.
run() {
    n=$((n + 1))
    out=$tmp/run$n
    mkdir "$out"
    env -i PATH="$PATH" WEB_CONF_OUT="$out/default.conf" WEB_CONFIG_OUT="$out/config.json" \
        WEB_API_URL="$1" WEB_OIDC_AUTHORITY="$2" WEB_OIDC_CLIENT_ID="$3" \
        sh "$script" >"$tmp/log" 2>&1
}

problem() {
    echo "FAIL: $*"
    failures=$((failures + 1))
}

ok() {
    run "$@" || { problem "expected success for: $*"; cat "$tmp/log"; }
}

# refused LABEL API AUTHORITY CLIENT: exit != 0 and no file left behind.
refused() {
    label=$1
    shift
    if run "$@"; then
        problem "expected refusal: $label"
    elif [ -n "$(ls -A "$out")" ]; then
        problem "files left behind after refusal: $label: $(ls -A "$out")"
    fi
}

same() {
    if [ "$1" != "$2" ]; then
        problem "$3"
        printf '  want: %s\n  got:  %s\n' "$2" "$1"
    fi
}

csp_line() {
    grep 'Content-Security-Policy' "$1"
}

# --- valid values, exact output ---------------------------------------------

ok "$API_OK" "$IDP_OK" "$CLIENT_OK"
same "$(cat "$out/config.json")" '{"apiBaseUrl":"http://localhost:8080","oidcAuthority":"http://localhost:8081/realms/geoint","oidcClientId":"geoint-web"}' "config.json (default)"
[ "$(wc -c <"$out/config.json")" -eq 121 ] || problem "config.json size: $(wc -c <"$out/config.json")"
same "$(csp_line "$out/default.conf")" "    add_header Content-Security-Policy \"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self' http://localhost:8080 http://localhost:8081; frame-src 'none'; worker-src 'none'; object-src 'none'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'\" always;" "CSP line (default)"
same "$(grep -c '^ *add_header' "$out/default.conf")" 7 "header count"
same "$(grep -c '^ *location' "$out/default.conf")" 3 "location count"
# The access log never holds a query string (a CPF typed in a link's ?q=).
same "$(grep -c '^ *access_log /var/log/nginx/access.log geoint_noquery;$' "$out/default.conf")" 1 "access_log without query"
grep -q "^log_format geoint_noquery .*\$uri" "$out/default.conf" || problem "log_format logs \$uri"
! grep -q '\$request\b\|\$args\|\$query_string\|\$request_uri\|\$http_referer' "$out/default.conf" || problem "a log variable with the query"

ok https://api.example.org/ https://id.example.org:8443/realms/x/ web.client_1
same "$(cat "$out/config.json")" '{"apiBaseUrl":"https://api.example.org","oidcAuthority":"https://id.example.org:8443/realms/x","oidcClientId":"web.client_1"}' "config.json (port, trailing slash)"
csp_line "$out/default.conf" | grep -qF "connect-src 'self' https://api.example.org https://id.example.org:8443;" || problem "CSP origins (port)"

ok 'http://[::1]:8080/api' 'http://127.0.0.1:8081/realms/r' c
csp_line "$out/default.conf" | grep -qF "connect-src 'self' http://[::1]:8080 http://127.0.0.1:8081;" || problem "CSP origins (IPv6)"

# Hosts and ports at the edge of the stricter rules: still accepted.
for good in 'http://a-b.example:1' 'http://x.example:65535' 'http://1.2.3.4' 'http://[2001:db8::1]:443' \
    'http://[::ffff:192.0.2.1]' 'http://[1:2:3:4:5:6:7:8]' 'http://[::]:80' 'http://localhost' \
    "http://$(printf 'a%.0s' $(seq 63)).example"; do
    ok "$good" "$IDP_OK" "$CLIENT_OK"
done

# --- refused -------------------------------------------------------------------

nl='
'
cr=$(printf '\r')
tab=$(printf '\t')
nul_like=$(printf '\001')

# Reviewer's exploits: an nginx location injected through continuation lines,
# and "script-src *" smuggled into the CSP after a valid first line.
exploit_location=$(printf 'k\\\n\\" always; location /pwn { return 200 owned; } add_header X \\"\\\nhttp://localhost:8081/realms/geoint')
exploit_csp=$(printf "http://localhost:8081/realms/geoint\n; script-src *; x 'unsafe-inline'")
exploit_csp2=$(printf 'http://x; script-src *\nhttp://localhost:8081')
for v in "$exploit_location" "$exploit_csp" "$exploit_csp2"; do
    refused "exploit as authority" "$API_OK" "$v" "$CLIENT_OK"
    refused "exploit as api" "$v" "$IDP_OK" "$CLIENT_OK"
done

for bad in \
    "http://localhost:8080${nl}" \
    "http://localhost:8080${nl}http://evil" \
    "http://localhost:8080${cr}" \
    "http://localhost${tab}:8080" \
    "http://localhost:8080${nul_like}" \
    'http://localhost:8080/a\b' \
    'http://localhost:8080/a"b' \
    "http://localhost:8080/a'b" \
    'http://local host:8080' \
    'http://localhost:8080/a;b' \
    'http://localhost:8080/#f' \
    'http://localhost:8080/a&b' \
    'http://localhost:8080/$(id)' \
    'http://localhost:8080/`id`' \
    'http://localhost:8080/?q=1' \
    'http://localhost:8080/a*b' \
    'http://localhost:8080/{a}' \
    'http://localhost:8080/<a>' \
    'http://localhost:8080/a|b' \
    'http://localhost:8080/a,b' \
    'http://user@localhost:8080' \
    'http://localhost:99999x' \
    'http://localhost:123456' \
    'http://[::1' \
    'http://[]' \
    'http://[zz]:1' \
    'http://.evil' \
    'http://h-' \
    'http://h-.example' \
    'http://-h.example' \
    'http://a.-b.example' \
    'http://h..x' \
    'http://h.' \
    "http://$(printf 'a%.0s' $(seq 64)).example" \
    'http://999.1.1.1' \
    'http://1.2.3' \
    'http://01.2.3.4' \
    'http://1.2.3.4.5' \
    'http://localhost:0' \
    'http://localhost:65536' \
    'http://localhost:99999' \
    'http://localhost:080' \
    'http://localhost:' \
    'http://[1:2:3:4:5:6:7:8:9]' \
    'http://[1::2::3]' \
    'http://[12345::1]' \
    'http://[:1]' \
    'http://[1:]' \
    'http://[1:::2]' \
    'http://[1.2.3.4::1]' \
    'http://[::1.2.3]' \
    'http://[::1.2.3.4:5]' \
    'http://[1:2:3:4:5:6:7]' \
    'http://[fe80::1%25eth0]' \
    'http://[::1]x' \
    'javascript:alert(1)' \
    'ftp://a.example' \
    'http://' \
    'localhost:8080' \
    ''; do
    refused "api: $bad" "$bad" "$IDP_OK" "$CLIENT_OK"
    refused "authority: $bad" "$API_OK" "$bad" "$CLIENT_OK"
done
long="http://a.example/$(printf '%0300d' 0)"
refused "too long" "$long" "$IDP_OK" "$CLIENT_OK"

for bad in '' 'a b' 'a"b' 'a;b' "a'b" 'a\b' "a${nl}b" 'a$b' "$(printf '%0101d' 0)"; do
    refused "client: $bad" "$API_OK" "$IDP_OK" "$bad"
done

if [ "$failures" -ne 0 ]; then
    echo "web-config tests: $failures failure(s) in $n runs"
    exit 1
fi
echo "web-config tests: ok ($n runs)"

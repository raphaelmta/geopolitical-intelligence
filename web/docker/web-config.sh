#!/bin/sh
# Validates the web app's public settings and renders, before nginx starts:
#   - /config.json (WEB_API_URL, WEB_OIDC_AUTHORITY, WEB_OIDC_CLIENT_ID);
#   - the nginx server block, with the CSP connect-src limited to the origins
#     (scheme://host[:port]) of the API and of the identity provider.
#
# Every check is on the whole value (shell `case` patterns, never a per-line
# tool): a strict character allow-list first (which rules out newlines and
# every other control character, quotes, spaces, `;`, `\`, `#`, `&`, `$`…),
# then the URL shape. Values are never passed through sed/awk or a template:
# both files are written with printf and fixed text, to temporary files that
# are moved into place only when everything succeeded. Any invalid value
# stops the container (the image's entrypoint runs this under set -e).
# Output paths can be overridden for tests.
set -eu

conf_out=${WEB_CONF_OUT:-/etc/nginx/conf.d/default.conf}
json_out=${WEB_CONFIG_OUT:-/etc/nginx/web-runtime/config.json}
MAX=300

fail() {
    echo "web-config: $*" >&2
    exit 1
}

# check_ipv4 VALUE: four decimal octets 0-255, no leading zeros.
check_ipv4() {
    a=$1
    i=0
    while :; do
        o=${a%%.*}
        case $o in
        "" | *[!0-9]* | 0?*) return 1 ;;
        esac
        [ "${#o}" -le 3 ] && [ "$o" -le 255 ] || return 1
        i=$((i + 1))
        [ "$o" = "$a" ] && break
        a=${a#*.}
    done
    [ "$i" -eq 4 ]
}

# check_ipv6 VALUE (inside the brackets): RFC 4291 text form, hex groups of
# at most 4 digits, at most one "::", 8 groups (fewer with "::"), an optional
# IPv4 tail; no zone index.
check_ipv6() {
    a=$1
    case $a in
    "" | *[!0-9A-Fa-f:.]* | *:::* | :[!:]* | *[!:]:) return 1 ;;
    *:*) ;;
    *) return 1 ;;
    esac
    double=0
    case $a in
    *::*::*) return 1 ;;
    *::*) double=1 ;;
    esac
    groups=0
    s=$a
    while :; do
        f=${s%%:*}
        last=0
        [ "$f" = "$s" ] && last=1
        case $f in
        "") ;;
        *.*)
            [ "$last" = 1 ] && check_ipv4 "$f" || return 1
            groups=$((groups + 2))
            ;;
        *)
            case $f in *[!0-9A-Fa-f]*) return 1 ;; esac
            [ "${#f}" -le 4 ] || return 1
            groups=$((groups + 1))
            ;;
        esac
        [ "$last" = 1 ] && break
        s=${s#*:}
    done
    if [ "$double" = 1 ]; then [ "$groups" -le 7 ]; else [ "$groups" -eq 8 ]; fi
}

# check_dns VALUE: dot-separated labels of 1-63 letters, digits and "-",
# none starting or ending with "-"; all-numeric names must be IPv4 addresses.
check_dns() {
    a=$1
    case $a in
    "" | *[!A-Za-z0-9.-]* | .* | *. | *..*) return 1 ;;
    *[!0-9.]*) ;;
    *) check_ipv4 "$a"; return ;;
    esac
    while :; do
        l=${a%%.*}
        case $l in
        -* | *-) return 1 ;;
        esac
        [ "${#l}" -le 63 ] || return 1
        [ "$l" = "$a" ] && break
        a=${a#*.}
    done
}

# check_url NAME VALUE: http(s)://host[:port][/path], host a DNS name, an
# IPv4 address or a bracketed IPv6 literal, port 1-65535; path of unreserved
# characters.
check_url() {
    name=$1
    v=$2
    [ -n "$v" ] || fail "$name is empty"
    [ "${#v}" -le "$MAX" ] || fail "$name is longer than $MAX characters"
    case $v in
    *[!]A-Za-z0-9._~:/%[-]*) fail "$name has a character outside A-Z a-z 0-9 . _ ~ : / % [ ] -" ;;
    esac
    case $v in
    http://* | https://*) ;;
    *) fail "$name must start with http:// or https://" ;;
    esac
    rest=${v#*://}
    hostport=${rest%%/*}
    path=${rest#"$hostport"}
    case $hostport in
    \[*\] | \[*\]:*)
        host=${hostport%%]*}]
        port=${hostport#"$host"}
        inner=${host#[}
        inner=${inner%]}
        check_ipv6 "$inner" || fail "$name has an invalid IPv6 host"
        ;;
    *)
        host=${hostport%%:*}
        port=${hostport#"$host"}
        check_dns "$host" || fail "$name has an invalid host"
        ;;
    esac
    case $port in
    "") ;;
    :*)
        p=${port#:}
        case $p in
        "" | *[!0-9]* | 0*) fail "$name has an invalid port" ;;
        esac
        [ "${#p}" -le 5 ] && [ "$p" -le 65535 ] || fail "$name has an invalid port (1-65535)"
        ;;
    *) fail "$name has an invalid host" ;;
    esac
    case $path in
    "" | /*) ;;
    *) fail "$name has an invalid path" ;;
    esac
    case $path in
    *[!A-Za-z0-9._~%/-]*) fail "$name has an invalid path" ;;
    esac
}

check_client() {
    v=$1
    [ -n "$v" ] && [ "${#v}" -le 100 ] || fail "WEB_OIDC_CLIENT_ID must have 1 to 100 characters"
    case $v in
    *[!A-Za-z0-9._-]*) fail "WEB_OIDC_CLIENT_ID may contain only letters, digits, '.', '_' and '-'" ;;
    esac
}

# scheme://host[:port] of a validated URL (parameter expansion, no tools).
origin_of() {
    rest=${1#*://}
    printf '%s://%s' "${1%%://*}" "${rest%%/*}"
}

# JSON string body. After validation only allow-listed characters remain;
# quote and backslash are escaped anyway, anything else unexpected fails.
json_escape() {
    s=$1
    out=
    while [ -n "$s" ]; do
        rest=${s#?}
        c=${s%"$rest"}
        s=$rest
        case $c in
        '"') out="$out\\\"" ;;
        '\') out="$out\\\\" ;;
        [A-Za-z0-9._~:/%@-] | '[' | ']') out="$out$c" ;;
        *) fail "unexpected character in a configuration value" ;;
        esac
    done
    printf '%s' "$out"
}

api=${WEB_API_URL:-}
authority=${WEB_OIDC_AUTHORITY:-}
client=${WEB_OIDC_CLIENT_ID:-}

check_url WEB_API_URL "$api"
check_url WEB_OIDC_AUTHORITY "$authority"
check_client "$client"

api=${api%/}
authority=${authority%/}
api_origin=$(origin_of "$api")
idp_origin=$(origin_of "$authority")
api_json=$(json_escape "$api")
authority_json=$(json_escape "$authority")
client_json=$(json_escape "$client")

conf_tmp=$(mktemp "$conf_out.XXXXXX")
json_tmp=$(mktemp "$json_out.XXXXXX")
trap 'rm -f "$conf_tmp" "$json_tmp"' EXIT

printf '{"apiBaseUrl":"%s","oidcAuthority":"%s","oidcClientId":"%s"}\n' \
    "$api_json" "$authority_json" "$client_json" >"$json_tmp"

{
    cat <<'NGINX'
# Written at container start by /docker-entrypoint.d/05-web-config.sh.
server {
    listen 5173;
    server_name _;
    server_tokens off;
    root /usr/share/nginx/html;
    charset utf-8;

    # Security headers on every response. No add_header inside the locations
    # below: that would drop these (nginx does not merge add_header levels).
NGINX
    printf '    add_header Content-Security-Policy "%s" always;\n' \
        "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self' $api_origin $idp_origin; frame-src 'none'; worker-src 'none'; object-src 'none'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
    cat <<'NGINX'
    add_header X-Content-Type-Options "nosniff" always;
    add_header Referrer-Policy "no-referrer" always;
    add_header X-Frame-Options "DENY" always;
    add_header Permissions-Policy "camera=(), microphone=(), geolocation=(), payment=(), usb=()" always;
    add_header Cross-Origin-Opener-Policy "same-origin" always;
    add_header Cross-Origin-Resource-Policy "same-origin" always;

    # Runtime configuration (public values only: public OIDC client, no secret).
    location = /config.json {
        alias /etc/nginx/web-runtime/config.json;
        default_type application/json;
        expires -1;
    }

    # Hashed build assets: cache long; a missing file is a 404, never the app.
    location /assets/ {
        expires 1y;
        try_files $uri =404;
    }

    # SPA: any other path is a client route.
    location / {
        expires -1;
        try_files $uri /index.html;
    }
}
NGINX
} >"$conf_tmp"

chmod 0644 "$conf_tmp" "$json_tmp"
mv -f "$json_tmp" "$json_out"
mv -f "$conf_tmp" "$conf_out"
trap - EXIT
echo "web-config: API $api, identity provider $authority, client $client"

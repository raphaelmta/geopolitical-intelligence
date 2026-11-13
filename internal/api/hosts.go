package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// DefaultAllowedHosts are the host names the API always answers to (with
// any port): the loopback names the browser and `make up` use, and "api",
// the compose service name other containers use.
var DefaultAllowedHosts = []string{"localhost", "127.0.0.1", "::1", "api"}

// ParseAllowedHosts reads a comma-separated list of extra host names
// (API_ALLOWED_HOSTS): host names or IP addresses without scheme, port, path
// or user info; IPv6 may be written in brackets. Empty entries are ignored;
// names are compared case-insensitively.
func ParseAllowedHosts(s string) ([]string, error) {
	var out []string
	for _, h := range strings.Split(s, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		bare := strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
		if net.ParseIP(bare) != nil {
			out = append(out, bare)
			continue
		}
		if !validHostName(h) {
			return nil, fmt.Errorf("api: allowed host %q is not a host name (no scheme, port, path or wildcard)", h)
		}
		out = append(out, h)
	}
	return out, nil
}

// validHostName: dot-separated labels of letters, digits and inner hyphens.
func validHostName(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

func allowedHostSet(extra []string) map[string]bool {
	set := map[string]bool{}
	for _, h := range append(append([]string{}, DefaultAllowedHosts...), extra...) {
		set[strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"))] = true
	}
	return set
}

var errNoHost = errors.New("no host")

// requestHost is the host name of a Host header value, without port or
// IPv6 brackets, lower-cased.
func requestHost(hostport string) (string, error) {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if h == "" {
		return "", errNoHost
	}
	return strings.ToLower(h), nil
}

// hostCheck answers 421 {"error":"misdirected_request"} to a request whose
// Host header names a host that is not allowed. The API binds to loopback;
// this keeps a web page from reaching it through a DNS name the attacker
// re-points at 127.0.0.1 (DNS rebinding), whose requests carry that name.
func hostCheck(allowed map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, err := requestHost(r.Host)
		if err != nil || !allowed[h] {
			info(r.Context()).set("misdirected_request", "")
			writeError(w, http.StatusMisdirectedRequest, "misdirected_request")
			return
		}
		next.ServeHTTP(w, r)
	})
}

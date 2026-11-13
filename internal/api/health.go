// Package api exposes the HTTP surface of the geoint binary: the JSON API
// under /api/v1 (authenticated, see router.go) and /healthz.
package api

import (
	"net/http"
)

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

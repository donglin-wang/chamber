package main

import (
	"net/http"
)

func newServer() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealth)
	registerDocsRoutes(mux)

	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
	})
}

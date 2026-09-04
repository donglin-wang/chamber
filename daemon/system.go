package main

import (
	"net/http"
)

type daemonStartupReport struct {
	Passed bool                 `json:"passed"`
	Scopes []daemonStartupScope `json:"scopes"`
}

type daemonStartupScope struct {
	Name           string `json:"name"`
	Implementation string `json:"implementation,omitempty"`
	Path           string `json:"path,omitempty"`
	Passed         bool   `json:"passed"`
	Error          string `json:"error,omitempty"`
}

type daemonStartupProbe struct {
	report daemonStartupReport
}

func newDaemonStartupProbe() *daemonStartupProbe {
	return &daemonStartupProbe{report: daemonStartupReport{Passed: true}}
}

func (probe *daemonStartupProbe) record(scope daemonStartupScope, err error) error {
	scope.Passed = err == nil
	if err != nil {
		scope.Error = err.Error()
		probe.report.Passed = false
	}
	probe.report.Scopes = append(probe.report.Scopes, scope)
	return err
}

func (probe *daemonStartupProbe) result() daemonStartupReport {
	if probe == nil {
		return daemonStartupReport{}
	}
	return probe.report
}

func registerSystemRoutes(mux *http.ServeMux, report daemonStartupReport) {
	mux.HandleFunc("GET /v1/system/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"startup_probe": report})
	})
}

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDaemonStartupProbeRecordsActualFailures(t *testing.T) {
	probe := newDaemonStartupProbe()
	failure := errors.New("runtime active probe failed")
	if err := probe.record(daemonStartupScope{Name: "runtime", Implementation: "runc"}, failure); !errors.Is(err, failure) {
		t.Fatalf("record() error = %v, want %v", err, failure)
	}
	report := probe.result()
	if report.Passed || len(report.Scopes) != 1 || report.Scopes[0].Passed || report.Scopes[0].Error != failure.Error() {
		t.Fatalf("startup report = %#v, want recorded runtime failure", report)
	}
}

func TestSystemInfoRouteReportsSelectedStartupScopes(t *testing.T) {
	report := daemonStartupReport{
		Passed: true,
		Scopes: []daemonStartupScope{
			{Name: "runtime", Implementation: "runc", Path: "/runtime", Passed: true},
			{Name: "socket", Path: "/run/chamber.sock", Passed: true},
		},
	}
	mux := newServer()
	registerSystemRoutes(mux, report)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/system/info", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		StartupProbe daemonStartupReport `json:"startup_probe"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.StartupProbe.Passed || len(response.StartupProbe.Scopes) != len(report.Scopes) {
		t.Fatalf("startup probe = %#v, want %#v", response.StartupProbe, report)
	}
}

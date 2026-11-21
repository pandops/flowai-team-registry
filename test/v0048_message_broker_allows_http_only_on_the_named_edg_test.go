//go:build !integration

package test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pandops/flowai-team-registry/internal/probe"
)

// Requirement: message-broker allows HTTP only on the named edges — Service
// offers no internal HTTP API. team-registry has no HTTP edge; "Source
// system reads its settings on the public edge", "Executor edge errors are
// distinguishable" and "Journal stream edge requires the Gateway token" are
// not applicable to team-registry.

func TestTeamRegistryServiceOffersNoInternalHTTPAPI(t *testing.T) {
	var checks atomic.Int32
	h := probe.Handler("team-registry", func(context.Context) error { checks.Add(1); return nil })
	for _, path := range []string{"/v1/livez", "/v1/readyz"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "team_") {
			t.Fatalf("probe %s carries data: %s", path, rec.Body.String())
		}
	}
	checksAfterProbes := checks.Load()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/internal/v1/teams/3"},
		{http.MethodGet, "/admin/teams/3"},
		{http.MethodPost, "/admin/teams"},
		{http.MethodPatch, "/admin/teams/3"},
		{http.MethodGet, "/v1/teams"},
		{http.MethodGet, "/"},
		{http.MethodGet, "/v1/livez/extra"},
		{http.MethodPost, "/v1/readyz"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"team_name":"x"}`)))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
	if checks.Load() != checksAfterProbes {
		t.Fatal("a non-probe request ran a dependency check")
	}
}

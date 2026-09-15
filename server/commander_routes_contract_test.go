package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// clientCommanderContract is the set of commander valuation-statement endpoints
// the aspirant-client calls through the /api proxy, as method+path pairs in the
// form gin registers them (":id" for the wildcard segment). It mirrors the
// client's own call sites (src/views/member/personal/ValuationStatement.vue).
//
// #5920: /decide was on this list on the client and had NO route on the server,
// so every call 404'd into the client's #306 fallback — a shipped feature dark
// in prod, green tests on both sides, because the only place the disagreement was
// visible is the route table of a third service. The same audit found GET
// operator-defaults missing too (manual entry, #5914, silently opened with empty
// defaults). This test is the list comparison that would have caught both.
//
// Adding a commander endpoint to the client means adding its row here and its
// route in routes.go — or this test reds, which is the whole point.
var clientCommanderContract = []struct{ method, path string }{
	{http.MethodPost, "/commander/valuation-statement/extract"},
	{http.MethodPost, "/commander/valuation-statement/decide"},
	{http.MethodPost, "/commander/valuation-statement/generate"},
	{http.MethodGet, "/commander/valuation-statement/operator-defaults"},
	{http.MethodPut, "/commander/valuation-statement/operator-defaults"},
	{http.MethodPost, "/commander/valuation-statement/processed"},
	{http.MethodGet, "/commander/valuation-statement/processed"},
	{http.MethodGet, "/commander/valuation-statement/processed/export.csv"},
	{http.MethodGet, "/commander/valuation-statement/processed/:id"},
	{http.MethodPatch, "/commander/valuation-statement/processed/:id"},
	{http.MethodDelete, "/commander/valuation-statement/processed/:id"},
}

// TestClientCommanderEndpointsAreRouted asserts every endpoint the client calls
// is registered on the production router. It compares against the shipped route
// table (RegisterRoutes via realRouter), not a re-declaration, so a dropped or
// renamed route reds here.
func TestClientCommanderEndpointsAreRouted(t *testing.T) {
	loadTestJWTSecret(t)
	r := realRouter(t)

	registered := map[string]bool{}
	for _, ri := range r.Routes() {
		registered[ri.Method+" "+ri.Path] = true
	}
	for _, want := range clientCommanderContract {
		if !registered[want.method+" "+want.path] {
			t.Errorf("client calls %s %s but the server registers no such route — a missing proxy hop 404s into the client fallback and the feature goes dark in prod (#5920)",
				want.method, want.path)
		}
	}
}

// TestDecideAndOperatorDefaultsGetReachTheAuthGate is the served-surface half of
// the #5920 acceptance: the two newly-added routes must reach the auth gate
// (401 unauthenticated), not fall through to the no-route 404 that was the bug.
// 401-vs-404 is the exact discriminator the row used to prove the gap.
func TestDecideAndOperatorDefaultsGetReachTheAuthGate(t *testing.T) {
	loadTestJWTSecret(t)
	r := realRouter(t)

	cases := []struct{ method, path string }{
		{http.MethodPost, "/commander/valuation-statement/decide"},
		{http.MethodGet, "/commander/valuation-statement/operator-defaults"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code == http.StatusNotFound {
				t.Fatalf("%s %s = 404: the route is not registered (the #5920 bug)", tc.method, tc.path)
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d unauthenticated, want 401 (route exists, auth required like /extract)", tc.method, tc.path, w.Code)
			}
		})
	}
}

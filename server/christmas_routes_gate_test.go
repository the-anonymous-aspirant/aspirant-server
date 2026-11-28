package server

import (
	"net/http"
	"strings"
	"testing"
)

// The Christmas cards (system_3 #12582) are admin-only: their words are the
// answers of a real hunt. Every route under /christmas is walked through the
// real RegisterRoutes router, with the card editor's fixtures, so a route
// added outside the Admin group fails here.
func TestChristmasRoutes_OnlyAdminPassesTheGate(t *testing.T) {
	loadTestJWTSecret(t)
	r := cardgenGateRouter(t)

	var routes []string
	for _, ri := range r.Routes() {
		if !strings.HasPrefix(ri.Path, "/christmas/") {
			continue
		}
		routes = append(routes, ri.Method+" "+strings.ReplaceAll(ri.Path, ":n", "1"))
	}
	// Positive control: an empty enumeration would pass every assertion below.
	if len(routes) < 10 {
		t.Fatalf("found %d /christmas/ routes, want the 9 of #12582 and #12780's opening times", len(routes))
	}

	member := mustToken(t, cardgenMemberID, "Member")
	viewer := mustToken(t, cardgenViewerID, "Viewer")
	admin := mustToken(t, cardgenAdminID, "Admin")

	for _, route := range routes {
		method, path, _ := strings.Cut(route, " ")
		t.Run(route, func(t *testing.T) {
			if code := cardgenCall(r, method, path, ""); code != http.StatusUnauthorized {
				t.Errorf("unauthenticated: got %d, want 401", code)
			}
			if code := cardgenCall(r, method, path, viewer); code != http.StatusForbidden {
				t.Errorf("Viewer: got %d, want 403", code)
			}
			if code := cardgenCall(r, method, path, member); code != http.StatusForbidden {
				t.Errorf("Member: got %d, want 403: a Member must not reach the Christmas cards", code)
			}
			if code := cardgenCall(r, method, path, admin); code == http.StatusUnauthorized || code == http.StatusForbidden {
				t.Errorf("Admin: got %d, want the gate to let Admin through", code)
			}
		})
	}
}

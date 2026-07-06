package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"aspirant-online/server/middleware"

	"github.com/gin-gonic/gin"
)

// --- IsPubliclyPublishedETag: whitelist lookup semantics ---

// A hash that appears in getDefaultAssetMappings — game-bg-music, a
// public asset that GameWordWeaver fetches unauthenticated.
const knownDefaultETag = "07ba67e86c21edb47a67728cfb6aa4ad"

// A random 32-hex string that must not collide with any default
// mapping; if this ever starts hitting the whitelist, either the
// defaults changed or a real MD5 collision was discovered — either
// way the test wants to know.
const unknownETag = "deadbeefdeadbeefdeadbeefdeadbeef"

func TestIsPubliclyPublishedETag_KnownDefaultReturnsTrue(t *testing.T) {
	if !IsPubliclyPublishedETag(knownDefaultETag) {
		t.Fatalf("known default ETag %s should be in the whitelist", knownDefaultETag)
	}
}

func TestIsPubliclyPublishedETag_UnknownReturnsFalse(t *testing.T) {
	if IsPubliclyPublishedETag(unknownETag) {
		t.Fatalf("random ETag %s should NOT be in the whitelist", unknownETag)
	}
}

func TestIsPubliclyPublishedETag_EmptyRejected(t *testing.T) {
	if IsPubliclyPublishedETag("") {
		t.Fatal("empty ETag must never pass the gate")
	}
}

func TestIsPubliclyPublishedETag_StripsQuotedInput(t *testing.T) {
	// S3-style quoted ETag: an upstream cache or client may send the
	// quoted form. The whitelist must match the same underlying MD5.
	quoted := "\"" + knownDefaultETag + "\""
	if !IsPubliclyPublishedETag(quoted) {
		t.Fatalf("quoted ETag %s should match unquoted whitelist entry", quoted)
	}
}

// --- FetchObjectHandler: gate behaviour ---

func newFetchObjectRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/fetch-object/:etag", FetchObjectHandler)
	return r
}

func TestFetchObjectHandler_UnknownETagUnauthenticatedReturns404(t *testing.T) {
	r := newFetchObjectRouter()
	req := httptest.NewRequest(http.MethodGet, "/fetch-object/"+unknownETag, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Gate returns 404 (Asset not found) so an unauthenticated probe
	// cannot distinguish "not in registry" from "not in storage".
	if w.Code != http.StatusNotFound {
		t.Fatalf("unauthenticated unknown ETag: want 404, got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestFetchObjectHandler_EmptyETagReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "etag", Value: ""}}
	FetchObjectHandler(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty etag: want 400, got %d — body=%s", w.Code, w.Body.String())
	}
}

// Storage is intentionally NOT wired in this test — a request that
// PASSES the gate must reach the "Asset storage not configured" 500
// path. Reaching 500 (not 404) proves the gate opened.
func TestFetchObjectHandler_KnownETagPassesGate(t *testing.T) {
	r := newFetchObjectRouter()
	req := httptest.NewRequest(http.MethodGet, "/fetch-object/"+knownDefaultETag, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Fatalf("known ETag was refused at the gate (got 404); expected 500 (storage unwired). body=%s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("known ETag: want 500 (storage unwired), got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestFetchObjectHandler_TrustedBearerBypassesGate(t *testing.T) {
	// Even for an ETag that is NOT in the whitelist, an authenticated
	// Trusted caller must pass the gate — the admin Assets.vue viewer
	// depends on this to preview uploaded files whose MD5 the admin
	// just listed via /assets.
	token, err := middleware.GenerateToken(42, "Trusted")
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	r := newFetchObjectRouter()
	req := httptest.NewRequest(http.MethodGet, "/fetch-object/"+unknownETag, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Fatalf("Trusted bearer failed to bypass the gate (got 404); expected 500 (storage unwired). body=%s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("Trusted bearer + unknown ETag: want 500 (storage unwired), got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestFetchObjectHandler_AdminBearerBypassesGate(t *testing.T) {
	token, err := middleware.GenerateToken(1, "Admin")
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	r := newFetchObjectRouter()
	req := httptest.NewRequest(http.MethodGet, "/fetch-object/"+unknownETag, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Fatalf("Admin bearer failed to bypass the gate (got 404). body=%s", w.Body.String())
	}
}

func TestFetchObjectHandler_MalformedBearerTreatedAsUnauthenticated(t *testing.T) {
	// A garbage Bearer must NOT surface a 401 — the endpoint is
	// public-shaped; the malformed token is simply ignored and the
	// caller falls through to the whitelist gate.
	r := newFetchObjectRouter()
	req := httptest.NewRequest(http.MethodGet, "/fetch-object/"+unknownETag, nil)
	req.Header.Set("Authorization", "Bearer not.a.jwt")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("malformed bearer: want 404 (fall through to gate), got %d — body=%s", w.Code, w.Body.String())
	}
}

func TestFetchObjectHandler_UnprivilegedRoleDoesNotBypass(t *testing.T) {
	// A "User" role token — valid signature, wrong role — must not
	// bypass the gate. Only Trusted/Admin do; regular users get the
	// same public-facing behaviour as unauthenticated callers.
	token, err := middleware.GenerateToken(99, "User")
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	r := newFetchObjectRouter()
	req := httptest.NewRequest(http.MethodGet, "/fetch-object/"+unknownETag, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("User-role bearer: want 404 (no bypass), got %d — body=%s", w.Code, w.Body.String())
	}
}

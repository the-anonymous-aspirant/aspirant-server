package handlers

import (
	"log"
	"net/http"

	"aspirant-online/server/middleware"
	"aspirant-online/server/storage"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// HealthCheckHandler handles the health check route.
// Response follows the convention schema: { status, service, checks }.
// The version field (git commit) was dropped from this unauthenticated
// surface (CWE-200, system_3 #3865) — no client reads it: HomeView.vue
// and SystemHealth.vue read fields this handler never served.
func HealthCheckHandler(c *gin.Context) {
	checks := gin.H{}
	allHealthy := true

	// Database check
	if db, exists := c.Get("db"); exists && db != nil {
		if gormDB, ok := db.(*gorm.DB); ok {
			if err := gormDB.DB().Ping(); err != nil {
				// Log the raw driver error server-side only — reflecting
				// err.Error() to the unauthenticated /health caller leaks
				// DSN details (host, user, database) on failure (CWE-209).
				log.Printf("Health check: database ping failed: %v", err)
				checks["database"] = "error"
				allHealthy = false
			} else {
				checks["database"] = "connected"
			}
		}
	} else {
		checks["database"] = "unavailable"
		allHealthy = false
	}

	status := "ok"
	if !allHealthy {
		status = "degraded"
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  status,
		"service": "server",
		"checks":  checks,
	})
}

// FetchObjectHandler serves an asset by its ETag (MD5 hash). The
// endpoint is UNAUTHENTICATED because the SPA shell references
// favicon/PWA-manifest/game-asset ETags before any JWT exists —
// so the defence against arbitrary-object disclosure
// (security-finding system_3 #1381, CWE-306/-285) is a hardcoded
// whitelist: unauthenticated callers can only retrieve ETags that
// appear in the asset_mappings publishing registry. Trusted-user
// uploads via /upload land in LocalStorage but stay unreachable
// through this handler even if their MD5 leaks. An authenticated
// caller with role Trusted or Admin bypasses the whitelist so the
// admin Assets.vue viewer can still preview newly uploaded files —
// role check keeps the bypass in line with the /assets Admin routes
// that already own the asset lifecycle.
func FetchObjectHandler(c *gin.Context) {
	etag := c.Param("etag")
	if etag == "" {
		RespondWithError(c, http.StatusBadRequest, "ETag parameter is required")
		return
	}

	if !callerCanBypassPublicETagGate(c) && !IsPubliclyPublishedETag(etag) {
		// Return 404 (not 401/403) so an unauthenticated probe cannot
		// distinguish "not in registry" from "not in storage" — never
		// leak the existence of a private object.
		RespondWithError(c, http.StatusNotFound, "Asset not found")
		return
	}

	store, exists := c.Get("storage")
	if !exists || store == nil {
		RespondWithError(c, http.StatusInternalServerError, "Asset storage not configured")
		return
	}
	assets := store.(*storage.LocalStorage)

	data, info, err := assets.GetByETag(etag)
	if err != nil {
		log.Printf("Failed to fetch asset by ETag %s: %v", etag, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to fetch asset")
		return
	}

	if data == nil {
		RespondWithError(c, http.StatusNotFound, "Asset not found")
		return
	}

	log.Printf("Serving asset: %s (%s, %d bytes)", info.Key, info.ContentType, len(data))
	c.Data(http.StatusOK, info.ContentType, data)
}

// callerCanBypassPublicETagGate returns true when the caller presents
// a valid JWT with a role that legitimately owns the asset lifecycle
// (Trusted or Admin). Falls back to false for unauthenticated,
// expired, or role-less tokens — no leak of tokens' internals to the
// public code path.
func callerCanBypassPublicETagGate(c *gin.Context) bool {
	_, role, ok := middleware.ParseTokenIfPresent(c)
	if !ok {
		return false
	}
	return role == "Trusted" || role == "Admin"
}

// UploadImageHandler uploads a file to the asset storage
func UploadImageHandler(c *gin.Context) {
	file, err := c.FormFile("image")
	if err != nil {
		RespondWithError(c, http.StatusBadRequest, "No file is received")
		return
	}

	path := c.PostForm("path")
	if path == "" {
		RespondWithError(c, http.StatusBadRequest, "Path is required")
		return
	}

	store, exists := c.Get("storage")
	if !exists || store == nil {
		RespondWithError(c, http.StatusInternalServerError, "Asset storage not configured")
		return
	}
	assets := store.(*storage.LocalStorage)

	fileContent, err := file.Open()
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to open file")
		return
	}
	defer fileContent.Close()

	err = assets.Put(path, fileContent)
	if err != nil {
		log.Printf("Failed to store asset at %s: %v", path, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to upload file")
		return
	}

	RespondWithSuccess(c, nil, "File uploaded successfully")
}

// DeleteAssetHandler deletes an asset by its key path
func DeleteAssetHandler(c *gin.Context) {
	key := c.Query("key")
	if key == "" {
		RespondWithError(c, http.StatusBadRequest, "Key parameter is required")
		return
	}

	store, exists := c.Get("storage")
	if !exists || store == nil {
		RespondWithError(c, http.StatusInternalServerError, "Asset storage not configured")
		return
	}
	assets := store.(*storage.LocalStorage)

	if err := assets.Delete(key); err != nil {
		log.Printf("Failed to delete asset %s: %v", key, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to delete asset")
		return
	}

	log.Printf("Deleted asset: %s", key)
	RespondWithSuccess(c, nil, "Asset deleted successfully")
}

// VerifyAdminHandler is a no-op endpoint whose sole purpose is to be
// wrapped by AuthMiddleware + ValidateRole("Admin") so that nginx's
// auth_request module can gate other upstream services on an Admin JWT.
// A 200 means the caller holds a valid Admin token; the middleware
// chain returns 401/403 otherwise, both of which nginx maps to a
// /login redirect.
func VerifyAdminHandler(c *gin.Context) {
	c.Status(http.StatusOK)
}

// ListAssetsHandler lists all assets in storage
func ListAssetsHandler(c *gin.Context) {
	store, exists := c.Get("storage")
	if !exists || store == nil {
		RespondWithError(c, http.StatusInternalServerError, "Asset storage not configured")
		return
	}
	assets := store.(*storage.LocalStorage)

	objects, err := assets.List("")
	if err != nil {
		log.Printf("Failed to list assets: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to list assets")
		return
	}

	c.JSON(http.StatusOK, gin.H{"assets": objects})
}

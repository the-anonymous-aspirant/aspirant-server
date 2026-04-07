package handlers

import (
	"log"
	"net/http"

	"aspirant-online/server/data_functions"
	"aspirant-online/server/storage"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

// HealthCheckHandler handles the health check route.
// Response follows the convention schema: { status, service, version, checks }
func HealthCheckHandler(c *gin.Context) {
	checks := gin.H{}
	allHealthy := true

	// Database check
	if db, exists := c.Get("db"); exists && db != nil {
		if gormDB, ok := db.(*gorm.DB); ok {
			if err := gormDB.DB().Ping(); err != nil {
				checks["database"] = "error: " + err.Error()
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
		"version": data_functions.GetGitCommit(),
		"checks":  checks,
	})
}

// FetchObjectHandler serves an asset by its ETag (MD5 hash)
func FetchObjectHandler(c *gin.Context) {
	etag := c.Param("etag")
	if etag == "" {
		RespondWithError(c, http.StatusBadRequest, "ETag parameter is required")
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

package handlers

import (
	"log"
	"net/http"
	"runtime"
	"time"

	"aspirant-online/server/data_functions"
	"aspirant-online/server/storage"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

var serverStartTime = time.Now()

// HealthCheckHandler handles the health check route
func HealthCheckHandler(c *gin.Context) {
	allHealthy := true

	// Database check
	dbStatus := gin.H{"status": "unavailable"}
	if db, exists := c.Get("db"); exists && db != nil {
		if gormDB, ok := db.(*gorm.DB); ok {
			if err := gormDB.DB().Ping(); err != nil {
				dbStatus = gin.H{"status": "unhealthy", "error": err.Error()}
				allHealthy = false
			} else {
				dbStatus = gin.H{"status": "healthy"}
			}
		}
	} else {
		allHealthy = false
	}

	// Memory stats
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	// Overall status
	status := "healthy"
	if !allHealthy {
		status = "degraded"
	}

	RespondWithSuccess(c, gin.H{
		"status": status,
		"commit": data_functions.GetGitCommit(),
		"uptime": time.Since(serverStartTime).Truncate(time.Second).String(),
		"database": dbStatus,
		"memory": gin.H{
			"alloc_mb":       mem.Alloc / 1024 / 1024,
			"total_alloc_mb": mem.TotalAlloc / 1024 / 1024,
			"sys_mb":         mem.Sys / 1024 / 1024,
			"heap_objects":   mem.HeapObjects,
			"gc_cycles":      mem.NumGC,
			"goroutines":     runtime.NumGoroutine(),
		},
		"go_version": runtime.Version(),
	}, "Health check complete")
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

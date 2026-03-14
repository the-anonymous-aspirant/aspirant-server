package handlers

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"aspirant-online/server/data_functions"

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

// FetchObjectHandler handles fetching an object from S3 and serving it to the frontend
func FetchObjectHandler(c *gin.Context) {
	etag := c.Param("etag")
	if etag == "" {
		RespondWithError(c, http.StatusBadRequest, "ETag parameter is required")
		return
	}

	bucket := os.Getenv("S3_BUCKET_NAME")
	if bucket == "" {
		RespondWithError(c, http.StatusInternalServerError, "S3 bucket not configured")
		return
	}

	// Ensure the ETag is wrapped in double quotes
	if etag[0] != '"' {
		etag = fmt.Sprintf("\"%s\"", etag)
	}

	log.Printf("Fetching object with ETag: %s from bucket: %s", etag, bucket)
	sess, err := data_functions.InitS3Session()
	if err != nil {
		log.Printf("Failed to initialize S3 session: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to initialize S3 session")
		return
	}

	key, err := data_functions.FindKeyByETag(sess, bucket, etag)
	if err != nil {
		log.Printf("Failed to find object by ETag %s: %v", etag, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to find object by ETag")
		return
	}

	if key == "" {
		RespondWithError(c, http.StatusNotFound, "Object not found")
		return
	}

	objectData, err := data_functions.FetchFileFromS3(sess, bucket, key)
	if err != nil {
		log.Printf("Failed to fetch object from S3 at key %s: %v", key, err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to fetch object from S3")
		return
	}

	// Determine the content type based on the file extension
	contentType := "application/octet-stream"
	if len(key) > 4 {
		switch key[len(key)-4:] {
		case ".mp3":
			contentType = "audio/mpeg"
		case ".wav":
			contentType = "audio/wav"
		case ".png":
			contentType = "image/png"
		}
	}

	log.Printf("Successfully fetched object with key: %s, content type: %s", key, contentType)
	c.Data(http.StatusOK, contentType, objectData)
}

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

	bucket := os.Getenv("S3_BUCKET_NAME")
	if bucket == "" {
		RespondWithError(c, http.StatusInternalServerError, "S3 bucket not configured")
		return
	}

	sess, err := data_functions.InitS3Session()
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to initialize S3 session")
		return
	}

	fileContent, err := file.Open()
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to open file")
		return
	}
	defer fileContent.Close()

	key := path
	err = data_functions.UploadFileToS3(sess, bucket, key, fileContent)
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to upload file to S3")
		return
	}

	RespondWithSuccess(c, nil, "File uploaded successfully")
}

// ListS3AssetsHandler handles listing all S3 assets
func ListS3AssetsHandler(c *gin.Context) {
	bucket := os.Getenv("S3_BUCKET_NAME")
	if bucket == "" {
		RespondWithError(c, http.StatusInternalServerError, "S3 bucket not configured")
		return
	}

	sess, err := data_functions.InitS3Session()
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to initialize S3 session")
		return
	}

	objects, err := data_functions.ListObjects(sess, bucket, "")
	if err != nil {
		RespondWithError(c, http.StatusInternalServerError, "Failed to list S3 objects")
		return
	}

	c.JSON(http.StatusOK, gin.H{"assets": objects})
}

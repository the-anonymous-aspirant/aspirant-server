package handlers

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

var monitorClient = &http.Client{Timeout: 30 * time.Second}

func monitorURL() string {
	if url := os.Getenv("MONITOR_URL"); url != "" {
		return url
	}
	return "http://monitor:8000"
}

// monitorProxyGet forwards a GET request to the monitor service and pipes the response back.
func monitorProxyGet(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", monitorURL(), path)

	resp, err := monitorClient.Get(url)
	if err != nil {
		log.Printf("Failed to reach monitor: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Monitor service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read monitor response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read monitor response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// GetMonitorHealthHandler proxies GET /health to the monitor service
func GetMonitorHealthHandler(c *gin.Context) {
	monitorProxyGet(c, "/health")
}

// GetMonitorContainersHandler proxies GET /containers to the monitor service
func GetMonitorContainersHandler(c *gin.Context) {
	monitorProxyGet(c, "/containers")
}

// GetMonitorDiskHandler proxies GET /disk to the monitor service
func GetMonitorDiskHandler(c *gin.Context) {
	monitorProxyGet(c, "/disk")
}

// GetDBStatsHandler returns database table sizes and row counts
func GetDBStatsHandler(c *gin.Context) {
	db, exists := c.Get("db")
	if !exists {
		RespondWithError(c, http.StatusInternalServerError, "Database not available")
		return
	}

	gormDB, ok := db.(*gorm.DB)
	if !ok {
		RespondWithError(c, http.StatusInternalServerError, "Invalid database connection")
		return
	}

	type tableInfo struct {
		Name     string `json:"name"`
		Rows     int64  `json:"rows"`
		SizeBytes int64 `json:"size_bytes"`
		SizeMB   float64 `json:"size_mb"`
	}

	rows, err := gormDB.Raw(`
		SELECT
			relname AS name,
			n_live_tup AS rows,
			pg_total_relation_size(quote_ident(relname)) AS size_bytes
		FROM pg_stat_user_tables
		ORDER BY pg_total_relation_size(quote_ident(relname)) DESC
	`).Rows()
	if err != nil {
		log.Printf("Failed to query table stats: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to query table stats")
		return
	}
	defer rows.Close()

	var tables []tableInfo
	var totalSize int64
	var totalRows int64

	for rows.Next() {
		var t tableInfo
		if err := rows.Scan(&t.Name, &t.Rows, &t.SizeBytes); err != nil {
			log.Printf("Failed to scan row: %v", err)
			continue
		}
		t.SizeMB = float64(t.SizeBytes) / 1024 / 1024
		totalSize += t.SizeBytes
		totalRows += t.Rows
		tables = append(tables, t)
	}

	RespondWithSuccess(c, gin.H{
		"tables":     tables,
		"total_size_mb": float64(totalSize) / 1024 / 1024,
		"total_rows":    totalRows,
		"table_count":   len(tables),
	}, "Database statistics")
}

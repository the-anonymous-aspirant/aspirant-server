package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"aspirant-online/server/data_models"

	"github.com/gin-gonic/gin"
	"github.com/jinzhu/gorm"
)

var monitorClient = newProxyClient("monitor", 30*time.Second)

func monitorURL() string {
	if url := os.Getenv("MONITOR_URL"); url != "" {
		return url
	}
	return "http://monitor:8000"
}

// monitorProxyGet forwards a GET request to the monitor service and pipes the response back.
func monitorProxyGet(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", monitorURL(), path)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}

	resp, err := doProxy(monitorClient, req, c)
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

// monitorControlToken is the bearer token aspirant-server presents to the
// monitor's write-scoped container-control endpoints (system_3 #10491-B /
// #10514). Unset disables the proxy routes below the same way an unset
// CONTROL_TOKEN disables them on the monitor side: the monitor refuses with
// its own 503, so there is one place that reports "not configured", not two.
func monitorControlToken() string {
	return os.Getenv("MONITOR_CONTROL_TOKEN")
}

// monitorProxyPost forwards a bodyless POST to the monitor service with the
// control bearer token attached, and pipes the response back unchanged. Only
// the container start/stop routes below use it. Returns the upstream's status
// code, or 0 if the call never produced one -- in that case the caller has
// already written its own error response.
func monitorProxyPost(c *gin.Context, path string) int {
	reqURL := fmt.Sprintf("%s%s", monitorURL(), path)

	req, err := http.NewRequest("POST", reqURL, nil)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return 0
	}
	if token := monitorControlToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := doProxy(monitorClient, req, c)
	if err != nil {
		log.Printf("Failed to reach monitor: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Monitor service unavailable")
		return 0
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read monitor response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read monitor response")
		return 0
	}

	if monitorRefusedServerCredential(resp.StatusCode, body) {
		// The credential that failed is the server's own control token, not
		// the caller's session: a 401 passed through reads as an expired
		// session in the client, whose interceptor then logs the admin out
		// (system_3 #10641). A server credential failing upstream is a gateway
		// fault, so it answers 502 with a code the client can name. The
		// caller still logs the monitor's own status.
		log.Printf("Monitor refused the control token: upstream status %d", resp.StatusCode)
		respondError(c, http.StatusBadGateway, "monitor_auth_failed", "The monitor refused the server's credentials")
		return resp.StatusCode
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
	return resp.StatusCode
}

// monitorRefusedServerCredential reports whether a monitor answer refuses the
// server's control token rather than the request: any 401, and any 403 other
// than not_controllable (the allowlist refusal, which is about the container
// and passes through unchanged). The monitor itself sends only 401
// unauthorized and 403 not_controllable (aspirant-monitor app/routes.py,
// _control_request); a 403 with another code would come from something in
// between, and is treated as the same gateway fault.
func monitorRefusedServerCredential(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.Error.Code != "not_controllable"
}

// logContainerControl records which admin asked to start or stop which
// container, and what the monitor answered (security review c65352 on
// #10515, asked for since c64991 on #10514): a container stop is the most
// destructive thing the admin page can do, and until now nothing recorded
// which admin acted. Deliberately never includes the bearer token.
func logContainerControl(c *gin.Context, action, name string, status int) {
	var userID uint
	if v, exists := c.Get("user_id"); exists {
		if uid, ok := v.(uint); ok {
			userID = uid
		}
	}
	log.Printf("container control: user_id=%d action=%s name=%q status=%d", userID, action, name, status)
}

// PostContainerStartHandler proxies POST /containers/:name/start to the
// monitor service. This route's Admin-tier gate (routes.go's adminRoutes
// group) is the only way a client reaches it; the monitor's own bearer token
// and hard allowlist (system_3 #10514) are a second, independent check once
// it arrives. This pair is "never a raw docker socket from the client": the
// client never holds a credential that talks to Docker directly, at either
// hop.
func PostContainerStartHandler(c *gin.Context) {
	name := c.Param("name")
	status := monitorProxyPost(c, fmt.Sprintf("/containers/%s/start", url.PathEscape(name)))
	logContainerControl(c, "start", name, status)
}

// PostContainerStopHandler proxies POST /containers/:name/stop to the monitor
// service. See PostContainerStartHandler.
func PostContainerStopHandler(c *gin.Context) {
	name := c.Param("name")
	status := monitorProxyPost(c, fmt.Sprintf("/containers/%s/stop", url.PathEscape(name)))
	logContainerControl(c, "stop", name, status)
}

// GetServiceUsageHandler returns the most recent service_usage_rollup run:
// one row per served service with its last-used time and request counts
// (system_3 #10491-B). Reads this service's own table directly, unlike the
// monitor-proxied routes above, so it needs no new credential.
func GetServiceUsageHandler(c *gin.Context) {
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

	type usageRow struct {
		ServiceName        string     `json:"service_name"`
		Kind               string     `json:"kind"`
		MemoryMB           *float64   `json:"memory_mb"`
		DiskMB             *float64   `json:"disk_mb"`
		LastUsedAt         *time.Time `json:"last_used_at"`
		RequestCount       *int64     `json:"request_count"`
		RequestCountMember *int64     `json:"request_count_member"`
	}

	// Found via the model, not a raw MAX(as_of) scan: scanning an aggregate
	// straight into *time.Time depends on the driver returning a time.Time
	// value rather than a string for a computed column, which sqlite (the
	// test suite's dialect) does not do.
	var mostRecent data_models.ServiceUsageRollup
	err := gormDB.Order("as_of desc").First(&mostRecent).Error
	if err == gorm.ErrRecordNotFound {
		RespondWithSuccess(c, gin.H{"as_of": nil, "services": []usageRow{}}, "No service usage rollup has run yet")
		return
	}
	if err != nil {
		log.Printf("Failed to find latest service usage rollup: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read service usage")
		return
	}
	asOf := mostRecent.AsOf

	var rows []data_models.ServiceUsageRollup
	if err := gormDB.Where("as_of = ?", asOf).Order("service_name").Find(&rows).Error; err != nil {
		log.Printf("Failed to read service usage rollup: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read service usage")
		return
	}

	results := make([]usageRow, 0, len(rows))
	for _, r := range rows {
		results = append(results, usageRow{
			ServiceName:        r.ServiceName,
			Kind:               r.Kind,
			MemoryMB:           r.MemoryMB,
			DiskMB:             r.DiskMB,
			LastUsedAt:         r.LastUsedAt,
			RequestCount:       r.RequestCount,
			RequestCountMember: r.RequestCountMember,
		})
	}

	RespondWithSuccess(c, gin.H{"as_of": asOf, "services": results}, "Latest service usage rollup")
}

// dbStatsQuery sizes every table by its OID (relid), never by name. A name is
// cast to regclass through the session's search_path, and server's path does
// not include the per-service schemas that #9321 moved tables into
// (aspirant_commander and the rest), so one moved table failed the whole
// statement and the route returned 500 (system_3 #10686).
const dbStatsQuery = `
		SELECT
			schemaname AS schema,
			relname AS name,
			n_live_tup AS rows,
			pg_total_relation_size(relid) AS size_bytes
		FROM pg_stat_user_tables
		ORDER BY pg_total_relation_size(relid) DESC
	`

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
		Schema    string  `json:"schema"`
		Name      string  `json:"name"`
		Rows      int64   `json:"rows"`
		SizeBytes int64   `json:"size_bytes"`
		SizeMB    float64 `json:"size_mb"`
	}

	rows, err := gormDB.Raw(dbStatsQuery).Rows()
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
		if err := rows.Scan(&t.Schema, &t.Name, &t.Rows, &t.SizeBytes); err != nil {
			log.Printf("Failed to scan row: %v", err)
			continue
		}
		t.SizeMB = float64(t.SizeBytes) / 1024 / 1024
		totalSize += t.SizeBytes
		totalRows += t.Rows
		tables = append(tables, t)
	}

	RespondWithSuccess(c, gin.H{
		"tables":        tables,
		"total_size_mb": float64(totalSize) / 1024 / 1024,
		"total_rows":    totalRows,
		"table_count":   len(tables),
	}, "Database statistics")
}

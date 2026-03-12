package handlers

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
)

var remarkableClient = &http.Client{Timeout: 120 * time.Second}

func remarkableURL() string {
	if url := os.Getenv("REMARKABLE_URL"); url != "" {
		return url
	}
	return "http://remarkable:8000"
}

// remarkableProxyGet forwards a GET request to the remarkable service.
// Uses io.Copy for streaming binary responses (PNG/PDF/ZIP).
func remarkableProxyGet(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", remarkableURL(), path)
	if c.Request.URL.RawQuery != "" {
		url += "?" + c.Request.URL.RawQuery
	}

	resp, err := remarkableClient.Get(url)
	if err != nil {
		log.Printf("Failed to reach remarkable: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Remarkable service unavailable")
		return
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	contentDisposition := resp.Header.Get("Content-Disposition")

	if contentDisposition != "" {
		c.Header("Content-Disposition", contentDisposition)
	}

	c.Status(resp.StatusCode)
	c.Header("Content-Type", contentType)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		log.Printf("Failed to stream remarkable response: %v", err)
	}
}

// remarkableProxyPost forwards a POST request to the remarkable service.
func remarkableProxyPost(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", remarkableURL(), path)

	req, err := http.NewRequest("POST", url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	req.Header.Set("Content-Type", c.GetHeader("Content-Type"))
	req.ContentLength = c.Request.ContentLength

	resp, err := remarkableClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach remarkable: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Remarkable service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read remarkable response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read remarkable response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// remarkableProxyDelete forwards a DELETE request to the remarkable service.
func remarkableProxyDelete(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", remarkableURL(), path)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}

	resp, err := remarkableClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach remarkable: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Remarkable service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read remarkable response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read remarkable response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// GetRemarkableHealthHandler proxies GET /health
func GetRemarkableHealthHandler(c *gin.Context) {
	remarkableProxyGet(c, "/health")
}

// ListRemarkableNotebooksHandler proxies GET /notebooks
func ListRemarkableNotebooksHandler(c *gin.Context) {
	remarkableProxyGet(c, "/notebooks")
}

// GetRemarkableNotebookHandler proxies GET /notebooks/:id
func GetRemarkableNotebookHandler(c *gin.Context) {
	id := c.Param("id")
	remarkableProxyGet(c, fmt.Sprintf("/notebooks/%s", id))
}

// RenderRemarkablePageHandler proxies GET /notebooks/:id/pages/:page/render
func RenderRemarkablePageHandler(c *gin.Context) {
	id := c.Param("id")
	page := c.Param("page")
	remarkableProxyGet(c, fmt.Sprintf("/notebooks/%s/pages/%s/render", id, page))
}

// ExportRemarkableNotebookHandler proxies GET /notebooks/:id/export
func ExportRemarkableNotebookHandler(c *gin.Context) {
	id := c.Param("id")
	remarkableProxyGet(c, fmt.Sprintf("/notebooks/%s/export", id))
}

// ListRemarkableFoldersHandler proxies GET /folders
func ListRemarkableFoldersHandler(c *gin.Context) {
	remarkableProxyGet(c, "/folders")
}

// GetRemarkableFolderContentsHandler proxies GET /folders/:id/contents
func GetRemarkableFolderContentsHandler(c *gin.Context) {
	id := c.Param("id")
	remarkableProxyGet(c, fmt.Sprintf("/folders/%s/contents", id))
}

// GetRemarkableTreeHandler proxies GET /tree
func GetRemarkableTreeHandler(c *gin.Context) {
	remarkableProxyGet(c, "/tree")
}

// SyncRemarkableHandler proxies POST /sync
func SyncRemarkableHandler(c *gin.Context) {
	remarkableProxyPost(c, "/sync")
}

// GetRemarkableSyncStatusHandler proxies GET /sync/status
func GetRemarkableSyncStatusHandler(c *gin.Context) {
	remarkableProxyGet(c, "/sync/status")
}

// UploadRemarkableToDeviceHandler proxies POST /to-device/upload (multipart)
func UploadRemarkableToDeviceHandler(c *gin.Context) {
	remarkableProxyPost(c, "/to-device/upload")
}

// ListRemarkablePendingHandler proxies GET /to-device/pending
func ListRemarkablePendingHandler(c *gin.Context) {
	remarkableProxyGet(c, "/to-device/pending")
}

// DeleteRemarkablePendingHandler proxies DELETE /to-device/:id
func DeleteRemarkablePendingHandler(c *gin.Context) {
	id := c.Param("id")
	remarkableProxyDelete(c, fmt.Sprintf("/to-device/%s", id))
}

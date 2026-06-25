package handlers

import (
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
)

var browserClient = &http.Client{Timeout: 60 * time.Second}

func browserURL() string {
	if url := os.Getenv("BROWSER_URL"); url != "" {
		return url
	}
	return "http://browser:8000"
}

// BrowserProxyHandler forwards any request received under /browser-flows*
// (after nginx strips the /api/ prefix) to the aspirant-browser JSON API
// at /api/browser-flows*, preserving method, body, query string, and
// Content-Type / Accept headers.
//
// HTML page routes (`/browser-flows`, `/browser-flows/{id}`) are served
// directly by nginx → browser:8000 and never reach this handler.
func BrowserProxyHandler(c *gin.Context) {
	path := "/api" + c.Request.URL.Path
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		path = path + "?" + rawQuery
	}

	url := browserURL() + path

	req, err := http.NewRequest(c.Request.Method, url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to build browser proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to build proxy request")
		return
	}

	if ct := c.GetHeader("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if accept := c.GetHeader("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := browserClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach browser service: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Browser service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read browser response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read browser response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

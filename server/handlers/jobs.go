package handlers

import (
	"io"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// JobsProxyHandler forwards any request received under /jobs* (after nginx
// strips the /api/ prefix) to the aspirant-browser JSON API at /api/jobs*,
// preserving method, body, query string, and Content-Type / Accept headers.
//
// Tied to the Trusted-role /trusted/jobs overview page (#1290): the
// operator browses the deduplicated jobs feed and hides reviewed rows.
// aspirant-browser exposes /api/jobs (read) and /api/jobs/{id}/hide
// (PATCH); both reach the operator through this proxy.
//
// Same upstream client + URL resolver as BrowserProxyHandler — both
// routes land on the same aspirant-browser service.
func JobsProxyHandler(c *gin.Context) {
	path := "/api" + c.Request.URL.Path
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		path = path + "?" + rawQuery
	}

	url := browserURL() + path

	req, err := http.NewRequest(c.Request.Method, url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to build jobs proxy request: %v", err)
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
		log.Printf("Failed to reach browser service for jobs: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Browser service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read jobs proxy response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read browser response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

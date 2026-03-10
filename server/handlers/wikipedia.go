package handlers

import (
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
)

var wikipediaClient = &http.Client{Timeout: 60 * time.Second}

func kiwixURL() string {
	if url := os.Getenv("KIWIX_URL"); url != "" {
		return url
	}
	return "http://kiwix:8080"
}

// WikipediaProxyHandler proxies all requests to the kiwix-serve container.
// Kiwix is configured with --urlRootLocation /api/wikipedia, so we reconstruct
// the full path before forwarding.
func WikipediaProxyHandler(c *gin.Context) {
	path := c.Param("path")
	targetURL := kiwixURL() + "/api/wikipedia" + path
	if c.Request.URL.RawQuery != "" {
		targetURL += "?" + c.Request.URL.RawQuery
	}

	req, err := http.NewRequest(c.Request.Method, targetURL, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create kiwix proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	req.Header.Set("Accept", c.GetHeader("Accept"))
	req.Header.Set("Accept-Encoding", c.GetHeader("Accept-Encoding"))

	resp, err := wikipediaClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach kiwix: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Wikipedia service unavailable")
		return
	}
	defer resp.Body.Close()

	// Stream response headers
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	c.Status(resp.StatusCode)
	io.Copy(c.Writer, resp.Body)
}

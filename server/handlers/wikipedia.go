package handlers

import (
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var wikipediaClient = &http.Client{Timeout: 60 * time.Second}

const zimName = "wikipedia_en_all_maxi_2026-02"

func kiwixURL() string {
	if url := os.Getenv("KIWIX_URL"); url != "" {
		return url
	}
	return "http://kiwix:8080"
}

// WikipediaProxyHandler proxies all requests to the kiwix-serve container.
// Kiwix is configured with --urlRootLocation /api/wikipedia, so we reconstruct
// the full path before forwarding.
//
// Kiwix's search overlay generates content links without the ZIM name
// (e.g. /content/Africa instead of /content/wikipedia_en_all_maxi_2026-02/Africa).
// We detect these and rewrite them to include the ZIM name prefix.
func WikipediaProxyHandler(c *gin.Context) {
	path := c.Param("path")

	// Rewrite content paths that are missing the ZIM name prefix
	contentPrefix := "/content/"
	zimContentPrefix := "/content/" + zimName + "/"
	if strings.HasPrefix(path, contentPrefix) && !strings.HasPrefix(path, zimContentPrefix) {
		article := strings.TrimPrefix(path, contentPrefix)
		path = zimContentPrefix + article
	}

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

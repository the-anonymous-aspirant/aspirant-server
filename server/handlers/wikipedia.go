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

// wikipediaClient does not follow redirects so we can rewrite Location headers
// from kiwix before passing them back to the browser.
var wikipediaClient = &http.Client{
	Timeout: 60 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

const zimName = "wikipedia_en_all_maxi_2026-02"

// kiwixKnownPrefixes lists path prefixes that belong to kiwix's own routing.
// Any path NOT matching these is treated as a bare article path.
var kiwixKnownPrefixes = []string{
	"/content/", "/search", "/catalog/", "/skin/",
	"/viewer", "/catch/", "/suggest", "/random",
	"/raw/",
}

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
// Kiwix links arrive in several forms that all need rewriting:
//  1. Bare article paths: /Applied_mathematics → /content/{zim}/Applied_mathematics
//  2. Content paths missing ZIM name: /content/Africa → /content/{zim}/Africa
//
// We also disable automatic redirect following so that any 302 from kiwix
// has its Location header rewritten before being sent to the browser.
func WikipediaProxyHandler(c *gin.Context) {
	path := c.Param("path")
	path = rewriteArticlePath(path)

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

	// Rewrite Location header on redirects so the browser's next request
	// also goes through our proxy with proper article path rewriting.
	if loc := resp.Header.Get("Location"); loc != "" {
		const proxyPrefix = "/api/wikipedia"
		if strings.HasPrefix(loc, proxyPrefix) {
			articlePath := strings.TrimPrefix(loc, proxyPrefix)
			resp.Header.Set("Location", proxyPrefix+rewriteArticlePath(articlePath))
		}
	}

	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	c.Status(resp.StatusCode)
	io.Copy(c.Writer, resp.Body)
}

// rewriteArticlePath ensures the path includes /content/{zimName}/.
// Bare article paths (not matching any known kiwix prefix) get the full prefix.
// Content paths missing the ZIM name get it inserted.
func rewriteArticlePath(path string) string {
	zimContentPrefix := "/content/" + zimName + "/"
	zimContentExact := "/content/" + zimName

	// Already has the full prefix (with or without trailing slash)
	if strings.HasPrefix(path, zimContentPrefix) || path == zimContentExact {
		return path
	}

	// Content path missing ZIM name: /content/Africa → /content/{zim}/Africa
	if strings.HasPrefix(path, "/content/") {
		article := strings.TrimPrefix(path, "/content/")
		return zimContentPrefix + article
	}

	// Check if this is a known kiwix path (search, skin, catalog, etc.)
	for _, prefix := range kiwixKnownPrefixes {
		if strings.HasPrefix(path, prefix) {
			return path
		}
	}

	// Bare article path: /Applied_mathematics → /content/{zim}/Applied_mathematics
	if path != "" && path != "/" {
		return zimContentPrefix + strings.TrimPrefix(path, "/")
	}

	return path
}

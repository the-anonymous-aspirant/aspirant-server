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

// Longer timeout for advisor — LLM generation on CPU can take 60s+
var advisorClient = &http.Client{Timeout: 120 * time.Second}

func advisorURL() string {
	if url := os.Getenv("ADVISOR_URL"); url != "" {
		return url
	}
	return "http://advisor:8000"
}

func advisorProxyGet(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", advisorURL(), path)

	resp, err := advisorClient.Get(url)
	if err != nil {
		log.Printf("Failed to reach advisor: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Advisor service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read advisor response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read advisor response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

func advisorProxyPost(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", advisorURL(), path)

	req, err := http.NewRequest("POST", url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	req.Header.Set("Content-Type", c.GetHeader("Content-Type"))

	resp, err := advisorClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach advisor: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Advisor service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read advisor response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read advisor response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

func advisorProxyDelete(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", advisorURL(), path)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}

	resp, err := advisorClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach advisor: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Advisor service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read advisor response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read advisor response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// advisorProxyMultipart forwards a multipart/form-data request to the advisor service.
func advisorProxyMultipart(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", advisorURL(), path)

	req, err := http.NewRequest("POST", url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	// Preserve the original Content-Type with boundary parameter
	req.Header.Set("Content-Type", c.GetHeader("Content-Type"))

	resp, err := advisorClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach advisor: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Advisor service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read advisor response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read advisor response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// --- Handler functions ---

func GetAdvisorHealthHandler(c *gin.Context) {
	advisorProxyGet(c, "/health")
}

func GetAdvisorSourcesHandler(c *gin.Context) {
	advisorProxyGet(c, "/sources")
}

func QueryAdvisorHandler(c *gin.Context) {
	advisorProxyPost(c, "/query")
}

func ListAdvisorDocumentsHandler(c *gin.Context) {
	advisorProxyGet(c, "/documents")
}

func GetAdvisorDocumentHandler(c *gin.Context) {
	id := c.Param("id")
	advisorProxyGet(c, fmt.Sprintf("/documents/%s", id))
}

func UploadAdvisorDocumentHandler(c *gin.Context) {
	advisorProxyMultipart(c, "/documents")
}

func DeleteAdvisorDocumentHandler(c *gin.Context) {
	id := c.Param("id")
	advisorProxyDelete(c, fmt.Sprintf("/documents/%s", id))
}

func GetAdvisorDocumentChunksHandler(c *gin.Context) {
	id := c.Param("id")
	advisorProxyGet(c, fmt.Sprintf("/documents/%s/chunks", id))
}

func ReprocessAdvisorDocumentHandler(c *gin.Context) {
	id := c.Param("id")
	advisorProxyPost(c, fmt.Sprintf("/documents/%s/reprocess", id))
}

func IngestAdvisorLawsHandler(c *gin.Context) {
	advisorProxyPost(c, "/laws")
}

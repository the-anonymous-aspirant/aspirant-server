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

var translatorClient = &http.Client{Timeout: 30 * time.Second}

func translatorURL() string {
	if url := os.Getenv("TRANSLATOR_URL"); url != "" {
		return url
	}
	return "http://translator:8000"
}

// translatorProxyGet forwards a GET request to the translator service and pipes the response back.
func translatorProxyGet(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", translatorURL(), path)

	resp, err := translatorClient.Get(url)
	if err != nil {
		log.Printf("Failed to reach translator: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Translator service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read translator response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read translator response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// translatorProxyPost forwards a POST request to the translator service and pipes the response back.
func translatorProxyPost(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", translatorURL(), path)

	req, err := http.NewRequest("POST", url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	req.Header.Set("Content-Type", c.GetHeader("Content-Type"))

	resp, err := translatorClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach translator: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Translator service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read translator response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read translator response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// GetTranslatorHealthHandler proxies GET /health to the translator service
func GetTranslatorHealthHandler(c *gin.Context) {
	translatorProxyGet(c, "/health")
}

// GetTranslatorLanguagesHandler proxies GET /languages to the translator service
func GetTranslatorLanguagesHandler(c *gin.Context) {
	translatorProxyGet(c, "/languages")
}

// InstallTranslatorLanguageHandler proxies POST /languages/install to the translator service
func InstallTranslatorLanguageHandler(c *gin.Context) {
	translatorProxyPost(c, "/languages/install")
}

// TranslateHandler proxies POST /translations to the translator service
func TranslateHandler(c *gin.Context) {
	translatorProxyPost(c, "/translations")
}

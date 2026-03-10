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

var proxyClient = &http.Client{Timeout: 120 * time.Second}

func transcriberURL() string {
	if url := os.Getenv("TRANSCRIBER_URL"); url != "" {
		return url
	}
	return "http://transcriber:8000"
}

// proxyGet forwards a GET request to the transcriber and pipes the response back.
func proxyGet(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", transcriberURL(), path)

	resp, err := proxyClient.Get(url)
	if err != nil {
		log.Printf("Failed to reach transcriber: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Transcriber service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read transcriber response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read transcriber response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// GetTranscriberHealthHandler proxies GET /health to the transcriber service
func GetTranscriberHealthHandler(c *gin.Context) {
	proxyGet(c, "/health")
}

// ListVoiceMessagesHandler proxies GET /voice-messages to the transcriber service
func ListVoiceMessagesHandler(c *gin.Context) {
	proxyGet(c, "/voice-messages")
}

// GetVoiceMessageHandler proxies GET /voice-messages/:id to the transcriber service
func GetVoiceMessageHandler(c *gin.Context) {
	proxyGet(c, fmt.Sprintf("/voice-messages/%s", c.Param("id")))
}

// UploadVoiceMessageHandler proxies POST /voice-messages to the transcriber service
func UploadVoiceMessageHandler(c *gin.Context) {
	url := fmt.Sprintf("%s/voice-messages", transcriberURL())

	req, err := http.NewRequest("POST", url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	req.Header.Set("Content-Type", c.GetHeader("Content-Type"))

	resp, err := proxyClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach transcriber: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Transcriber service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read transcriber response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read transcriber response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// DeleteVoiceMessageHandler proxies DELETE /voice-messages/:id to the transcriber service
func DeleteVoiceMessageHandler(c *gin.Context) {
	url := fmt.Sprintf("%s/voice-messages/%s", transcriberURL(), c.Param("id"))

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}

	resp, err := proxyClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach transcriber: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Transcriber service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read transcriber response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read transcriber response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// GetVoiceAudioHandler proxies GET /voice-messages/:id/audio to the transcriber service
func GetVoiceAudioHandler(c *gin.Context) {
	url := fmt.Sprintf("%s/voice-messages/%s/audio", transcriberURL(), c.Param("id"))

	resp, err := proxyClient.Get(url)
	if err != nil {
		log.Printf("Failed to reach transcriber: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Transcriber service unavailable")
		return
	}
	defer resp.Body.Close()

	c.Status(resp.StatusCode)
	c.Header("Content-Type", resp.Header.Get("Content-Type"))
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		c.Header("Content-Disposition", cd)
	}
	io.Copy(c.Writer, resp.Body)
}

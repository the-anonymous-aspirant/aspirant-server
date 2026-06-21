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

var commanderClient = &http.Client{Timeout: 30 * time.Second}

func commanderURL() string {
	if url := os.Getenv("COMMANDER_URL"); url != "" {
		return url
	}
	return "http://commander:8000"
}

// commanderProxyGet forwards a GET request to the commander service and pipes the response back.
func commanderProxyGet(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", commanderURL(), path)

	resp, err := commanderClient.Get(url)
	if err != nil {
		log.Printf("Failed to reach commander: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Commander service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read commander response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read commander response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// ListCommanderTasksHandler proxies GET /tasks to the commander service
func ListCommanderTasksHandler(c *gin.Context) {
	path := "/tasks"
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		path = fmt.Sprintf("/tasks?%s", rawQuery)
	}
	commanderProxyGet(c, path)
}

// GetCommanderTaskHandler proxies GET /tasks/:id to the commander service
func GetCommanderTaskHandler(c *gin.Context) {
	commanderProxyGet(c, fmt.Sprintf("/tasks/%s", c.Param("id")))
}

// UpdateCommanderTaskHandler proxies PATCH /tasks/:id to the commander service
func UpdateCommanderTaskHandler(c *gin.Context) {
	url := fmt.Sprintf("%s/tasks/%s", commanderURL(), c.Param("id"))

	req, err := http.NewRequest("PATCH", url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := commanderClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach commander: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Commander service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read commander response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read commander response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// DeleteCommanderTaskHandler proxies DELETE /tasks/:id to the commander service
func DeleteCommanderTaskHandler(c *gin.Context) {
	url := fmt.Sprintf("%s/tasks/%s", commanderURL(), c.Param("id"))

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}

	resp, err := commanderClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach commander: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Commander service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read commander response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read commander response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// TriggerCommanderProcessHandler proxies POST /process to the commander service
func TriggerCommanderProcessHandler(c *gin.Context) {
	url := fmt.Sprintf("%s/process", commanderURL())

	req, err := http.NewRequest("POST", url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	req.Header.Set("Content-Type", c.GetHeader("Content-Type"))

	resp, err := commanderClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach commander: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Commander service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read commander response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read commander response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// GetCommanderVocabularyHandler proxies GET /vocabulary to the commander service
func GetCommanderVocabularyHandler(c *gin.Context) {
	commanderProxyGet(c, "/vocabulary")
}

// ListCommanderNotesHandler proxies GET /notes to the commander service
func ListCommanderNotesHandler(c *gin.Context) {
	path := "/notes"
	if rawQuery := c.Request.URL.RawQuery; rawQuery != "" {
		path = fmt.Sprintf("/notes?%s", rawQuery)
	}
	commanderProxyGet(c, path)
}

// GetCommanderNoteHandler proxies GET /notes/:id to the commander service
func GetCommanderNoteHandler(c *gin.Context) {
	commanderProxyGet(c, fmt.Sprintf("/notes/%s", c.Param("id")))
}

// UpdateCommanderNoteHandler proxies PATCH /notes/:id to the commander service
func UpdateCommanderNoteHandler(c *gin.Context) {
	url := fmt.Sprintf("%s/notes/%s", commanderURL(), c.Param("id"))

	req, err := http.NewRequest("PATCH", url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := commanderClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach commander: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Commander service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read commander response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read commander response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// DeleteCommanderNoteHandler proxies DELETE /notes/:id to the commander service
func DeleteCommanderNoteHandler(c *gin.Context) {
	url := fmt.Sprintf("%s/notes/%s", commanderURL(), c.Param("id"))

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}

	resp, err := commanderClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach commander: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Commander service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read commander response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read commander response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// GetCommanderHealthHandler proxies GET /health to the commander service
func GetCommanderHealthHandler(c *gin.Context) {
	commanderProxyGet(c, "/health")
}

// commanderProxyPassthrough forwards any method + body + headers to commander
// and streams the response back, preserving Content-Type and Content-Disposition.
// Used by the valuation-statement endpoints where the request is multipart or
// the response is a binary file download.
//
// The original request's query string is appended verbatim — POST
// /valuation-statement/generate?format=pdf must hit commander as
// /valuation-statement/generate?format=pdf, otherwise commander defaults to
// docx and the client receives a docx download when the operator asked for a
// PDF. Locked by TestCommanderProxyPreservesQueryString.
func commanderProxyPassthrough(c *gin.Context, method string, path string) {
	url := fmt.Sprintf("%s%s", commanderURL(), path)
	if raw := c.Request.URL.RawQuery; raw != "" {
		url += "?" + raw
	}

	req, err := http.NewRequest(method, url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	if ct := c.GetHeader("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.ContentLength = c.Request.ContentLength

	resp, err := commanderClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach commander: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Commander service unavailable")
		return
	}
	defer resp.Body.Close()

	// Preserve the Content-Disposition header so file downloads carry their
	// suggested filename through to the browser.
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		c.Header("Content-Disposition", cd)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read commander response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read commander response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// PostCommanderValuationExtractHandler proxies POST /valuation-statement/extract.
// Multipart upload of one or more property-document PDFs.
func PostCommanderValuationExtractHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "POST", "/valuation-statement/extract")
}

// PostCommanderValuationGenerateHandler proxies POST /valuation-statement/generate.
// JSON body of reviewed values → docx file download.
func PostCommanderValuationGenerateHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "POST", "/valuation-statement/generate")
}

// PutCommanderValuationOperatorDefaultsHandler proxies PUT
// /valuation-statement/operator-defaults. Persists the appraiser-identity
// fields the review step pre-fills.
func PutCommanderValuationOperatorDefaultsHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "PUT", "/valuation-statement/operator-defaults")
}

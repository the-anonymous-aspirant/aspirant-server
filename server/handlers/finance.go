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

var financeClient = &http.Client{Timeout: 60 * time.Second}

func financeURL() string {
	if url := os.Getenv("FINANCE_URL"); url != "" {
		return url
	}
	return "http://finance:8000"
}

// financeProxyGet forwards a GET request to the finance service.
func financeProxyGet(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", financeURL(), path)
	if c.Request.URL.RawQuery != "" {
		url += "?" + c.Request.URL.RawQuery
	}

	resp, err := financeClient.Get(url)
	if err != nil {
		log.Printf("Failed to reach finance: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Finance service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read finance response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read finance response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// financeProxyPost forwards a POST request to the finance service.
func financeProxyPost(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", financeURL(), path)

	req, err := http.NewRequest("POST", url, c.Request.Body)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}
	req.Header.Set("Content-Type", c.GetHeader("Content-Type"))
	req.ContentLength = c.Request.ContentLength

	resp, err := financeClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach finance: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Finance service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read finance response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read finance response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// financeProxyDelete forwards a DELETE request to the finance service.
func financeProxyDelete(c *gin.Context, path string) {
	url := fmt.Sprintf("%s%s", financeURL(), path)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		log.Printf("Failed to create proxy request: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to create proxy request")
		return
	}

	resp, err := financeClient.Do(req)
	if err != nil {
		log.Printf("Failed to reach finance: %v", err)
		RespondWithError(c, http.StatusBadGateway, "Finance service unavailable")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read finance response: %v", err)
		RespondWithError(c, http.StatusInternalServerError, "Failed to read finance response")
		return
	}

	c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
}

// GetFinanceHealthHandler proxies GET /health
func GetFinanceHealthHandler(c *gin.Context) {
	financeProxyGet(c, "/health")
}

// UploadFinanceCSVHandler proxies POST /sources/:bank/upload
func UploadFinanceCSVHandler(c *gin.Context) {
	bank := c.Param("bank")
	financeProxyPost(c, fmt.Sprintf("/sources/%s/upload", bank))
}

// ListFinanceSourcesHandler proxies GET /sources
func ListFinanceSourcesHandler(c *gin.Context) {
	financeProxyGet(c, "/sources")
}

// GetFinanceSourceSchemaHandler proxies GET /sources/:bank/schema
func GetFinanceSourceSchemaHandler(c *gin.Context) {
	bank := c.Param("bank")
	financeProxyGet(c, fmt.Sprintf("/sources/%s/schema", bank))
}

// ListFinanceTransactionsHandler proxies GET /transactions
func ListFinanceTransactionsHandler(c *gin.Context) {
	financeProxyGet(c, "/transactions")
}

// GetFinanceMonthlySummaryHandler proxies GET /summary/monthly
func GetFinanceMonthlySummaryHandler(c *gin.Context) {
	financeProxyGet(c, "/summary/monthly")
}

// GetFinanceOverviewHandler proxies GET /summary/overview
func GetFinanceOverviewHandler(c *gin.Context) {
	financeProxyGet(c, "/summary/overview")
}

// ListFinanceCategoriesHandler proxies GET /categories
func ListFinanceCategoriesHandler(c *gin.Context) {
	financeProxyGet(c, "/categories")
}

// CreateFinanceCategoryHandler proxies POST /categories
func CreateFinanceCategoryHandler(c *gin.Context) {
	financeProxyPost(c, "/categories")
}

// DeleteFinanceCategoryHandler proxies DELETE /categories/:id
func DeleteFinanceCategoryHandler(c *gin.Context) {
	id := c.Param("id")
	financeProxyDelete(c, fmt.Sprintf("/categories/%s", id))
}

// ReEnrichFinanceHandler proxies POST /re-enrich
func ReEnrichFinanceHandler(c *gin.Context) {
	financeProxyPost(c, "/re-enrich")
}

// ListFinanceAccountsHandler proxies GET /accounts
func ListFinanceAccountsHandler(c *gin.Context) {
	financeProxyGet(c, "/accounts")
}

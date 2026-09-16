package handlers

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
)

// 30s was an enormous margin when extraction was a sub-second text-layer read.
// Since #5907 shipped OCR it is not: a 4-page scan is ~16-24s uncontended and
// stretches past 30s under concurrent OCR (CPU-bound, no GPU), which surfaced
// as an intermittent 502 on a valid upload (#5919, jenny). 300s matches the
// advisor client's margin for a comparably slow upstream. This is a timeout,
// not a capacity plan — see the row for the concurrency-bounding disposition.
var commanderClient = newProxyClient("commander.valuation", 300*time.Second)

// decideClient is deliberately far tighter than commanderClient's 300s (#5920).
// /decide is the fast pre-flight the client calls BEFORE the blocking /extract to
// announce OCR and estimate it; being quick is its whole value. Because the client
// awaits /decide and THEN /extract, every second /decide spends is added in front
// of extraction — so a slow or stuck /decide must fail fast into the client's #306
// fallback (generic phases, no announcement) rather than delay the extract behind
// it. Deployed /decide is ~2.3s (fitz-only, commander PR #40); 15s absorbs
// CPU-contention slowdown under concurrent OCR while still failing well inside
// extract's budget. Re-measurable, not a budget any implementation inherits.
var decideClient = newProxyClient("commander.decide", 15*time.Second)

// respondCommanderError maps a failed commander call to a status the user can
// read correctly (#5919). A timeout is "the extraction took too long", not "the
// server refused" — the operator's first reading of the 502 was that jenny's
// file was at fault. A genuine connection failure stays a 502.
func respondCommanderError(c *gin.Context, err error) {
	log.Printf("Failed to reach commander: %v", err)
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		RespondWithError(c, http.StatusGatewayTimeout,
			"Underlaget tog för lång tid att läsa. Skannade PDF:er kan ta upp till en minut — försök igen.")
		return
	}
	RespondWithError(c, http.StatusBadGateway, "Commander service unavailable")
}

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
		respondCommanderError(c, err)
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
		respondCommanderError(c, err)
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
		respondCommanderError(c, err)
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
		respondCommanderError(c, err)
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
		respondCommanderError(c, err)
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
		respondCommanderError(c, err)
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
	commanderProxyPassthroughVia(c, commanderClient, method, path)
}

// commanderProxyPassthroughVia is commanderProxyPassthrough with the upstream
// client made explicit, so /decide can run on decideClient's tighter deadline
// instead of the 300s extract client (#5920). Everything else — query string,
// identity propagation, header and body streaming — is identical.
func commanderProxyPassthroughVia(c *gin.Context, client *http.Client, method string, path string) {
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

	// Propagate the authenticated caller's identity so commander can scope
	// per-owner (security-finding #3096: the proxy previously dropped identity
	// entirely, leaving commander structurally unable to authorise). The value
	// comes from the server-verified user_id set by AuthMiddleware — never from
	// the client. Because req is built fresh above (only Content-Type is copied
	// across), any inbound client-supplied X-Aspirant-User-Id is not forwarded;
	// setting it here from the authed id makes forgery impossible by construction.
	if uid, ok := c.Get("user_id"); ok {
		req.Header.Set("X-Aspirant-User-Id", fmt.Sprintf("%v", uid))
	}

	resp, err := client.Do(req)
	if err != nil {
		respondCommanderError(c, err)
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

// PostCommanderValuationExtractAsyncHandler proxies POST
// /valuation-statement/extract-async — the async submit (system_3 #5977/#5973).
// It returns a job id at once so the request never outlives the ~100s Cloudflare
// edge that severs a slow synchronous /extract (#5969); commander runs the
// extraction in the background and the client polls GET /jobs/:id. Runs on the
// same 300s commanderClient as /extract (the submit itself is fast) so the hop is
// instrumented in proxy_request_log under commander.valuation (#5953).
func PostCommanderValuationExtractAsyncHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "POST", "/valuation-statement/extract-async")
}

// GetCommanderValuationJobHandler proxies GET /valuation-statement/jobs/:id — the
// async extraction poll (#5977). Streams commander's {status, result?, error?}
// (and its 404 for an unknown id) back verbatim.
func GetCommanderValuationJobHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "GET", fmt.Sprintf("/valuation-statement/jobs/%s", c.Param("id")))
}

// PostCommanderValuationDecideHandler proxies POST /valuation-statement/decide,
// the OCR pre-flight (#5915) whose server route was missing so every client call
// 404'd into the silent #306 fallback — a shipped feature dark in prod with green
// tests on both sides (#5920). Runs on decideClient's short ceiling, not the 300s
// extract client, so a slow /decide fails fast into that same fallback instead of
// delaying the extract that follows it.
func PostCommanderValuationDecideHandler(c *gin.Context) {
	commanderProxyPassthroughVia(c, decideClient, "POST", "/valuation-statement/decide")
}

// PostCommanderValuationGenerateHandler proxies POST /valuation-statement/generate.
// JSON body of reviewed values → docx file download.
func PostCommanderValuationGenerateHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "POST", "/valuation-statement/generate")
}

// GetCommanderValuationOperatorDefaultsHandler proxies GET
// /valuation-statement/operator-defaults. The client reads it to pre-fill the
// manual-entry form (#5914); like PutCommander… it was registered on the server,
// but only the PUT was — the GET 404'd, so manual entry always opened with empty
// defaults, the same missing-proxy-hop class as /decide (surfaced by the route
// agreement test, #5920). Member-tier: the block it returns (appraiser identity +
// likviditet) is exactly what /extract already embeds for a Member, so it exposes
// nothing new. Registered on trustedRoutes, not adminRoutes (the write stays
// Admin-only, #3182).
func GetCommanderValuationOperatorDefaultsHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "GET", "/valuation-statement/operator-defaults")
}

// PutCommanderValuationOperatorDefaultsHandler proxies PUT
// /valuation-statement/operator-defaults. Persists the appraiser-identity
// fields the review step pre-fills.
func PutCommanderValuationOperatorDefaultsHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "PUT", "/valuation-statement/operator-defaults")
}

// ---------- processed-valuations store (system_3 #1154-B1) ----------
//
// Backs the 'Tidigare värderingar' tab in the Värdeutlåtande tool:
// every persisted processing iteration is reached through these proxy
// routes. Each handler is a thin pass-through to commander's
// /valuation-statement/processed* endpoints — no business logic.

// PostCommanderValuationProcessedHandler proxies POST /valuation-statement/processed.
// Body is the extracted + final values + input file refs; commander persists the row.
func PostCommanderValuationProcessedHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "POST", "/valuation-statement/processed")
}

// ListCommanderValuationProcessedHandler proxies GET /valuation-statement/processed.
// Pagination (limit, offset) rides via the preserved query string.
func ListCommanderValuationProcessedHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "GET", "/valuation-statement/processed")
}

// ExportCommanderValuationProcessedCsvHandler proxies GET
// /valuation-statement/processed/export.csv. Registered before the :id route
// so the literal 'export.csv' segment wins over the wildcard param matcher.
func ExportCommanderValuationProcessedCsvHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "GET", "/valuation-statement/processed/export.csv")
}

// GetCommanderValuationProcessedHandler proxies GET /valuation-statement/processed/:id.
func GetCommanderValuationProcessedHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "GET", fmt.Sprintf("/valuation-statement/processed/%s", c.Param("id")))
}

// UpdateCommanderValuationProcessedHandler proxies PATCH /valuation-statement/processed/:id.
// Edit-in-place — mutates the existing row.
func UpdateCommanderValuationProcessedHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "PATCH", fmt.Sprintf("/valuation-statement/processed/%s", c.Param("id")))
}

// DeleteCommanderValuationProcessedHandler proxies DELETE /valuation-statement/processed/:id.
func DeleteCommanderValuationProcessedHandler(c *gin.Context) {
	commanderProxyPassthrough(c, "DELETE", fmt.Sprintf("/valuation-statement/processed/%s", c.Param("id")))
}

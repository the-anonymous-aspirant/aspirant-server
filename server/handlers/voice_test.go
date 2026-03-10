package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
)

// newTestTranscriber creates a fake transcriber HTTP server.
func newTestTranscriber(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// setTranscriberURL points the proxy at the test server.
func setTranscriberURL(t *testing.T, url string) {
	t.Helper()
	os.Setenv("TRANSCRIBER_URL", url)
	t.Cleanup(func() { os.Unsetenv("TRANSCRIBER_URL") })
}

// newRouter builds a minimal Gin router with the handler under test.
func newRouter(method, path string, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	switch method {
	case "GET":
		r.GET(path, handler)
	case "POST":
		r.POST(path, handler)
	case "DELETE":
		r.DELETE(path, handler)
	}
	return r
}

// --- Import / compile test ---

func TestVoiceHandlersExist(t *testing.T) {
	// Verify all handler functions are non-nil (compile check).
	handlers := []gin.HandlerFunc{
		ListVoiceMessagesHandler,
		GetVoiceMessageHandler,
		UploadVoiceMessageHandler,
		DeleteVoiceMessageHandler,
		GetVoiceAudioHandler,
	}
	for i, h := range handlers {
		if h == nil {
			t.Errorf("handler %d is nil", i)
		}
	}
}

// --- Contract tests ---

func TestListVoiceMessages_ProxiesResponse(t *testing.T) {
	payload := `{"items":[],"total":0,"page":1,"page_size":20}`
	ts := newTestTranscriber(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/voice-messages" {
			t.Errorf("expected path /voice-messages, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(payload))
	})
	defer ts.Close()
	setTranscriberURL(t, ts.URL)

	router := newRouter("GET", "/voice-messages", ListVoiceMessagesHandler)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/voice-messages", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != payload {
		t.Errorf("expected proxied body, got %s", w.Body.String())
	}
}

func TestGetVoiceMessage_ProxiesResponse(t *testing.T) {
	ts := newTestTranscriber(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/voice-messages/abc-123" {
			t.Errorf("expected path /voice-messages/abc-123, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"abc-123","status":"completed"}`))
	})
	defer ts.Close()
	setTranscriberURL(t, ts.URL)

	router := newRouter("GET", "/voice-messages/:id", GetVoiceMessageHandler)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/voice-messages/abc-123", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestDeleteVoiceMessage_Returns204(t *testing.T) {
	ts := newTestTranscriber(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer ts.Close()
	setTranscriberURL(t, ts.URL)

	router := newRouter("DELETE", "/voice-messages/:id", DeleteVoiceMessageHandler)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/voice-messages/abc-123", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", w.Code)
	}
}

func TestListVoiceMessages_TranscriberDown_Returns502(t *testing.T) {
	// Point at a URL that will refuse connections.
	setTranscriberURL(t, "http://127.0.0.1:1")

	router := newRouter("GET", "/voice-messages", ListVoiceMessagesHandler)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/voice-messages", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", w.Code)
	}

	var body ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error == "" {
		t.Error("expected error message in response body")
	}
}

func TestTranscriberURL_DefaultsWhenUnset(t *testing.T) {
	os.Unsetenv("TRANSCRIBER_URL")
	url := transcriberURL()
	if url != "http://transcriber:8000" {
		t.Errorf("expected default URL, got %s", url)
	}
}

func TestTranscriberURL_ReadsEnvVar(t *testing.T) {
	setTranscriberURL(t, "http://custom:9999")
	url := transcriberURL()
	if url != "http://custom:9999" {
		t.Errorf("expected custom URL, got %s", url)
	}
}

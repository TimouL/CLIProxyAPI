package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestProbeAuthFilesProxyEgress_InvalidBody_Returns400(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	handler := &Handler{authManager: manager}

	router := gin.New()
	router.POST("/v0/management/auth-files/proxy-egress", handler.ProbeAuthFilesProxyEgress)

	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/proxy-egress", bytes.NewBufferString("{"))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, resp.Code)
	}
}

func TestProbeAuthFilesProxyEgress_ReturnsPerAuthErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "no-proxy",
		Provider: "test",
		ProxyURL: "",
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	handler := &Handler{authManager: manager}

	router := gin.New()
	router.POST("/v0/management/auth-files/proxy-egress", handler.ProbeAuthFilesProxyEgress)

	reqBody, err := json.Marshal(map[string]any{"auth_ids": []string{"missing", "no-proxy"}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/proxy-egress", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, resp.Code)
	}

	var payload struct {
		Results map[string]struct {
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if payload.Results["missing"].Error != "auth not found" {
		t.Fatalf("expected missing auth error %q, got %q", "auth not found", payload.Results["missing"].Error)
	}
	if payload.Results["no-proxy"].Error != "proxy_url not configured" {
		t.Fatalf("expected no-proxy error %q, got %q", "proxy_url not configured", payload.Results["no-proxy"].Error)
	}
}

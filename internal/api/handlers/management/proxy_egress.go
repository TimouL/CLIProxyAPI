package management

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/proxy"
)

const (
	proxyEgressMaxAuthIDs      = 100
	proxyEgressProbeTimeout    = 10 * time.Second
	proxyEgressMaxTraceBodyLen = 64 * 1024
	proxyEgressMaxConcurrency  = 8
)

var cloudflareTraceURL = "https://1.1.1.1/cdn-cgi/trace"

type probeAuthFilesProxyEgressRequest struct {
	AuthIDs    []string `json:"auth_ids"`
	AuthIDsAlt []string `json:"authIds"`
}

type probeAuthFilesProxyEgressResponse struct {
	Results map[string]probeAuthFilesProxyEgressResult `json:"results"`
}

type probeAuthFilesProxyEgressResult struct {
	IP        string `json:"ip,omitempty"`
	Loc       string `json:"loc,omitempty"`
	Colo      string `json:"colo,omitempty"`
	RTTMs     int64  `json:"rtt_ms,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (h *Handler) ProbeAuthFilesProxyEgress(c *gin.Context) {
	var req probeAuthFilesProxyEgressRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	authIDs := req.AuthIDs
	if len(authIDs) == 0 && len(req.AuthIDsAlt) > 0 {
		authIDs = req.AuthIDsAlt
	}
	if len(authIDs) > proxyEgressMaxAuthIDs {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("too many auth_ids (max %d)", proxyEgressMaxAuthIDs)})
		return
	}

	results := make(map[string]probeAuthFilesProxyEgressResult, len(authIDs))
	if len(authIDs) == 0 {
		c.JSON(http.StatusOK, probeAuthFilesProxyEgressResponse{Results: results})
		return
	}

	sem := make(chan struct{}, proxyEgressMaxConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, id := range authIDs {
		authID := strings.TrimSpace(id)
		if authID == "" {
			continue
		}
		wg.Add(1)
		go func(authID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := h.probeSingleAuthProxyEgress(c.Request.Context(), authID)
			mu.Lock()
			results[authID] = res
			mu.Unlock()
		}(authID)
	}

	wg.Wait()
	c.JSON(http.StatusOK, probeAuthFilesProxyEgressResponse{Results: results})
}

func (h *Handler) probeSingleAuthProxyEgress(ctx context.Context, authID string) probeAuthFilesProxyEgressResult {
	manager := h
	if manager == nil || manager.authManager == nil {
		return probeAuthFilesProxyEgressResult{Error: "auth manager not configured"}
	}

	auth, ok := manager.authManager.GetByID(authID)
	if !ok || auth == nil {
		return probeAuthFilesProxyEgressResult{Error: "auth not found"}
	}

	proxyStr := strings.TrimSpace(auth.ProxyURL)
	if proxyStr == "" {
		return probeAuthFilesProxyEgressResult{Error: "proxy_url not configured"}
	}

	ip, loc, colo, rttMs, checkedAt, err := probeCloudflareTraceViaProxy(ctx, proxyStr, cloudflareTraceURL)
	if err != nil {
		return probeAuthFilesProxyEgressResult{Error: err.Error()}
	}

	return probeAuthFilesProxyEgressResult{
		IP:        ip,
		Loc:       loc,
		Colo:      colo,
		RTTMs:     rttMs,
		CheckedAt: checkedAt.UTC().Format(time.RFC3339),
	}
}

func probeCloudflareTraceViaProxy(ctx context.Context, proxyStr, traceURL string) (string, string, string, int64, time.Time, error) {
	transport, err := buildProxyTransportForProxyEgress(proxyStr)
	if err != nil {
		return "", "", "", 0, time.Time{}, err
	}

	client := &http.Client{
		Timeout:   proxyEgressProbeTimeout,
		Transport: transport,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, traceURL, nil)
	if err != nil {
		return "", "", "", 0, time.Time{}, fmt.Errorf("failed to build request")
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", 0, time.Time{}, fmt.Errorf("proxy request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", "", 0, time.Time{}, fmt.Errorf("trace request failed")
	}

	limited := io.LimitReader(resp.Body, proxyEgressMaxTraceBodyLen)
	body, err := io.ReadAll(limited)
	if err != nil {
		return "", "", "", 0, time.Time{}, fmt.Errorf("failed to read response")
	}

	rttMs := time.Since(start).Milliseconds()
	checkedAt := time.Now().UTC()

	ip, loc, colo := parseCloudflareTrace(body)
	if ip == "" || loc == "" || colo == "" {
		return "", "", "", 0, time.Time{}, fmt.Errorf("trace parse failed")
	}

	return ip, loc, colo, rttMs, checkedAt, nil
}

func buildProxyTransportForProxyEgress(proxyStr string) (*http.Transport, error) {
	proxyStr = strings.TrimSpace(proxyStr)
	if proxyStr == "" {
		return nil, fmt.Errorf("invalid proxy url")
	}

	parsed, err := url.Parse(proxyStr)
	if err != nil || strings.TrimSpace(parsed.Scheme) == "" || strings.TrimSpace(parsed.Host) == "" {
		return nil, fmt.Errorf("invalid proxy url")
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return &http.Transport{Proxy: http.ProxyURL(parsed)}, nil
	case "socks5":
		var proxyAuth *proxy.Auth
		if parsed.User != nil {
			username := parsed.User.Username()
			password, _ := parsed.User.Password()
			if username != "" || password != "" {
				proxyAuth = &proxy.Auth{User: username, Password: password}
			}
		}
		dialer, err := proxy.SOCKS5("tcp", parsed.Host, proxyAuth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("failed to configure proxy dialer")
		}
		return &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme")
	}
}

func parseCloudflareTrace(body []byte) (string, string, string) {
	var ip, loc, colo string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024), proxyEgressMaxTraceBodyLen)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "ip":
			ip = value
		case "loc":
			loc = value
		case "colo":
			colo = value
		}
	}
	return ip, loc, colo
}

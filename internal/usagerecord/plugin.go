package usagerecord

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// Plugin implements coreusage.Plugin to persist usage records to SQLite.
// It captures request/response details from the gin context and stores them
// in the database for later analysis.
type Plugin struct {
	store                *Store
	enabled              atomic.Bool
	tokenIncrementor     TokenIncrementor
	usageIncrementor     UsageIncrementor
	candidateIncrementor CandidateIncrementor
}

// TokenIncrementor is a callback function type for incrementing API key token counts.
// It takes the API key string and input/output token counts.
type TokenIncrementor func(apiKey string, inputTokens, outputTokens int64)

// UsageIncrementor is a callback function type for incrementing API key usage counts.
// It takes the API key string to increment its usage count and update last used time.
type UsageIncrementor func(apiKey string)

// CandidateIncrementor is a callback function type for recording request candidates.
// It takes the request ID, candidate information for tracing request routing.
type CandidateIncrementor func(requestID string, provider string, apiKey string, status string, statusCode int, success bool, durationMs int64, errorMessage string, candidateIndex int, retryIndex int)

var (
	defaultPlugin     *Plugin
	defaultPluginOnce sync.Once
)

// DefaultPlugin returns the global plugin instance.
func DefaultPlugin() *Plugin {
	defaultPluginOnce.Do(func() {
		defaultPlugin = &Plugin{}
		defaultPlugin.enabled.Store(true)
	})
	return defaultPlugin
}

// NewPlugin creates a new usage record plugin.
func NewPlugin(store *Store) *Plugin {
	p := &Plugin{store: store}
	p.enabled.Store(true)
	return p
}

// SetStore sets the store for the default plugin.
func SetStore(store *Store) {
	DefaultPlugin().store = store
}

// SetTokenIncrementor sets the callback function for incrementing API key token counts.
func SetTokenIncrementor(fn TokenIncrementor) {
	DefaultPlugin().tokenIncrementor = fn
}

// SetUsageIncrementor sets the callback function for incrementing API key usage counts.
func SetUsageIncrementor(fn UsageIncrementor) {
	DefaultPlugin().usageIncrementor = fn
}

// SetCandidateIncrementor sets the callback function for recording request candidates.
func SetCandidateIncrementor(fn CandidateIncrementor) {
	DefaultPlugin().candidateIncrementor = fn
}

// SetEnabled enables or disables the plugin.
func (p *Plugin) SetEnabled(enabled bool) {
	if p == nil {
		return
	}
	p.enabled.Store(enabled)
}

// Enabled returns whether the plugin is enabled.
func (p *Plugin) Enabled() bool {
	if p == nil {
		return false
	}
	return p.enabled.Load()
}

// HandleUsage implements coreusage.Plugin.
// It creates a usage record from the provided data and stores it in the database.
func (p *Plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || p.store == nil {
		return
	}
	if !p.enabled.Load() {
		return
	}

	// Extract additional info from gin context if available
	var (
		requestURL      string
		requestMethod   string
		requestHeaders  = make(map[string]string)
		responseHeaders = make(map[string]string)
		statusCode      int
		requestBody     string
		responseBody    string
		isStreaming     bool
		durationMs      int64
		recordDBID      int64
		requestID       string
		ip              string
	)

	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
			ip = ginCtx.ClientIP()
			if ginCtx.Request != nil {
				requestURL = ginCtx.Request.URL.String()
				requestMethod = ginCtx.Request.Method

				// Copy headers (mask sensitive ones)
				for key, values := range ginCtx.Request.Header {
					if len(values) > 0 {
						value := values[0]
						if isSensitiveHeader(key) {
							value = maskValue(value)
						}
						requestHeaders[key] = value
					}
				}
			}

			statusCode = ginCtx.Writer.Status()

			// Capture response headers (mask sensitive ones)
			for key, values := range ginCtx.Writer.Header() {
				if len(values) > 0 {
					value := values[0]
					if isSensitiveHeader(key) {
						value = maskValue(value)
					}
					responseHeaders[key] = value
				}
			}

			// Get request ID using standard utility
			requestID = logging.GetGinRequestID(ginCtx)

			// Try to associate with the start-record inserted by GinUsageRecordMiddleware.
			if v, exists := ginCtx.Get(ginUsageRecordIDKey); exists {
				switch t := v.(type) {
				case int64:
					recordDBID = t
				case int:
					recordDBID = int64(t)
				case float64:
					recordDBID = int64(t)
				case string:
					if parsed, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
						recordDBID = parsed
					}
				}
			}

			// Check if streaming from context
			if streaming, exists := ginCtx.Get("is_streaming"); exists {
				if streamBool, ok := streaming.(bool); ok {
					isStreaming = streamBool
				}
			}

			// Get duration from context if available, otherwise calculate from start time
			if startTime, exists := ginCtx.Get("request_start_time"); exists {
				if st, ok := startTime.(time.Time); ok {
					durationMs = time.Since(st).Milliseconds()
				}
			}
			if durationMs == 0 && !record.RequestedAt.IsZero() {
				durationMs = time.Since(record.RequestedAt).Milliseconds()
			}

			// Get cached request/response bodies if available (完整存储，不截断)
			if body, exists := ginCtx.Get("request_body_for_log"); exists {
				if bodyBytes, ok := body.([]byte); ok {
					requestBody = string(bodyBytes) // 完整存储，不截断
				}
			}
			if body, exists := ginCtx.Get("response_body_for_log"); exists {
				if bodyBytes, ok := body.([]byte); ok {
					responseBody = string(bodyBytes) // 完整存储，不截断
				}
			}
		}
	}

	// Fallback for timestamp
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	// Determine success
	success := !record.Failed
	if statusCode >= 400 {
		success = false
	}

	// If GinUsageRecordMiddleware already inserted a start-record, update it in-place instead of inserting a duplicate.
	patched := false
	if recordDBID > 0 {
		apiKey := record.APIKey
		apiKeyMasked := MaskAPIKey(apiKey)
		model := record.Model
		provider := record.Provider
		inputTokens := record.Detail.InputTokens
		outputTokens := record.Detail.OutputTokens
		totalTokens := record.Detail.InputTokens + record.Detail.OutputTokens
		cachedTokens := record.Detail.CachedTokens
		reasoningTokens := record.Detail.ReasoningTokens

		patch := RecordPatch{
			APIKey:          &apiKey,
			APIKeyMasked:    &apiKeyMasked,
			Model:           &model,
			Provider:        &provider,
			IsStreaming:     &isStreaming,
			InputTokens:     &inputTokens,
			OutputTokens:    &outputTokens,
			TotalTokens:     &totalTokens,
			CachedTokens:    &cachedTokens,
			ReasoningTokens: &reasoningTokens,
		}

		patchCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := p.store.PatchByID(patchCtx, recordDBID, patch)
		cancel()
		if err == nil {
			// token usage & provider/model updated; rely on GinUsageRecordMiddleware to patch status/body later.
			patched = true
		}
	}

	if !patched {
		// Fallback: insert a new row (legacy behavior).
		// Always calculate total tokens as input + output (API-returned total may be inaccurate)
		rec := &Record{
			RequestID:       requestID,
			Timestamp:       timestamp,
			IP:              ip,
			APIKey:          record.APIKey,
			APIKeyMasked:    MaskAPIKey(record.APIKey),
			Model:           record.Model,
			Provider:        record.Provider,
			IsStreaming:     isStreaming,
			InputTokens:     record.Detail.InputTokens,
			OutputTokens:    record.Detail.OutputTokens,
			TotalTokens:     record.Detail.InputTokens + record.Detail.OutputTokens,
			CachedTokens:    record.Detail.CachedTokens,
			ReasoningTokens: record.Detail.ReasoningTokens,
			DurationMs:      durationMs,
			StatusCode:      statusCode,
			Success:         success,
			RequestURL:      requestURL,
			RequestMethod:   requestMethod,
			RequestHeaders:  requestHeaders,
			RequestBody:     requestBody,
			ResponseHeaders: responseHeaders,
			ResponseBody:    responseBody,
		}

		p.store.EnqueueUsageRecord(rec)
	}

	// Increment API key token counts if callback is set
	if p.tokenIncrementor != nil && record.APIKey != "" {
		inputTokens := record.Detail.InputTokens
		outputTokens := record.Detail.OutputTokens
		if inputTokens > 0 || outputTokens > 0 {
			p.tokenIncrementor(record.APIKey, inputTokens, outputTokens)
		}
	}

	// Increment API key usage count and update last used time
	if p.usageIncrementor != nil && record.APIKey != "" {
		p.usageIncrementor(record.APIKey)
	}
}

// isSensitiveHeader returns true for headers that should be masked.
func isSensitiveHeader(key string) bool {
	lower := strings.ToLower(key)
	sensitivePatterns := []string{
		"authorization",
		"x-api-key",
		"api-key",
		"x-goog-api-key",
		"cookie",
		"set-cookie",
		"x-management-key",
	}
	for _, pattern := range sensitivePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// maskValue masks a sensitive value, showing only first and last few characters.
func maskValue(value string) string {
	if len(value) <= 8 {
		return strings.Repeat("*", len(value))
	}
	return value[:4] + "..." + value[len(value)-4:]
}

// truncateBody truncates body content to a maximum length.
func truncateBody(body string, maxLen int) string {
	if len(body) <= maxLen {
		return body
	}
	return body[:maxLen] + "\n...[truncated]"
}

// Register registers the default plugin with the core usage manager.
func Register() {
	coreusage.RegisterPlugin(DefaultPlugin())
}

// RecordCandidate records a request candidate for tracing purposes.
// This should be called during request routing to track all attempts.
func RecordCandidate(requestID string, provider string, apiKey string, status string, statusCode int, success bool, durationMs int64, errorMessage string, candidateIndex int, retryIndex int) {
	plugin := DefaultPlugin()
	if plugin == nil || plugin.store == nil || !plugin.enabled.Load() {
		return
	}

	candidate := &RequestCandidate{
		RequestID:      requestID,
		Timestamp:      time.Now(),
		Provider:       provider,
		APIKey:         apiKey,
		APIKeyMasked:   MaskAPIKey(apiKey),
		Status:         status,
		StatusCode:     statusCode,
		Success:        success,
		DurationMs:     durationMs,
		ErrorMessage:   errorMessage,
		CandidateIndex: candidateIndex,
		RetryIndex:     retryIndex,
	}

	plugin.store.EnqueueRequestCandidate(candidate)

	// Call the callback if set
	if plugin.candidateIncrementor != nil {
		plugin.candidateIncrementor(requestID, provider, apiKey, status, statusCode, success, durationMs, errorMessage, candidateIndex, retryIndex)
	}
}

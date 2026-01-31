package usagerecord

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	ginUsageRecordIDKey   = "__usage_record_id__"
	ginUsageRecordStartKey = "request_start_time"
	ginUsageRecordPanicKey = "__usage_record_panic__"
)

// GinUsageRecordMiddleware records a usage record at request start and patches it at request end.
// This guarantees that failed requests (including 5xx) are still visible in the usage records UI.
func GinUsageRecordMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		store := DefaultStore()
		if store == nil || c == nil || c.Request == nil {
			c.Next()
			return
		}

		// Only track AI API requests that have request_id assigned by GinLogrusLogger.
		requestID := logging.GetGinRequestID(c)
		if requestID == "" {
			c.Next()
			return
		}

		// Skip common non-business requests.
		switch c.Request.Method {
		case http.MethodGet, http.MethodOptions:
			c.Next()
			return
		}

		start := time.Now()
		c.Set(ginUsageRecordStartKey, start)

		// Capture request info (mask query).
		requestURL := c.Request.URL.Path
		if masked := util.MaskSensitiveQuery(c.Request.URL.RawQuery); masked != "" {
			requestURL += "?" + masked
		}

		requestBody := captureRequestBodyBestEffort(c)
		requestHeaders := captureHeadersBestEffort(c.Request.Header)

		model := extractModelBestEffort(c.Request.URL.Path, requestBody)

		rec := &Record{
			RequestID:      requestID,
			Timestamp:      start,
			IP:             c.ClientIP(),
			Model:          model,
			Provider:       "pending",
			IsStreaming:    false,
			InputTokens:    0,
			OutputTokens:   0,
			TotalTokens:    0,
			CachedTokens:   0,
			ReasoningTokens: 0,
			DurationMs:     0,
			StatusCode:     0,
			Success:        true,
			RequestURL:     requestURL,
			RequestMethod:  c.Request.Method,
			RequestHeaders: requestHeaders,
			RequestBody:    string(requestBody),
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		errInsert := store.Insert(ctx, rec)
		cancel()
		if errInsert != nil {
			log.WithError(errInsert).Warn("usage record: failed to insert start record")
		} else {
			c.Set(ginUsageRecordIDKey, rec.ID)
		}

		defer func() {
			recovered := recover()
			if recovered != nil {
				c.Set(ginUsageRecordPanicKey, fmt.Sprint(recovered))
			}

			if rec.ID > 0 {
				patchUsageRecordFinal(store, c, rec.ID, start, requestURL, requestHeaders, requestBody, recovered != nil)
			}

			if recovered != nil {
				panic(recovered)
			}
		}()

		c.Next()
	}
}

func captureRequestBodyBestEffort(c *gin.Context) []byte {
	if c == nil {
		return nil
	}
	if v, exists := c.Get("request_body_for_log"); exists {
		if bodyBytes, ok := v.([]byte); ok && len(bodyBytes) > 0 {
			return bodyBytes
		}
	}

	if c.Request == nil || c.Request.Body == nil {
		return nil
	}
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil
	}
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	return bodyBytes
}

func captureHeadersBestEffort(headers http.Header) map[string]string {
	out := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		value := values[0]
		if isSensitiveHeader(key) {
			value = maskValue(value)
		}
		out[key] = value
	}
	return out
}

func extractModelBestEffort(path string, body []byte) string {
	trimmedPath := strings.TrimSpace(path)
	if strings.HasPrefix(trimmedPath, "/v1beta/models/") {
		rest := strings.TrimPrefix(trimmedPath, "/v1beta/models/")
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" {
			return ""
		}
		if idx := strings.Index(rest, "/"); idx >= 0 {
			rest = rest[:idx]
		}
		if idx := strings.Index(rest, ":"); idx >= 0 {
			rest = rest[:idx]
		}
		return strings.TrimSpace(rest)
	}

	if len(body) == 0 {
		return ""
	}
	value := gjson.GetBytes(body, "model").String()
	return strings.TrimSpace(value)
}

func patchUsageRecordFinal(store *Store, c *gin.Context, recordID int64, start time.Time, requestURL string, requestHeaders map[string]string, requestBody []byte, recovered bool) {
	if store == nil || c == nil || recordID <= 0 {
		return
	}

	durationMs := time.Since(start).Milliseconds()

	statusCode := c.Writer.Status()
	if recovered {
		statusCode = http.StatusInternalServerError
	}

	success := statusCode > 0 && statusCode < http.StatusBadRequest

	apiKey := ""
	if v, exists := c.Get("apiKey"); exists {
		if s, ok := v.(string); ok {
			apiKey = s
		} else {
			apiKey = fmt.Sprint(v)
		}
	}

	isStreaming := false
	if v, exists := c.Get("is_streaming"); exists {
		if b, ok := v.(bool); ok {
			isStreaming = b
		}
	}
	if !isStreaming {
		contentType := c.Writer.Header().Get("Content-Type")
		if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
			isStreaming = true
		}
	}

	respHeaders := captureHeadersBestEffort(c.Writer.Header())

	responseBody := extractResponseBodyBestEffort(c, recovered)

	ip := c.ClientIP()
	apiKeyMasked := ""
	if strings.TrimSpace(apiKey) != "" {
		apiKeyMasked = MaskAPIKey(apiKey)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := store.PatchByID(ctx, recordID, RecordPatch{
		IP:              &ip,
		APIKey:          &apiKey,
		APIKeyMasked:    &apiKeyMasked,
		IsStreaming:     &isStreaming,
		DurationMs:      &durationMs,
		StatusCode:      &statusCode,
		Success:         &success,
		RequestURL:      &requestURL,
		RequestMethod:   &c.Request.Method,
		RequestHeaders:  &requestHeaders,
		RequestBody:     ptrString(string(requestBody)),
		ResponseHeaders: &respHeaders,
		ResponseBody:    &responseBody,
	})
	if err != nil {
		log.WithError(err).Warn("usage record: failed to patch final record")
	}
}

func extractResponseBodyBestEffort(c *gin.Context, recovered bool) string {
	if c == nil {
		return ""
	}

	// Prefer actual response body captured by response writer wrapper.
	if v, exists := c.Get("response_body_for_log"); exists {
		switch t := v.(type) {
		case []byte:
			if len(t) > 0 {
				return string(t)
			}
		case string:
			if strings.TrimSpace(t) != "" {
				return t
			}
		}
	}

	// Fallback: upstream API response (may be useful when the proxy returned an error early).
	if v, exists := c.Get("API_RESPONSE"); exists {
		if b, ok := v.([]byte); ok && len(bytes.TrimSpace(b)) > 0 {
			return string(b)
		}
	}

	// Fallback: panic recovered value.
	if recovered {
		if v, exists := c.Get(ginUsageRecordPanicKey); exists {
			msg := strings.TrimSpace(fmt.Sprint(v))
			if msg != "" {
				return msg
			}
		}
	}

	// Fallback: gin errors.
	if c.Errors != nil && len(c.Errors) > 0 {
		msg := strings.TrimSpace(c.Errors.String())
		if msg != "" {
			return msg
		}
	}

	// Ensure error responses have something to inspect even when upstream didn't provide a body.
	status := 0
	if c.Writer != nil {
		status = c.Writer.Status()
	}
	if recovered {
		status = http.StatusInternalServerError
	}
	if status >= http.StatusBadRequest {
		text := strings.TrimSpace(http.StatusText(status))
		if text != "" {
			return fmt.Sprintf("HTTP %d %s", status, text)
		}
		return fmt.Sprintf("HTTP %d", status)
	}

	return ""
}

func ptrString(s string) *string { return &s }

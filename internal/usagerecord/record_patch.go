package usagerecord

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RecordPatch describes a partial update for an existing usage record.
// Any nil field is ignored.
type RecordPatch struct {
	Timestamp       *time.Time
	IP              *string
	APIKey          *string
	APIKeyMasked    *string
	Model           *string
	Provider        *string
	IsStreaming     *bool
	InputTokens     *int64
	OutputTokens    *int64
	TotalTokens     *int64
	CachedTokens    *int64
	ReasoningTokens *int64
	DurationMs      *int64
	StatusCode      *int
	Success         *bool
	RequestURL      *string
	RequestMethod   *string
	RequestHeaders  *map[string]string
	RequestBody     *string
	ResponseHeaders *map[string]string
	ResponseBody    *string
}

func (s *Store) PatchByID(ctx context.Context, id int64, patch RecordPatch) (int64, error) {
	if s.isClosed() {
		return 0, fmt.Errorf("store is closed")
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid record id")
	}

	var (
		sets []string
		args []any
	)
	add := func(set string, arg any) {
		sets = append(sets, set)
		args = append(args, arg)
	}

	if patch.Timestamp != nil {
		add("timestamp = ?", patch.Timestamp.Format(time.RFC3339))
	}
	if patch.IP != nil {
		add("ip = ?", *patch.IP)
	}
	if patch.APIKey != nil {
		add("api_key = ?", *patch.APIKey)
	}
	if patch.APIKeyMasked != nil {
		add("api_key_masked = ?", *patch.APIKeyMasked)
	}
	if patch.Model != nil {
		add("model = ?", *patch.Model)
	}
	if patch.Provider != nil {
		add("provider = ?", *patch.Provider)
	}
	if patch.IsStreaming != nil {
		val := 0
		if *patch.IsStreaming {
			val = 1
		}
		add("is_streaming = ?", val)
	}
	if patch.InputTokens != nil {
		add("input_tokens = ?", *patch.InputTokens)
	}
	if patch.OutputTokens != nil {
		add("output_tokens = ?", *patch.OutputTokens)
	}
	if patch.TotalTokens != nil {
		add("total_tokens = ?", *patch.TotalTokens)
	}
	if patch.CachedTokens != nil {
		add("cached_tokens = ?", *patch.CachedTokens)
	}
	if patch.ReasoningTokens != nil {
		add("reasoning_tokens = ?", *patch.ReasoningTokens)
	}
	if patch.DurationMs != nil {
		add("duration_ms = ?", *patch.DurationMs)
	}
	if patch.StatusCode != nil {
		add("status_code = ?", *patch.StatusCode)
	}
	if patch.Success != nil {
		val := 1
		if !*patch.Success {
			val = 0
		}
		add("success = ?", val)
	}
	if patch.RequestURL != nil {
		add("request_url = ?", *patch.RequestURL)
	}
	if patch.RequestMethod != nil {
		add("request_method = ?", *patch.RequestMethod)
	}
	if patch.RequestHeaders != nil {
		payload, err := json.Marshal(patch.RequestHeaders)
		if err != nil {
			payload = []byte("{}")
		}
		add("request_headers = ?", string(payload))
	}
	if patch.RequestBody != nil {
		add("request_body = ?", *patch.RequestBody)
	}
	if patch.ResponseHeaders != nil {
		payload, err := json.Marshal(patch.ResponseHeaders)
		if err != nil {
			payload = []byte("{}")
		}
		add("response_headers = ?", string(payload))
	}
	if patch.ResponseBody != nil {
		add("response_body = ?", *patch.ResponseBody)
	}

	if len(sets) == 0 {
		return 0, nil
	}

	args = append(args, id)
	query := fmt.Sprintf("UPDATE usage_records SET %s WHERE id = ?", strings.Join(sets, ", "))
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to update record: %w", err)
	}
	s.invalidateCaches()
	affected, _ := result.RowsAffected()
	return affected, nil
}


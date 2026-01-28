package usagerecord

import (
	"context"
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// CandidateHook records request routing attempts (provider + credential) into the usage record store.
// It enables the management UI to render request trace timelines similar to Aether.
type CandidateHook struct {
	cliproxyauth.NoopHook
}

func NewCandidateHook() cliproxyauth.Hook {
	return CandidateHook{}
}

func (CandidateHook) OnCandidate(ctx context.Context, candidate cliproxyauth.Candidate) {
	store := DefaultStore()
	if store == nil {
		return
	}

	requestID := strings.TrimSpace(candidate.RequestID)
	if requestID == "" {
		return
	}

	provider := strings.TrimSpace(candidate.Provider)
	authID := strings.TrimSpace(candidate.AuthID)
	authFile := strings.TrimSpace(candidate.AuthFile)
	if authFile == "" {
		authFile = authID
	}
	if authID == "" && authFile == "" {
		return
	}

	status := strings.TrimSpace(candidate.Status)
	switch status {
	case "pending", "success", "failed", "skipped":
	default:
		if candidate.Success {
			status = "success"
		} else {
			status = "failed"
		}
	}

	statusCode := candidate.StatusCode
	if statusCode == 0 && status == "success" {
		statusCode = 200
	}

	record := &RequestCandidate{
		RequestID:      requestID,
		Timestamp:      time.Now(),
		Provider:       provider,
		APIKey:         authID,
		APIKeyMasked:   authFile,
		Status:         status,
		StatusCode:     statusCode,
		Success:        candidate.Success,
		DurationMs:     candidate.DurationMs,
		ErrorMessage:   strings.TrimSpace(candidate.ErrorMessage),
		CandidateIndex: candidate.CandidateIndex,
		RetryIndex:     candidate.RetryIndex,
	}

	store.EnqueueRequestCandidate(record)
}

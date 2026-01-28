// Package usagerecord provides SQLite-based storage for API usage records.
// It tracks detailed request/response information for each API call and
// provides query capabilities for the management interface.
package usagerecord

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	log "github.com/sirupsen/logrus"
)

// ParseTimeParam converts a time parameter to RFC3339 format for database comparison.
// It accepts:
// - Unix timestamp as string (e.g., "1736582400") - converts to local RFC3339
// - ISO 8601 / RFC3339 string (e.g., "2026-01-11T12:00:00.000Z") - converts to local RFC3339
// - Empty string - returns empty string (no filter)
func ParseTimeParam(param string) string {
	if param == "" {
		return ""
	}

	// Try parsing as Unix timestamp (seconds)
	if ts, err := strconv.ParseInt(param, 10, 64); err == nil && ts > 1000000000 && ts < 9999999999 {
		// Valid Unix timestamp (between ~2001 and ~2286)
		return time.Unix(ts, 0).Format(time.RFC3339)
	}

	// Try parsing as RFC3339 (with timezone)
	if t, err := time.Parse(time.RFC3339, param); err == nil {
		return t.Local().Format(time.RFC3339)
	}

	// Try parsing as RFC3339Nano (with nanoseconds and timezone)
	if t, err := time.Parse(time.RFC3339Nano, param); err == nil {
		return t.Local().Format(time.RFC3339)
	}

	// Try parsing as ISO 8601 with milliseconds (JavaScript toISOString format)
	// Format: 2026-01-11T12:00:00.000Z
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", param); err == nil {
		return t.Local().Format(time.RFC3339)
	}

	// Try parsing as ISO 8601 without milliseconds
	if t, err := time.Parse("2006-01-02T15:04:05Z", param); err == nil {
		return t.Local().Format(time.RFC3339)
	}

	// If all parsing fails, return the original string (backward compatibility)
	return param
}

// ParseTimeParamToTime converts a time parameter to time.Time for Go operations.
// It accepts:
// - Unix timestamp as string (e.g., "1736582400") - converts to time.Time
// - ISO 8601 / RFC3339 string (e.g., "2026-01-11T12:00:00.000Z") - converts to local time.Time
// - Empty string - returns zero time
func ParseTimeParamToTime(param string) time.Time {
	if param == "" {
		return time.Time{}
	}

	// Try parsing as Unix timestamp (seconds)
	if ts, err := strconv.ParseInt(param, 10, 64); err == nil && ts > 1000000000 && ts < 9999999999 {
		return time.Unix(ts, 0)
	}

	// Try parsing as RFC3339 (with timezone)
	if t, err := time.Parse(time.RFC3339, param); err == nil {
		return t.Local()
	}

	// Try parsing as RFC3339Nano (with nanoseconds and timezone)
	if t, err := time.Parse(time.RFC3339Nano, param); err == nil {
		return t.Local()
	}

	// Try parsing as ISO 8601 with milliseconds (JavaScript toISOString format)
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", param); err == nil {
		return t.Local()
	}

	// Try parsing as ISO 8601 without milliseconds
	if t, err := time.Parse("2006-01-02T15:04:05Z", param); err == nil {
		return t.Local()
	}

	return time.Time{}
}

// Record represents a single API usage record stored in the database.
type Record struct {
	ID                     int64             `json:"id"`
	RequestID              string            `json:"request_id"`
	Timestamp              time.Time         `json:"timestamp"`
	IP                     string            `json:"ip"`
	APIKey                 string            `json:"api_key"`
	APIKeyMasked           string            `json:"api_key_masked"`
	Model                  string            `json:"model"`
	Provider               string            `json:"provider"`
	UpstreamProvider       string            `json:"upstream_provider,omitempty"`
	UpstreamAPIKeyMasked   string            `json:"upstream_api_key_masked,omitempty"`
	UpstreamCandidateCount int               `json:"upstream_candidate_count,omitempty"`
	UpstreamHasRetry       bool              `json:"upstream_has_retry,omitempty"`
	IsStreaming            bool              `json:"is_streaming"`
	InputTokens            int64             `json:"input_tokens"`
	OutputTokens           int64             `json:"output_tokens"`
	TotalTokens            int64             `json:"total_tokens"`
	CachedTokens           int64             `json:"cached_tokens"`
	ReasoningTokens        int64             `json:"reasoning_tokens"`
	DurationMs             int64             `json:"duration_ms"`
	StatusCode             int               `json:"status_code"`
	Success                bool              `json:"success"`
	RequestURL             string            `json:"request_url"`
	RequestMethod          string            `json:"request_method"`
	RequestHeaders         map[string]string `json:"request_headers,omitempty"`
	RequestBody            string            `json:"request_body,omitempty"`
	ResponseHeaders        map[string]string `json:"response_headers,omitempty"`
	ResponseBody           string            `json:"response_body,omitempty"`
}

// ListQuery defines the query parameters for listing records.
type ListQuery struct {
	Page        int    `form:"page"`
	PageSize    int    `form:"page_size"`
	APIKey      string `form:"api_key"`
	Model       string `form:"model"`
	Provider    string `form:"provider"`
	StartTime   string `form:"start_time"`
	EndTime     string `form:"end_time"`
	Success     *bool  `form:"success"`
	Search      string `form:"search"`
	SortBy      string `form:"sort_by"`
	SortOrder   string `form:"sort_order"`
	IncludeKPIs bool   `form:"include_kpis"`
}

// ListResult contains the paginated list of records.
type ListResult struct {
	Records    []Record   `json:"records"`
	Total      int64      `json:"total"`
	Page       int        `json:"page"`
	PageSize   int        `json:"page_size"`
	TotalPages int        `json:"total_pages"`
	KPIs       *UsageKPIs `json:"kpis,omitempty"`
}

// Store provides SQLite-based storage for usage records.
type Store struct {
	db     *sql.DB
	dbPath string

	closed atomic.Bool

	// Async write queue (best-effort, bounded; drops on overload).
	writeQueue     chan writeTask
	writeStop      chan struct{}
	writeDone      chan struct{}
	writeDropLogAt atomic.Int64

	// Short TTL caches to protect the DB from frequent dashboard refreshes.
	listCache             *queryCache
	modelStatsCache       *queryCache
	providerStatsCache    *queryCache
	intervalTimelineCache *queryCache
	kpisCache             *queryCache
}

var (
	defaultStore     *Store
	defaultStoreMu   sync.Mutex
	defaultStoreOnce sync.Once
)

// DefaultStore returns the global store instance.
func DefaultStore() *Store {
	return defaultStore
}

// InitDefaultStore initializes the global store with the given data directory.
func InitDefaultStore(dataDir string) error {
	defaultStoreMu.Lock()
	defer defaultStoreMu.Unlock()

	if defaultStore != nil {
		return nil
	}

	store, err := NewStore(dataDir)
	if err != nil {
		return err
	}
	defaultStore = store
	return nil
}

// CloseDefaultStore closes the global store.
func CloseDefaultStore() error {
	defaultStoreMu.Lock()
	defer defaultStoreMu.Unlock()

	if defaultStore == nil {
		return nil
	}
	err := defaultStore.Close()
	defaultStore = nil
	return err
}

// NewStore creates a new SQLite-based usage record store.
func NewStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = "."
	}

	// Ensure the directory exists
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, "usage_records.db")

	// SQLite tuning:
	// - WAL allows concurrent readers with a single writer.
	// - busy_timeout reduces "database is locked" failures under load.
	// - synchronous=NORMAL improves throughput (acceptable for analytics-like data).
	dsn := dbPath + "?" +
		"_pragma=journal_mode(WAL)&" +
		"_pragma=synchronous(NORMAL)&" +
		"_pragma=busy_timeout(5000)&" +
		"_pragma=temp_store(MEMORY)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool.
	// SQLite is single-writer, but WAL supports multiple concurrent readers.
	maxConns := runtime.GOMAXPROCS(0)
	if maxConns < 2 {
		maxConns = 2
	}
	if maxConns > 8 {
		maxConns = 8
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(time.Hour)

	store := &Store{
		db:                    db,
		dbPath:                dbPath,
		listCache:             newQueryCache(2 * time.Second),
		modelStatsCache:       newQueryCache(3 * time.Second),
		providerStatsCache:    newQueryCache(3 * time.Second),
		intervalTimelineCache: newQueryCache(3 * time.Second),
		kpisCache:             newQueryCache(2 * time.Second),
	}

	// Initialize schema
	if err := store.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	store.startWriteQueue()

	log.Infof("usage record store initialized at %s", dbPath)
	return store, nil
}

func (s *Store) isClosed() bool {
	if s == nil {
		return true
	}
	return s.closed.Load()
}

// initSchema creates the database tables if they don't exist.
func (s *Store) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS usage_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL,
		timestamp DATETIME NOT NULL,
		ip TEXT NOT NULL DEFAULT '',
		api_key TEXT NOT NULL DEFAULT '',
		api_key_masked TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		is_streaming INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		status_code INTEGER NOT NULL DEFAULT 0,
		success INTEGER NOT NULL DEFAULT 1,
		request_url TEXT NOT NULL DEFAULT '',
		request_method TEXT NOT NULL DEFAULT '',
		request_headers TEXT NOT NULL DEFAULT '{}',
		request_body TEXT NOT NULL DEFAULT '',
		response_headers TEXT NOT NULL DEFAULT '{}',
		response_body TEXT NOT NULL DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_usage_records_timestamp ON usage_records(timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_usage_records_api_key ON usage_records(api_key);
	CREATE INDEX IF NOT EXISTS idx_usage_records_model ON usage_records(model);
	CREATE INDEX IF NOT EXISTS idx_usage_records_provider ON usage_records(provider);
	CREATE INDEX IF NOT EXISTS idx_usage_records_request_id ON usage_records(request_id);
	CREATE INDEX IF NOT EXISTS idx_usage_records_success_timestamp ON usage_records(success, timestamp DESC);

	CREATE TABLE IF NOT EXISTS request_candidates (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		request_id TEXT NOT NULL,
		timestamp DATETIME NOT NULL,
		provider TEXT NOT NULL DEFAULT '',
		api_key TEXT NOT NULL DEFAULT '',
		api_key_masked TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending',
		status_code INTEGER NOT NULL DEFAULT 0,
		success INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		error_message TEXT NOT NULL DEFAULT '',
		candidate_index INTEGER NOT NULL DEFAULT 0,
		retry_index INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_request_candidates_request_id ON request_candidates(request_id);
	CREATE INDEX IF NOT EXISTS idx_request_candidates_timestamp ON request_candidates(timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_request_candidates_status ON request_candidates(status);
	CREATE INDEX IF NOT EXISTS idx_request_candidates_best ON request_candidates(request_id, success DESC, candidate_index DESC, retry_index DESC);
	CREATE INDEX IF NOT EXISTS idx_request_candidates_req_retry ON request_candidates(request_id, retry_index);
	`

	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Hotfix/Migration: Add ip column if it doesn't exist
	// Ignore errors as column might already exist
	_, _ = s.db.Exec("ALTER TABLE usage_records ADD COLUMN ip TEXT NOT NULL DEFAULT ''")
	_, _ = s.db.Exec("ALTER TABLE usage_records ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0")
	_, _ = s.db.Exec("ALTER TABLE usage_records ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0")

	return nil
}

// Insert adds a new usage record to the database.
func (s *Store) Insert(ctx context.Context, record *Record) error {
	if s.isClosed() {
		return fmt.Errorf("store is closed")
	}

	reqHeaders, err := json.Marshal(record.RequestHeaders)
	if err != nil {
		reqHeaders = []byte("{}")
	}

	respHeaders, err := json.Marshal(record.ResponseHeaders)
	if err != nil {
		respHeaders = []byte("{}")
	}

	query := `
	INSERT INTO usage_records (
		request_id, timestamp, ip, api_key, api_key_masked, model, provider,
		is_streaming, input_tokens, output_tokens, total_tokens,
		cached_tokens, reasoning_tokens,
		duration_ms, status_code, success, request_url, request_method,
		request_headers, request_body, response_headers, response_body
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	isStreaming := 0
	if record.IsStreaming {
		isStreaming = 1
	}
	success := 1
	if !record.Success {
		success = 0
	}

	result, err := s.db.ExecContext(ctx, query,
		record.RequestID,
		record.Timestamp.Format(time.RFC3339),
		record.IP,
		record.APIKey,
		record.APIKeyMasked,
		record.Model,
		record.Provider,
		isStreaming,
		record.InputTokens,
		record.OutputTokens,
		record.TotalTokens,
		record.CachedTokens,
		record.ReasoningTokens,
		record.DurationMs,
		record.StatusCode,
		success,
		record.RequestURL,
		record.RequestMethod,
		string(reqHeaders),
		record.RequestBody,
		string(respHeaders),
		record.ResponseBody,
	)
	if err != nil {
		return fmt.Errorf("failed to insert record: %w", err)
	}

	id, _ := result.LastInsertId()
	record.ID = id

	return nil
}

// List retrieves a paginated list of usage records.
func (s *Store) List(ctx context.Context, query ListQuery) (*ListResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	// Default values
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 20
	}
	if query.PageSize > 100 {
		query.PageSize = 100
	}

	cacheKey := listCacheKey(query)
	value, err := s.listCache.get(cacheKey, func() (any, error) {
		return s.listUncached(ctx, query)
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(*ListResult)
	if !ok || result == nil {
		return nil, fmt.Errorf("unexpected list cache value type")
	}
	return result, nil
}

func listCacheKey(query ListQuery) string {
	var b strings.Builder
	b.Grow(256)

	b.WriteString("page=")
	b.WriteString(strconv.Itoa(query.Page))
	b.WriteString("&page_size=")
	b.WriteString(strconv.Itoa(query.PageSize))
	b.WriteString("&api_key=")
	b.WriteString(query.APIKey)
	b.WriteString("&model=")
	b.WriteString(query.Model)
	b.WriteString("&provider=")
	b.WriteString(query.Provider)
	b.WriteString("&start_time=")
	b.WriteString(query.StartTime)
	b.WriteString("&end_time=")
	b.WriteString(query.EndTime)
	b.WriteString("&search=")
	b.WriteString(query.Search)
	b.WriteString("&sort_by=")
	b.WriteString(query.SortBy)
	b.WriteString("&sort_order=")
	b.WriteString(query.SortOrder)
	b.WriteString("&include_kpis=")
	if query.IncludeKPIs {
		b.WriteString("1")
	} else {
		b.WriteString("0")
	}
	b.WriteString("&success=")
	if query.Success == nil {
		b.WriteString("any")
	} else if *query.Success {
		b.WriteString("1")
	} else {
		b.WriteString("0")
	}
	return b.String()
}

func (s *Store) listUncached(ctx context.Context, query ListQuery) (*ListResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	// Build WHERE clause
	var conditions []string
	var args []interface{}

	if query.APIKey != "" {
		conditions = append(conditions, "api_key LIKE ?")
		args = append(args, "%"+query.APIKey+"%")
	}
	if query.Model != "" {
		conditions = append(conditions, "model LIKE ?")
		args = append(args, "%"+query.Model+"%")
	}
	if query.Provider != "" {
		conditions = append(conditions, "provider LIKE ?")
		args = append(args, "%"+query.Provider+"%")
	}
	if query.StartTime != "" {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, ParseTimeParam(query.StartTime))
	}
	if query.EndTime != "" {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, ParseTimeParam(query.EndTime))
	}
	if query.Success != nil {
		if *query.Success {
			conditions = append(conditions, "success = 1")
		} else {
			conditions = append(conditions, "success = 0")
		}
	}
	if query.Search != "" {
		conditions = append(conditions, "(model LIKE ? OR provider LIKE ? OR request_url LIKE ? OR api_key LIKE ? OR api_key_masked LIKE ? OR ip LIKE ?)")
		searchTerm := "%" + query.Search + "%"
		args = append(args, searchTerm, searchTerm, searchTerm, searchTerm, searchTerm, searchTerm)
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}
	baseArgs := make([]interface{}, len(args))
	copy(baseArgs, args)

	// Count total records
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM usage_records %s", whereClause)
	var total int64
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("failed to count records: %w", err)
	}

	// Sort order
	sortBy := "timestamp"
	if query.SortBy != "" {
		// Whitelist allowed sort columns
		switch query.SortBy {
		case "timestamp", "model", "provider", "total_tokens", "duration_ms", "status_code":
			sortBy = query.SortBy
		}
	}
	sortOrder := "DESC"
	if strings.ToUpper(query.SortOrder) == "ASC" {
		sortOrder = "ASC"
	}

	// Build main query
	offset := (query.Page - 1) * query.PageSize
	selectQuery := fmt.Sprintf(`
		SELECT id, request_id, timestamp, ip, api_key, api_key_masked, model, provider,
			COALESCE((
				SELECT provider
				FROM request_candidates rc
				WHERE rc.request_id = usage_records.request_id
				ORDER BY rc.success DESC, rc.candidate_index DESC, rc.retry_index DESC
				LIMIT 1
			), '') AS upstream_provider,
			COALESCE((
				SELECT api_key_masked
				FROM request_candidates rc
				WHERE rc.request_id = usage_records.request_id
				ORDER BY rc.success DESC, rc.candidate_index DESC, rc.retry_index DESC
				LIMIT 1
			), '') AS upstream_api_key_masked,
			(
				SELECT COUNT(*)
				FROM request_candidates rc
				WHERE rc.request_id = usage_records.request_id
			) AS upstream_candidate_count,
			(
				SELECT CASE WHEN EXISTS(
					SELECT 1
					FROM request_candidates rc
					WHERE rc.request_id = usage_records.request_id AND rc.retry_index > 0
				) THEN 1 ELSE 0 END
			) AS upstream_has_retry,
			is_streaming, input_tokens, output_tokens, total_tokens, cached_tokens, reasoning_tokens,
			duration_ms, status_code, success, request_url, request_method
		FROM usage_records %s
		ORDER BY %s %s
		LIMIT ? OFFSET ?
	`, whereClause, sortBy, sortOrder)

	args = append(args, query.PageSize, offset)
	rows, err := s.db.QueryContext(ctx, selectQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query records: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		var r Record
		var isStreaming, success int
		var timestamp string
		var upstreamHasRetry int

		err := rows.Scan(
			&r.ID, &r.RequestID, &timestamp, &r.IP, &r.APIKey, &r.APIKeyMasked,
			&r.Model, &r.Provider, &r.UpstreamProvider, &r.UpstreamAPIKeyMasked, &r.UpstreamCandidateCount, &upstreamHasRetry,
			&isStreaming, &r.InputTokens,
			&r.OutputTokens, &r.TotalTokens, &r.CachedTokens, &r.ReasoningTokens, &r.DurationMs, &r.StatusCode,
			&success, &r.RequestURL, &r.RequestMethod,
		)
		if err != nil {
			log.WithError(err).Warn("failed to scan record")
			continue
		}

		r.Timestamp, _ = time.Parse(time.RFC3339, timestamp)
		if r.Timestamp.IsZero() {
			r.Timestamp, _ = time.Parse("2006-01-02 15:04:05", timestamp)
		}
		if r.Timestamp.IsZero() {
			r.Timestamp, _ = time.Parse("2006-01-02T15:04:05Z", timestamp)
		}
		r.IsStreaming = isStreaming == 1
		r.Success = success == 1
		r.UpstreamHasRetry = upstreamHasRetry == 1
		if r.UpstreamProvider == "" {
			r.UpstreamProvider = r.Provider
		}

		records = append(records, r)
	}

	totalPages := int((total + int64(query.PageSize) - 1) / int64(query.PageSize))

	result := &ListResult{
		Records:    records,
		Total:      total,
		Page:       query.Page,
		PageSize:   query.PageSize,
		TotalPages: totalPages,
	}

	if query.IncludeKPIs {
		kpis, err := s.GetUsageKPIs(ctx, whereClause, baseArgs, query.StartTime, query.EndTime)
		if err != nil {
			log.WithError(err).Warn("failed to compute usage kpis")
		} else {
			result.KPIs = kpis
		}
	}

	return result, nil
}

// GetByID retrieves a single record by ID including full request/response details.
func (s *Store) GetByID(ctx context.Context, id int64) (*Record, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	query := `
		SELECT id, request_id, timestamp, ip, api_key, api_key_masked, model, provider,
			COALESCE((
				SELECT provider
				FROM request_candidates rc
				WHERE rc.request_id = usage_records.request_id
				ORDER BY rc.success DESC, rc.candidate_index DESC, rc.retry_index DESC
				LIMIT 1
			), '') AS upstream_provider,
			COALESCE((
				SELECT api_key_masked
				FROM request_candidates rc
				WHERE rc.request_id = usage_records.request_id
				ORDER BY rc.success DESC, rc.candidate_index DESC, rc.retry_index DESC
				LIMIT 1
			), '') AS upstream_api_key_masked,
			(
				SELECT COUNT(*)
				FROM request_candidates rc
				WHERE rc.request_id = usage_records.request_id
			) AS upstream_candidate_count,
			(
				SELECT CASE WHEN EXISTS(
					SELECT 1
					FROM request_candidates rc
					WHERE rc.request_id = usage_records.request_id AND rc.retry_index > 0
				) THEN 1 ELSE 0 END
			) AS upstream_has_retry,
			is_streaming, input_tokens, output_tokens, total_tokens, cached_tokens, reasoning_tokens,
			duration_ms, status_code, success, request_url, request_method,
			request_headers, request_body, response_headers, response_body
		FROM usage_records
		WHERE id = ?
	`

	var r Record
	var isStreaming, success int
	var timestamp string
	var reqHeadersJSON, respHeadersJSON string
	var upstreamHasRetry int

	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&r.ID, &r.RequestID, &timestamp, &r.IP, &r.APIKey, &r.APIKeyMasked,
		&r.Model, &r.Provider, &r.UpstreamProvider, &r.UpstreamAPIKeyMasked, &r.UpstreamCandidateCount, &upstreamHasRetry,
		&isStreaming, &r.InputTokens,
		&r.OutputTokens, &r.TotalTokens, &r.CachedTokens, &r.ReasoningTokens, &r.DurationMs, &r.StatusCode,
		&success, &r.RequestURL, &r.RequestMethod,
		&reqHeadersJSON, &r.RequestBody, &respHeadersJSON, &r.ResponseBody,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get record: %w", err)
	}

	r.Timestamp, _ = time.Parse(time.RFC3339, timestamp)
	if r.Timestamp.IsZero() {
		r.Timestamp, _ = time.Parse("2006-01-02 15:04:05", timestamp)
	}
	if r.Timestamp.IsZero() {
		r.Timestamp, _ = time.Parse("2006-01-02T15:04:05Z", timestamp)
	}
	r.IsStreaming = isStreaming == 1
	r.Success = success == 1
	r.UpstreamHasRetry = upstreamHasRetry == 1
	if r.UpstreamProvider == "" {
		r.UpstreamProvider = r.Provider
	}

	if err := json.Unmarshal([]byte(reqHeadersJSON), &r.RequestHeaders); err != nil {
		r.RequestHeaders = make(map[string]string)
	}
	if err := json.Unmarshal([]byte(respHeadersJSON), &r.ResponseHeaders); err != nil {
		r.ResponseHeaders = make(map[string]string)
	}

	return &r, nil
}

// DeleteOlderThan removes records older than the specified duration.
func (s *Store) DeleteOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	if s.isClosed() {
		return 0, fmt.Errorf("store is closed")
	}

	cutoff := time.Now().Add(-age).Format(time.RFC3339)
	result, err := s.db.ExecContext(ctx, "DELETE FROM usage_records WHERE timestamp < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to delete old records: %w", err)
	}

	return result.RowsAffected()
}

// Close closes the database connection.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	if s.closed.Swap(true) {
		return nil
	}

	s.stopWriteQueue()
	return s.db.Close()
}

// MaskAPIKey masks an API key for display, showing only the first and last 2 characters.
func MaskAPIKey(key string) string {
	if len(key) <= 4 {
		return strings.Repeat("*", len(key))
	}
	return key[:2] + strings.Repeat("*", len(key)-4) + key[len(key)-2:]
}

// ActivityHeatmapDay represents a single day in the activity heatmap.
type ActivityHeatmapDay struct {
	Date         string  `json:"date"`
	Requests     int64   `json:"requests"`
	TotalTokens  int64   `json:"total_tokens"`
	AvgDuration  float64 `json:"avg_duration"`  // average duration in ms
	UniqueModels int64   `json:"unique_models"` // count of unique models used
}

// ActivityHeatmap represents the activity heatmap data.
type ActivityHeatmap struct {
	StartDate   string               `json:"start_date"`
	EndDate     string               `json:"end_date"`
	TotalDays   int                  `json:"total_days"`
	MaxRequests int64                `json:"max_requests"`
	Days        []ActivityHeatmapDay `json:"days"`
}

// GetActivityHeatmap returns activity data for the heatmap (last N days).
func (s *Store) GetActivityHeatmap(ctx context.Context, days int) (*ActivityHeatmap, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	if days < 1 {
		days = 90 // Default to 90 days
	}
	if days > 365 {
		days = 365
	}

	endDate := time.Now()
	startDate := endDate.AddDate(0, 0, -days+1)

	// NOTE: timestamps may be stored with various formats/timezones depending on driver.
	// Using `substr(timestamp, 1, 10)` is more robust than `date(timestamp)`.
	query := `
		SELECT
			substr(timestamp, 1, 10) as day,
			COUNT(*) as requests,
			COALESCE(SUM(total_tokens), 0) as total_tokens,
			COALESCE(AVG(duration_ms), 0) as avg_duration,
			COUNT(DISTINCT model) as unique_models
		FROM usage_records
		WHERE substr(timestamp, 1, 10) >= ? AND substr(timestamp, 1, 10) <= ?
		GROUP BY substr(timestamp, 1, 10)
		ORDER BY day ASC
	`

	rows, err := s.db.QueryContext(ctx, query,
		startDate.Format("2006-01-02"),
		endDate.Format("2006-01-02"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query heatmap data: %w", err)
	}
	defer rows.Close()

	// Create a map of dates to data
	dataMap := make(map[string]ActivityHeatmapDay)
	var maxRequests int64 = 0

	for rows.Next() {
		var day ActivityHeatmapDay
		if err := rows.Scan(&day.Date, &day.Requests, &day.TotalTokens, &day.AvgDuration, &day.UniqueModels); err != nil {
			continue
		}
		dataMap[day.Date] = day
		if day.Requests > maxRequests {
			maxRequests = day.Requests
		}
	}

	// Fill in all days (including those with 0 requests)
	var allDays []ActivityHeatmapDay
	for d := startDate; !d.After(endDate); d = d.AddDate(0, 0, 1) {
		dateStr := d.Format("2006-01-02")
		if data, exists := dataMap[dateStr]; exists {
			allDays = append(allDays, data)
		} else {
			allDays = append(allDays, ActivityHeatmapDay{
				Date:        dateStr,
				Requests:    0,
				TotalTokens: 0,
			})
		}
	}

	return &ActivityHeatmap{
		StartDate:   startDate.Format("2006-01-02"),
		EndDate:     endDate.Format("2006-01-02"),
		TotalDays:   len(allDays),
		MaxRequests: maxRequests,
		Days:        allDays,
	}, nil
}

// ModelStats represents usage statistics for a single model.
type ModelStats struct {
	Model        string  `json:"model"`
	Provider     string  `json:"provider"`
	RequestCount int64   `json:"request_count"`
	SuccessCount int64   `json:"success_count"`
	FailureCount int64   `json:"failure_count"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	AvgDuration  float64 `json:"avg_duration_ms"`
}

// ModelStatsResult contains the list of model statistics.
type ModelStatsResult struct {
	Models      []ModelStats `json:"models"`
	TotalModels int          `json:"total_models"`
}

// GetModelStats returns usage statistics grouped by model.
func (s *Store) GetModelStats(ctx context.Context, startTime, endTime string) (*ModelStatsResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	cacheKey := startTime + "|" + endTime
	value, err := s.modelStatsCache.get(cacheKey, func() (any, error) {
		return s.getModelStatsUncached(ctx, startTime, endTime)
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(*ModelStatsResult)
	if !ok || result == nil {
		return nil, fmt.Errorf("unexpected model stats cache value type")
	}
	return result, nil
}

func (s *Store) getModelStatsUncached(ctx context.Context, startTime, endTime string) (*ModelStatsResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	var conditions []string
	var args []interface{}

	if startTime != "" {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, ParseTimeParam(startTime))
	}
	if endTime != "" {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, ParseTimeParam(endTime))
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT 
			model,
			provider,
			COUNT(*) as request_count,
			COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0) as success_count,
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0) as failure_count,
			COALESCE(SUM(input_tokens), 0) as input_tokens,
			COALESCE(SUM(output_tokens), 0) as output_tokens,
			COALESCE(SUM(total_tokens), 0) as total_tokens,
			COALESCE(AVG(duration_ms), 0) as avg_duration
		FROM usage_records
		%s
		GROUP BY model, provider
		ORDER BY request_count DESC
	`, whereClause)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query model stats: %w", err)
	}
	defer rows.Close()

	var models []ModelStats
	for rows.Next() {
		var m ModelStats
		if err := rows.Scan(
			&m.Model, &m.Provider, &m.RequestCount, &m.SuccessCount,
			&m.FailureCount, &m.InputTokens, &m.OutputTokens,
			&m.TotalTokens, &m.AvgDuration,
		); err != nil {
			continue
		}
		models = append(models, m)
	}

	return &ModelStatsResult{
		Models:      models,
		TotalModels: len(models),
	}, nil
}

// ProviderStats represents usage statistics for a single provider.
type ProviderStats struct {
	Provider     string  `json:"provider"`
	RequestCount int64   `json:"request_count"`
	SuccessCount int64   `json:"success_count"`
	FailureCount int64   `json:"failure_count"`
	TotalTokens  int64   `json:"total_tokens"`
	AvgDuration  float64 `json:"avg_duration_ms"`
	ModelCount   int64   `json:"model_count"`
}

// ProviderStatsResult contains the list of provider statistics.
type ProviderStatsResult struct {
	Providers      []ProviderStats `json:"providers"`
	TotalProviders int             `json:"total_providers"`
}

type DistinctOptionsResult struct {
	Models    []string `json:"models"`
	Providers []string `json:"providers"`
}

func (s *Store) GetDistinctOptions(ctx context.Context, startTime, endTime string) (*DistinctOptionsResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	var conditions []string
	var args []interface{}

	if startTime != "" {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, ParseTimeParam(startTime))
	}
	if endTime != "" {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, ParseTimeParam(endTime))
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	modelQuery := fmt.Sprintf(`SELECT DISTINCT model FROM usage_records %s ORDER BY model ASC`, whereClause)
	providerQuery := fmt.Sprintf(`SELECT DISTINCT provider FROM usage_records %s ORDER BY provider ASC`, whereClause)

	models, err := queryDistinctStrings(ctx, s.db, modelQuery, args...)
	if err != nil {
		return nil, err
	}
	providers, err := queryDistinctStrings(ctx, s.db, providerQuery, args...)
	if err != nil {
		return nil, err
	}

	return &DistinctOptionsResult{Models: models, Providers: providers}, nil
}

func queryDistinctStrings(ctx context.Context, db *sql.DB, query string, args ...interface{}) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query distinct options: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out, nil
}

// GetProviderStats returns usage statistics grouped by provider.
func (s *Store) GetProviderStats(ctx context.Context, startTime, endTime string) (*ProviderStatsResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	cacheKey := startTime + "|" + endTime
	value, err := s.providerStatsCache.get(cacheKey, func() (any, error) {
		return s.getProviderStatsUncached(ctx, startTime, endTime)
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(*ProviderStatsResult)
	if !ok || result == nil {
		return nil, fmt.Errorf("unexpected provider stats cache value type")
	}
	return result, nil
}

func (s *Store) getProviderStatsUncached(ctx context.Context, startTime, endTime string) (*ProviderStatsResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	var conditions []string
	var args []interface{}

	if startTime != "" {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, ParseTimeParam(startTime))
	}
	if endTime != "" {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, ParseTimeParam(endTime))
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT 
			provider,
			COUNT(*) as request_count,
			COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0) as success_count,
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0) as failure_count,
			COALESCE(SUM(total_tokens), 0) as total_tokens,
			COALESCE(AVG(duration_ms), 0) as avg_duration,
			COUNT(DISTINCT model) as model_count
		FROM usage_records
		%s
		GROUP BY provider
		ORDER BY request_count DESC
	`, whereClause)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query provider stats: %w", err)
	}
	defer rows.Close()

	var providers []ProviderStats
	for rows.Next() {
		var p ProviderStats
		if err := rows.Scan(
			&p.Provider, &p.RequestCount, &p.SuccessCount,
			&p.FailureCount, &p.TotalTokens, &p.AvgDuration, &p.ModelCount,
		); err != nil {
			continue
		}
		providers = append(providers, p)
	}

	return &ProviderStatsResult{
		Providers:      providers,
		TotalProviders: len(providers),
	}, nil
}

// UsageSummary contains overall usage summary statistics.
type UsageSummary struct {
	TotalRequests   int64   `json:"total_requests"`
	SuccessRequests int64   `json:"success_requests"`
	FailureRequests int64   `json:"failure_requests"`
	SuccessRate     float64 `json:"success_rate"`
	TotalTokens     int64   `json:"total_tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	AvgDuration     float64 `json:"avg_duration_ms"`
	UniqueModels    int64   `json:"unique_models"`
	UniqueProviders int64   `json:"unique_providers"`
}

type KPITrendPoint struct {
	T string `json:"t"`
	V int64  `json:"v"`
}

// UsageKPIs contains lightweight KPI metrics for the usage records page.
type UsageKPIs struct {
	TotalRequests   int64 `json:"total_requests"`
	SuccessRequests int64 `json:"success_requests"`
	FailureRequests int64 `json:"failure_requests"`

	TotalTokens     int64 `json:"total_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`

	RPM int64 `json:"rpm"`
	TPM int64 `json:"tpm"`

	TrendBucket   string          `json:"trend_bucket"` // hour | day
	RequestsTrend []KPITrendPoint `json:"requests_trend"`
	TokensTrend   []KPITrendPoint `json:"tokens_trend"`
	RPMTrend      []KPITrendPoint `json:"rpm_trend"`
	TPMTrend      []KPITrendPoint `json:"tpm_trend"`

	GeneratedAt string `json:"generated_at"`
}

// GetUsageSummary returns overall usage summary.
func (s *Store) GetUsageSummary(ctx context.Context, startTime, endTime string) (*UsageSummary, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	var conditions []string
	var args []interface{}

	if startTime != "" {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, ParseTimeParam(startTime))
	}
	if endTime != "" {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, ParseTimeParam(endTime))
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
		SELECT 
			COUNT(*) as total_requests,
			COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0) as success_requests,
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0) as failure_requests,
			COALESCE(SUM(input_tokens), 0) as input_tokens,
			COALESCE(SUM(output_tokens), 0) as output_tokens,
			COALESCE(SUM(total_tokens), 0) as total_tokens,
			COALESCE(AVG(duration_ms), 0) as avg_duration,
			COUNT(DISTINCT model) as unique_models,
			COUNT(DISTINCT provider) as unique_providers
		FROM usage_records
		%s
	`, whereClause)

	var summary UsageSummary
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&summary.TotalRequests, &summary.SuccessRequests, &summary.FailureRequests,
		&summary.InputTokens, &summary.OutputTokens, &summary.TotalTokens,
		&summary.AvgDuration, &summary.UniqueModels, &summary.UniqueProviders,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query usage summary: %w", err)
	}

	if summary.TotalRequests > 0 {
		summary.SuccessRate = float64(summary.SuccessRequests) / float64(summary.TotalRequests) * 100
	}

	return &summary, nil
}

func (s *Store) GetUsageKPIs(ctx context.Context, whereClause string, whereArgs []interface{}, startTime, endTime string) (*UsageKPIs, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	cacheKey := whereClause + "|" + fmt.Sprint(whereArgs) + "|" + startTime + "|" + endTime
	value, err := s.kpisCache.get(cacheKey, func() (any, error) {
		return s.getUsageKPIsUncached(ctx, whereClause, whereArgs, startTime, endTime)
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(*UsageKPIs)
	if !ok || result == nil {
		return nil, fmt.Errorf("unexpected usage kpis cache value type")
	}
	return result, nil
}

func (s *Store) getUsageKPIsUncached(ctx context.Context, whereClause string, whereArgs []interface{}, startTime, endTime string) (*UsageKPIs, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	kpis := &UsageKPIs{
		GeneratedAt: time.Now().Format(time.RFC3339),
	}

	// Totals (based on the same filters as the list endpoint)
	totalsQuery := fmt.Sprintf(`
		SELECT
			COUNT(*) as total_requests,
			COALESCE(SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END), 0) as success_requests,
			COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0) as failure_requests,
			COALESCE(SUM(input_tokens + output_tokens + cached_tokens + reasoning_tokens), 0) as total_tokens,
			COALESCE(SUM(cached_tokens), 0) as cached_tokens,
			COALESCE(SUM(reasoning_tokens), 0) as reasoning_tokens
		FROM usage_records
		%s
	`, whereClause)

	if err := s.db.QueryRowContext(ctx, totalsQuery, whereArgs...).Scan(
		&kpis.TotalRequests,
		&kpis.SuccessRequests,
		&kpis.FailureRequests,
		&kpis.TotalTokens,
		&kpis.CachedTokens,
		&kpis.ReasoningTokens,
	); err != nil {
		return nil, fmt.Errorf("failed to query usage kpis totals: %w", err)
	}

	// Determine time window for trends.
	trendStart := ParseTimeParamToTime(startTime)
	trendEnd := ParseTimeParamToTime(endTime)
	if trendEnd.IsZero() {
		trendEnd = time.Now()
	}
	if trendStart.IsZero() {
		trendStart = trendEnd.Add(-24 * time.Hour)
	}
	if trendStart.After(trendEnd) {
		trendStart, trendEnd = trendEnd, trendStart
	}

	// Choose bucket size for the compact sparkline.
	bucket := "hour"
	if trendEnd.Sub(trendStart) > 48*time.Hour {
		bucket = "day"
	}
	kpis.TrendBucket = bucket

	type aggRow struct {
		key      string
		requests int64
		tokens   int64
	}
	aggMapRequests := make(map[string]int64)
	aggMapTokens := make(map[string]int64)

	keyExpr := "substr(timestamp, 1, 13)"
	if bucket == "day" {
		keyExpr = "substr(timestamp, 1, 10)"
	}
	trendQuery := fmt.Sprintf(`
		SELECT
			%s as bucket_key,
			COUNT(*) as requests,
			COALESCE(SUM(input_tokens + output_tokens + cached_tokens + reasoning_tokens), 0) as tokens
		FROM usage_records
		%s
		GROUP BY bucket_key
		ORDER BY bucket_key ASC
	`, keyExpr, whereClause)

	rows, err := s.db.QueryContext(ctx, trendQuery, whereArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to query usage kpis trend: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r aggRow
		if err := rows.Scan(&r.key, &r.requests, &r.tokens); err != nil {
			continue
		}
		label := r.key
		if bucket == "hour" {
			label = strings.Replace(r.key, "T", " ", 1) + ":00"
		}
		aggMapRequests[label] = r.requests
		aggMapTokens[label] = r.tokens
	}

	if bucket == "day" {
		startDay := time.Date(trendStart.Year(), trendStart.Month(), trendStart.Day(), 0, 0, 0, 0, trendStart.Location())
		endDay := time.Date(trendEnd.Year(), trendEnd.Month(), trendEnd.Day(), 0, 0, 0, 0, trendEnd.Location())
		for d := startDay; !d.After(endDay); d = d.AddDate(0, 0, 1) {
			label := d.Format("2006-01-02")
			kpis.RequestsTrend = append(kpis.RequestsTrend, KPITrendPoint{T: label, V: aggMapRequests[label]})
			kpis.TokensTrend = append(kpis.TokensTrend, KPITrendPoint{T: label, V: aggMapTokens[label]})
		}
	} else {
		startHour := trendStart.Truncate(time.Hour)
		endHour := trendEnd.Truncate(time.Hour)
		for h := startHour; !h.After(endHour); h = h.Add(time.Hour) {
			label := h.Format("2006-01-02 15:00")
			kpis.RequestsTrend = append(kpis.RequestsTrend, KPITrendPoint{T: label, V: aggMapRequests[label]})
			kpis.TokensTrend = append(kpis.TokensTrend, KPITrendPoint{T: label, V: aggMapTokens[label]})
		}
	}

	// RPM/TPM: based on the last 60 seconds up to endTime (or now if endTime not provided).
	const rpmWindowSeconds = 60
	windowEnd := trendEnd
	windowStart := windowEnd.Add(-rpmWindowSeconds * time.Second)

	rpmClause := strings.TrimSpace(whereClause)
	rpmArgs := append([]interface{}{}, whereArgs...)
	if rpmClause == "" {
		rpmClause = "WHERE timestamp >= ? AND timestamp <= ?"
	} else {
		rpmClause = rpmClause + " AND timestamp >= ? AND timestamp <= ?"
	}
	rpmArgs = append(rpmArgs, windowStart.Format(time.RFC3339), windowEnd.Format(time.RFC3339))

	rpmQuery := fmt.Sprintf(`SELECT COUNT(*) FROM usage_records %s`, rpmClause)
	if err := s.db.QueryRowContext(ctx, rpmQuery, rpmArgs...).Scan(&kpis.RPM); err != nil {
		return nil, fmt.Errorf("failed to query rpm: %w", err)
	}

	tpmQuery := fmt.Sprintf(`SELECT COALESCE(SUM(input_tokens + output_tokens + cached_tokens + reasoning_tokens), 0) FROM usage_records %s`, rpmClause)
	if err := s.db.QueryRowContext(ctx, tpmQuery, rpmArgs...).Scan(&kpis.TPM); err != nil {
		return nil, fmt.Errorf("failed to query tpm: %w", err)
	}

	// RPM/TPM trend: last 60 minutes (per minute buckets).
	const trendMinutes = 60
	minEnd := windowEnd.Truncate(time.Minute)
	minStart := minEnd.Add(-time.Duration(trendMinutes-1) * time.Minute)

	minClause := strings.TrimSpace(whereClause)
	minArgs := append([]interface{}{}, whereArgs...)
	if minClause == "" {
		minClause = "WHERE timestamp >= ? AND timestamp <= ?"
	} else {
		minClause = minClause + " AND timestamp >= ? AND timestamp <= ?"
	}
	minArgs = append(minArgs, minStart.Format(time.RFC3339), minEnd.Format(time.RFC3339))

	minuteQuery := fmt.Sprintf(`
		SELECT
			substr(timestamp, 1, 16) as minute_key,
			COUNT(*) as requests,
			COALESCE(SUM(input_tokens + output_tokens + cached_tokens + reasoning_tokens), 0) as tokens
		FROM usage_records
		%s
		GROUP BY minute_key
		ORDER BY minute_key ASC
	`, minClause)

	minRows, err := s.db.QueryContext(ctx, minuteQuery, minArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to query rpm/tpm trend: %w", err)
	}
	defer minRows.Close()

	minReqMap := make(map[string]int64)
	minTokMap := make(map[string]int64)
	for minRows.Next() {
		var key string
		var req, tok int64
		if err := minRows.Scan(&key, &req, &tok); err != nil {
			continue
		}
		label := strings.Replace(key, "T", " ", 1)
		minReqMap[label] = req
		minTokMap[label] = tok
	}

	for t := minStart; !t.After(minEnd); t = t.Add(time.Minute) {
		label := t.Format("2006-01-02 15:04")
		kpis.RPMTrend = append(kpis.RPMTrend, KPITrendPoint{T: label, V: minReqMap[label]})
		kpis.TPMTrend = append(kpis.TPMTrend, KPITrendPoint{T: label, V: minTokMap[label]})
	}

	return kpis, nil
}

// RequestTimelinePoint represents a single point in the hourly request timeline.
type RequestTimelinePoint struct {
	Hour     string `json:"hour"`     // Format: "2006-01-02 15:00"
	Requests int64  `json:"requests"` // Number of requests in this hour
	Tokens   int64  `json:"tokens"`   // Total tokens in this hour
}

// RequestTimelineResult contains the hourly request distribution data.
type RequestTimelineResult struct {
	StartTime   string                 `json:"start_time"`
	EndTime     string                 `json:"end_time"`
	TotalHours  int                    `json:"total_hours"`
	MaxRequests int64                  `json:"max_requests"`
	Points      []RequestTimelinePoint `json:"points"`
}

// APIKeyStats represents aggregated statistics for a single API key.
type APIKeyStats struct {
	APIKey       string `json:"api_key"`
	UsageCount   int64  `json:"usage_count"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	LastUsedAt   string `json:"last_used_at"`
}

// GetAPIKeyStats returns aggregated statistics for all API keys from the usage records.
// This provides persistent data that survives server restarts.
func (s *Store) GetAPIKeyStats(ctx context.Context) (map[string]*APIKeyStats, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	query := `
		SELECT 
			api_key,
			COUNT(*) as usage_count,
			COALESCE(SUM(input_tokens), 0) as input_tokens,
			COALESCE(SUM(output_tokens), 0) as output_tokens,
			MAX(timestamp) as last_used_at
		FROM usage_records
		WHERE api_key != ''
		GROUP BY api_key
	`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query API key stats: %w", err)
	}
	defer rows.Close()

	result := make(map[string]*APIKeyStats)
	for rows.Next() {
		var stats APIKeyStats
		if err := rows.Scan(
			&stats.APIKey, &stats.UsageCount, &stats.InputTokens,
			&stats.OutputTokens, &stats.LastUsedAt,
		); err != nil {
			continue
		}
		result[stats.APIKey] = &stats
	}

	return result, nil
}

// GetRequestTimeline returns hourly request distribution for timeline visualization.
func (s *Store) GetRequestTimeline(ctx context.Context, startTime, endTime string) (*RequestTimelineResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	var conditions []string
	var args []interface{}

	if startTime != "" {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, ParseTimeParam(startTime))
	}
	if endTime != "" {
		conditions = append(conditions, "timestamp <= ?")
		args = append(args, ParseTimeParam(endTime))
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Group by hour (extract YYYY-MM-DD HH from timestamp)
	query := fmt.Sprintf(`
		SELECT 
			substr(timestamp, 1, 13) as hour,
			COUNT(*) as requests,
			COALESCE(SUM(total_tokens), 0) as tokens
		FROM usage_records
		%s
		GROUP BY hour
		ORDER BY hour ASC
	`, whereClause)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query request timeline: %w", err)
	}
	defer rows.Close()

	dataMap := make(map[string]RequestTimelinePoint)
	var maxRequests int64

	for rows.Next() {
		var hour string
		var requests, tokens int64
		if err := rows.Scan(&hour, &requests, &tokens); err != nil {
			continue
		}
		// Convert "2006-01-02T15" to "2006-01-02 15:00" for display
		displayHour := strings.Replace(hour, "T", " ", 1) + ":00"
		dataMap[displayHour] = RequestTimelinePoint{
			Hour:     displayHour,
			Requests: requests,
			Tokens:   tokens,
		}
		if requests > maxRequests {
			maxRequests = requests
		}
	}

	// Build complete hourly timeline
	var startDate, endDate time.Time
	now := time.Now()

	if startTime != "" {
		startDate = ParseTimeParamToTime(startTime)
		if startDate.IsZero() {
			startDate = now.Add(-24 * time.Hour)
		}
	} else {
		// Default to last 24 hours
		startDate = now.Add(-24 * time.Hour)
	}
	if endTime != "" {
		endDate = ParseTimeParamToTime(endTime)
		if endDate.IsZero() {
			endDate = now
		}
	} else {
		endDate = now
	}

	// Truncate to hour
	startDate = startDate.Truncate(time.Hour)
	endDate = endDate.Truncate(time.Hour)

	// Fill in all hours
	var points []RequestTimelinePoint
	for h := startDate; !h.After(endDate); h = h.Add(time.Hour) {
		hourStr := h.Format("2006-01-02 15:00")
		if data, exists := dataMap[hourStr]; exists {
			points = append(points, data)
		} else {
			points = append(points, RequestTimelinePoint{
				Hour:     hourStr,
				Requests: 0,
				Tokens:   0,
			})
		}
	}

	return &RequestTimelineResult{
		StartTime:   startDate.Format(time.RFC3339),
		EndTime:     endDate.Format(time.RFC3339),
		TotalHours:  len(points),
		MaxRequests: maxRequests,
		Points:      points,
	}, nil
}

// IntervalTimelinePoint represents a single point in the interval timeline scatter chart.
type IntervalTimelinePoint struct {
	X     string  `json:"x"`     // ISO timestamp
	Y     float64 `json:"y"`     // Interval in minutes
	Model string  `json:"model"` // Model name for color coding
}

// IntervalTimelineResult contains the interval timeline data for scatter chart visualization.
type IntervalTimelineResult struct {
	AnalysisPeriodHours int                     `json:"analysis_period_hours"`
	TotalPoints         int                     `json:"total_points"`
	Points              []IntervalTimelinePoint `json:"points"`
	Models              []string                `json:"models,omitempty"` // List of unique models
}

// GetIntervalTimeline returns request interval data for scatter chart visualization.
// It calculates the time interval between consecutive requests.
func (s *Store) GetIntervalTimeline(ctx context.Context, hours int, limit int) (*IntervalTimelineResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	if hours < 1 {
		hours = 24
	}
	if hours > 720 {
		hours = 720
	}
	if limit < 1 {
		limit = 5000
	}
	if limit > 10000 {
		limit = 10000
	}

	cacheKey := strconv.Itoa(hours) + "|" + strconv.Itoa(limit)
	value, err := s.intervalTimelineCache.get(cacheKey, func() (any, error) {
		return s.getIntervalTimelineUncached(ctx, hours, limit)
	})
	if err != nil {
		return nil, err
	}
	result, ok := value.(*IntervalTimelineResult)
	if !ok || result == nil {
		return nil, fmt.Errorf("unexpected interval timeline cache value type")
	}
	return result, nil
}

func (s *Store) getIntervalTimelineUncached(ctx context.Context, hours int, limit int) (*IntervalTimelineResult, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	const oversampleFactor = 5
	maxRecords := limit*oversampleFactor + 1
	if maxRecords < 2 {
		maxRecords = 2
	}
	if maxRecords > 200000 {
		maxRecords = 200000
	}

	now := time.Now()
	startTime := now.Add(-time.Duration(hours) * time.Hour)

	// Bound the scan: fetch only the most recent records in the window.
	// We will sample down to `limit` points after interval filtering.
	query := `
		SELECT timestamp, model
		FROM usage_records
		WHERE timestamp >= ? AND success = 1
		ORDER BY timestamp DESC
		LIMIT ?
	`

	rows, err := s.db.QueryContext(ctx, query, startTime.Format(time.RFC3339), maxRecords)
	if err != nil {
		return nil, fmt.Errorf("failed to query interval timeline: %w", err)
	}
	defer rows.Close()

	type recordData struct {
		timestamp time.Time
		model     string
	}

	records := make([]recordData, 0, maxRecords)
	for rows.Next() {
		var timestampStr, model string
		if err := rows.Scan(&timestampStr, &model); err != nil {
			continue
		}

		ts, _ := time.Parse(time.RFC3339, timestampStr)
		if ts.IsZero() {
			ts, _ = time.Parse("2006-01-02 15:04:05", timestampStr)
		}
		if ts.IsZero() {
			ts, _ = time.Parse("2006-01-02T15:04:05Z", timestampStr)
		}
		if ts.IsZero() {
			continue
		}
		records = append(records, recordData{timestamp: ts, model: model})
	}

	// Reverse to chronological order.
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}

	// Calculate intervals between consecutive requests.
	points := make([]IntervalTimelinePoint, 0, minInt(limit, len(records)))
	modelsSet := make(map[string]bool)

	for i := 1; i < len(records); i++ {
		interval := records[i].timestamp.Sub(records[i-1].timestamp).Minutes()
		if interval > 120 {
			continue
		}

		points = append(points, IntervalTimelinePoint{
			X:     records[i].timestamp.Format(time.RFC3339),
			Y:     float64(int(interval*100)) / 100, // Round to 2 decimals
			Model: records[i].model,
		})
		if records[i].model != "" {
			modelsSet[records[i].model] = true
		}
	}

	// Apply limit if needed (sample evenly).
	if len(points) > limit {
		step := float64(len(points)) / float64(limit)
		sampled := make([]IntervalTimelinePoint, 0, limit)
		for i := 0; i < limit; i++ {
			idx := int(float64(i) * step)
			if idx < len(points) {
				sampled = append(sampled, points[idx])
			}
		}
		points = sampled
	}

	models := make([]string, 0, len(modelsSet))
	for model := range modelsSet {
		models = append(models, model)
	}

	return &IntervalTimelineResult{
		AnalysisPeriodHours: hours,
		TotalPoints:         len(points),
		Points:              points,
		Models:              models,
	}, nil
}

// RequestCandidate represents a request candidate record for tracing request routing.
type RequestCandidate struct {
	ID             int64     `json:"id"`
	RequestID      string    `json:"request_id"`
	Timestamp      time.Time `json:"timestamp"`
	Provider       string    `json:"provider"`
	APIKey         string    `json:"api_key"`
	APIKeyMasked   string    `json:"api_key_masked"`
	Status         string    `json:"status"` // pending, success, failed, skipped
	StatusCode     int       `json:"status_code"`
	Success        bool      `json:"success"`
	DurationMs     int64     `json:"duration_ms"`
	ErrorMessage   string    `json:"error_message,omitempty"`
	CandidateIndex int       `json:"candidate_index"`
	RetryIndex     int       `json:"retry_index"`
}

// GetRequestCandidates retrieves all candidate records for a specific request ID.
func (s *Store) GetRequestCandidates(ctx context.Context, requestID string) ([]RequestCandidate, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("store is closed")
	}

	query := `
		SELECT id, request_id, timestamp, provider, api_key, api_key_masked,
			status, status_code, success, duration_ms, error_message,
			candidate_index, retry_index
		FROM request_candidates
		WHERE request_id = ?
		ORDER BY candidate_index ASC, retry_index ASC
	`

	rows, err := s.db.QueryContext(ctx, query, requestID)
	if err != nil {
		return nil, fmt.Errorf("failed to query request candidates: %w", err)
	}
	defer rows.Close()

	var candidates []RequestCandidate
	for rows.Next() {
		var c RequestCandidate
		var timestamp string
		var success int

		err := rows.Scan(
			&c.ID, &c.RequestID, &timestamp, &c.Provider, &c.APIKey, &c.APIKeyMasked,
			&c.Status, &c.StatusCode, &success, &c.DurationMs, &c.ErrorMessage,
			&c.CandidateIndex, &c.RetryIndex,
		)
		if err != nil {
			log.WithError(err).Warn("failed to scan request candidate")
			continue
		}

		c.Timestamp, _ = time.Parse(time.RFC3339, timestamp)
		if c.Timestamp.IsZero() {
			c.Timestamp, _ = time.Parse("2006-01-02 15:04:05", timestamp)
		}
		if c.Timestamp.IsZero() {
			c.Timestamp, _ = time.Parse("2006-01-02T15:04:05Z", timestamp)
		}
		c.Success = success == 1

		candidates = append(candidates, c)
	}

	return candidates, nil
}

// InsertRequestCandidate adds a new request candidate record.
func (s *Store) InsertRequestCandidate(ctx context.Context, candidate *RequestCandidate) error {
	if s.isClosed() {
		return fmt.Errorf("store is closed")
	}

	query := `
	INSERT INTO request_candidates (
		request_id, timestamp, provider, api_key, api_key_masked,
		status, status_code, success, duration_ms, error_message,
		candidate_index, retry_index
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	success := 0
	if candidate.Success {
		success = 1
	}

	result, err := s.db.ExecContext(ctx, query,
		candidate.RequestID,
		candidate.Timestamp.Format(time.RFC3339),
		candidate.Provider,
		candidate.APIKey,
		candidate.APIKeyMasked,
		candidate.Status,
		candidate.StatusCode,
		success,
		candidate.DurationMs,
		candidate.ErrorMessage,
		candidate.CandidateIndex,
		candidate.RetryIndex,
	)
	if err != nil {
		return fmt.Errorf("failed to insert request candidate: %w", err)
	}

	id, _ := result.LastInsertId()
	candidate.ID = id

	return nil
}

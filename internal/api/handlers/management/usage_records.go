package management

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usagerecord"
)

// GetUsageRecords returns a paginated list of usage records.
func (h *Handler) GetUsageRecords(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	var query usagerecord.ListQuery
	query.Page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	query.PageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	query.APIKey = c.Query("api_key")
	query.Model = c.Query("model")
	query.Provider = c.Query("provider")
	query.StartTime = c.Query("start_time")
	query.EndTime = c.Query("end_time")
	query.Search = c.Query("search")
	query.SortBy = c.Query("sort_by")
	query.SortOrder = c.Query("sort_order")

	if includeKPIsStr := c.Query("include_kpis"); includeKPIsStr != "" {
		query.IncludeKPIs = includeKPIsStr == "true" || includeKPIsStr == "1"
	}

	if successStr := c.Query("success"); successStr != "" {
		success := successStr == "true" || successStr == "1"
		query.Success = &success
	}

	result, err := store.List(c.Request.Context(), query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetRequestTimeline returns hourly request distribution for timeline visualization.
func (h *Handler) GetRequestTimeline(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	startTime := c.Query("start_time")
	endTime := c.Query("end_time")

	result, err := store.GetRequestTimeline(c.Request.Context(), startTime, endTime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetUsageRecordByID returns a single usage record with full details.
func (h *Handler) GetUsageRecordByID(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	record, err := store.GetByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "record not found"})
		return
	}

	c.JSON(http.StatusOK, record)
}

// DeleteOldUsageRecords removes records older than specified days.
func (h *Handler) DeleteOldUsageRecords(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	daysStr := c.DefaultQuery("days", "30")
	days, err := strconv.Atoi(daysStr)
	if err != nil || days < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid days parameter"})
		return
	}

	var body struct {
		Days *int `json:"days"`
	}
	if err := c.ShouldBindJSON(&body); err == nil && body.Days != nil {
		days = *body.Days
	}

	if days < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "days must be at least 1"})
		return
	}

	age := time.Duration(days) * 24 * time.Hour
	count, err := store.DeleteOlderThan(c.Request.Context(), age)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"deleted": count,
		"message": fmt.Sprintf("deleted %d records older than %d days", count, days),
	})
}

// GetActivityHeatmap returns activity data for a GitHub-style heatmap visualization.
func (h *Handler) GetActivityHeatmap(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	daysStr := c.DefaultQuery("days", "90")
	days, err := strconv.Atoi(daysStr)
	if err != nil || days < 1 {
		days = 90
	}

	result, err := store.GetActivityHeatmap(c.Request.Context(), days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetUsageRecordOptions returns distinct model/provider options for filters.
func (h *Handler) GetUsageRecordOptions(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	startTime := c.Query("start_time")
	endTime := c.Query("end_time")

	result, err := store.GetDistinctOptions(c.Request.Context(), startTime, endTime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetModelStats returns usage statistics grouped by model.
func (h *Handler) GetModelStats(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	startTime := c.Query("start_time")
	endTime := c.Query("end_time")

	result, err := store.GetModelStats(c.Request.Context(), startTime, endTime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetProviderStats returns usage statistics grouped by provider.
func (h *Handler) GetProviderStats(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	startTime := c.Query("start_time")
	endTime := c.Query("end_time")

	result, err := store.GetProviderStats(c.Request.Context(), startTime, endTime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetUsageSummary returns overall usage summary statistics.
func (h *Handler) GetUsageSummary(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	startTime := c.Query("start_time")
	endTime := c.Query("end_time")

	result, err := store.GetUsageSummary(c.Request.Context(), startTime, endTime)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}

// GetIntervalTimeline returns request interval data for scatter chart visualization.
func (h *Handler) GetIntervalTimeline(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	hoursStr := c.DefaultQuery("hours", "24")
	hours, err := strconv.Atoi(hoursStr)
	if err != nil || hours < 1 {
		hours = 24
	}

	limitStr := c.DefaultQuery("limit", "5000")
	limit, err := strconv.Atoi(limitStr)
	if err != nil || limit < 1 {
		limit = 5000
	}

	result, err := store.GetIntervalTimeline(c.Request.Context(), hours, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}
// GetRequestCandidates returns all candidate records for a specific request ID.
func (h *Handler) GetRequestCandidates(c *gin.Context) {
	store := usagerecord.DefaultStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage records not available"})
		return
	}

	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	// First get the usage record to find the request_id
	record, err := store.GetByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "record not found"})
		return
	}

	// Then get candidates by request_id
	candidates, err := store.GetRequestCandidates(c.Request.Context(), record.RequestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"request_id": record.RequestID,
		"candidates": candidates,
	})
}

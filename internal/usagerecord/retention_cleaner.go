package usagerecord

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultRetentionInterval   = 1 * time.Hour
	defaultRetentionBatchSize  = 1000
	defaultRetentionPause      = 200 * time.Millisecond
	defaultRetentionMaxRuntime = 15 * time.Second
)

// RetentionCleaner periodically deletes old usage records to limit database growth.
// It performs deletions in small batches to minimize write-lock time and reduce performance impact.
type RetentionCleaner struct {
	store *Store

	retentionDays atomic.Int64
	started       atomic.Bool

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	done      chan struct{}

	interval   time.Duration
	batchSize  int
	pause      time.Duration
	maxRuntime time.Duration
}

func NewRetentionCleaner(store *Store, retentionDays int) *RetentionCleaner {
	cleaner := &RetentionCleaner{
		store:      store,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		interval:   defaultRetentionInterval,
		batchSize:  defaultRetentionBatchSize,
		pause:      defaultRetentionPause,
		maxRuntime: defaultRetentionMaxRuntime,
	}
	cleaner.retentionDays.Store(int64(retentionDays))
	return cleaner
}

func (c *RetentionCleaner) Start() {
	if c == nil {
		return
	}
	c.startOnce.Do(func() {
		c.started.Store(true)
		go c.loop()
	})
}

func (c *RetentionCleaner) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		close(c.stop)
	})
	if !c.started.Load() {
		close(c.done)
		return
	}
	<-c.done
}

func (c *RetentionCleaner) RetentionDays() int {
	if c == nil {
		return 0
	}
	return int(c.retentionDays.Load())
}

func (c *RetentionCleaner) UpdateRetentionDays(days int) (previous int) {
	if c == nil {
		return 0
	}
	return int(c.retentionDays.Swap(int64(days)))
}

func (c *RetentionCleaner) loop() {
	defer close(c.done)

	// Initial delay avoids startup spikes. Keep deterministic and bounded.
	initialDelay := 1*time.Minute + time.Duration(time.Now().UnixNano()%int64(2*time.Minute))
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-timer.C:
			c.runOnce()
			timer.Reset(c.interval)
		}
	}
}

func (c *RetentionCleaner) runOnce() {
	if c == nil || c.store == nil {
		return
	}

	days := int(c.retentionDays.Load())
	if days <= 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.maxRuntime)
	defer cancel()

	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	start := time.Now()
	var totalDeleted int64

	for {
		if ctx.Err() != nil {
			break
		}
		if time.Since(start) >= c.maxRuntime {
			break
		}

		deleted, err := c.store.deleteOlderThanCutoffBatch(ctx, cutoff, c.batchSize)
		if err != nil {
			if ctx.Err() != nil {
				log.WithError(err).Debug("usage record retention cleanup timed out")
				break
			}
			log.WithError(err).Warn("usage record retention cleanup failed")
			break
		}
		if deleted == 0 {
			break
		}
		totalDeleted += deleted

		if c.pause > 0 {
			select {
			case <-c.stop:
				return
			case <-ctx.Done():
				break
			case <-time.After(c.pause):
			}
		}
	}

	if totalDeleted > 0 {
		log.Infof("usage record retention cleanup: deleted %d records older than %d days", totalDeleted, days)
	}
}

func (s *Store) deleteOlderThanCutoffBatch(ctx context.Context, cutoff string, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = defaultRetentionBatchSize
	}

	if s.isClosed() {
		return 0, fmt.Errorf("store is closed")
	}

	query := `
		DELETE FROM usage_records
		WHERE id IN (
			SELECT id
			FROM usage_records
			WHERE timestamp < ?
			ORDER BY timestamp ASC
			LIMIT ?
		)
	`

	result, err := s.db.ExecContext(ctx, query, cutoff, batchSize)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

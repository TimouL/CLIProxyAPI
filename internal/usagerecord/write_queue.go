package usagerecord

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultWriteQueueSize = 2048
	defaultWriteTimeout   = 5 * time.Second
	writeDropLogInterval  = 10 * time.Second
)

type writeTaskKind uint8

const (
	writeTaskInsertUsageRecord writeTaskKind = iota
	writeTaskInsertRequestCandidate
)

type writeTask struct {
	kind             writeTaskKind
	usageRecord      *Record
	requestCandidate *RequestCandidate
}

func (s *Store) startWriteQueue() {
	if s == nil {
		return
	}
	if s.writeQueue != nil {
		return
	}

	s.writeQueue = make(chan writeTask, defaultWriteQueueSize)
	s.writeStop = make(chan struct{})
	s.writeDone = make(chan struct{})

	go s.writeLoop()
}

func (s *Store) stopWriteQueue() {
	if s == nil || s.writeStop == nil {
		return
	}

	select {
	case <-s.writeStop:
		// already closed
	default:
		close(s.writeStop)
	}

	if s.writeDone == nil {
		return
	}
	select {
	case <-s.writeDone:
	case <-time.After(2 * time.Second):
	}
}

func (s *Store) writeLoop() {
	defer close(s.writeDone)

	for {
		select {
		case <-s.writeStop:
			return
		case task := <-s.writeQueue:
			if s.isClosed() {
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), defaultWriteTimeout)
			var err error
			switch task.kind {
			case writeTaskInsertUsageRecord:
				err = s.Insert(ctx, task.usageRecord)
			case writeTaskInsertRequestCandidate:
				err = s.InsertRequestCandidate(ctx, task.requestCandidate)
			default:
				err = nil
			}
			cancel()

			if err != nil {
				switch task.kind {
				case writeTaskInsertUsageRecord:
					log.WithError(err).Warn("failed to insert usage record")
				case writeTaskInsertRequestCandidate:
					log.WithError(err).Warn("failed to insert request candidate")
				default:
					log.WithError(err).Warn("failed to process write task")
				}
			}
		}
	}
}

func (s *Store) EnqueueUsageRecord(record *Record) bool {
	if s == nil || record == nil || s.isClosed() || s.writeQueue == nil {
		return false
	}

	select {
	case s.writeQueue <- writeTask{kind: writeTaskInsertUsageRecord, usageRecord: record}:
		return true
	default:
		s.logWriteDrop("usage record")
		return false
	}
}

func (s *Store) EnqueueRequestCandidate(candidate *RequestCandidate) bool {
	if s == nil || candidate == nil || s.isClosed() || s.writeQueue == nil {
		return false
	}

	select {
	case s.writeQueue <- writeTask{kind: writeTaskInsertRequestCandidate, requestCandidate: candidate}:
		return true
	default:
		s.logWriteDrop("request candidate")
		return false
	}
}

func (s *Store) logWriteDrop(kind string) {
	if s == nil {
		return
	}

	now := time.Now().UnixNano()
	last := s.writeDropLogAt.Load()
	if last > 0 && time.Duration(now-last) < writeDropLogInterval {
		return
	}
	// Best effort: avoid log spam, correctness isn't critical.
	s.writeDropLogAt.Store(now)

	queueLen := 0
	queueCap := 0
	if s.writeQueue != nil {
		queueLen = len(s.writeQueue)
		queueCap = cap(s.writeQueue)
	}
	log.WithFields(log.Fields{
		"kind":      kind,
		"queue_len": queueLen,
		"queue_cap": queueCap,
	}).Warn("usage record write queue is full; dropping write task")
}

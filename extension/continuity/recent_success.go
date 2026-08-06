package continuity

import (
	"container/list"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/extension"
	"github.com/QuantumNous/new-api/model"

	"github.com/bytedance/gopkg/util/gopool"
)

type continuityRecentRelayOutcome struct {
	LatestSuccessAt        int64
	LatestSuccessLatencyMs int64
	LatestFailureAt        int64
}

type continuityRecentRelayOutcomeEntry struct {
	evidence continuityRecentRelayOutcome
	order    *list.Element
}

type continuityRecentRelaySuccessEntry = continuityRecentRelayOutcomeEntry

const (
	continuityRecentRelaySuccessMaxEntries      = 4096
	continuityRecentRelaySuccessMaxModelBytes   = 255
	continuityRecentRelayOutcomeRetention       = 2 * time.Hour
	continuityRecentRelayOutcomeCleanupInterval = 5 * time.Minute
	continuityRelayOutcomePersistenceRetention  = 48 * time.Hour
)

var continuityRecentRelaySuccessCapacity = continuityRecentRelaySuccessMaxEntries

var continuityRecentRelaySuccesses = struct {
	sync.RWMutex
	byPair        map[string]*continuityRecentRelayOutcomeEntry
	order         *list.List
	lastCleanupAt int64
}{
	byPair: make(map[string]*continuityRecentRelayOutcomeEntry),
	order:  list.New(),
}

var continuityRelayOutcomeLastDBCleanup atomic.Int64

func (plugin) ObserveRelayOutcome(event extension.RelayOutcomeEvent) {
	if !recordContinuityRecentRelayOutcome(event) {
		return
	}
	gopool.Go(func() {
		if err := persistContinuityRelayOutcome(event); err != nil {
			common.SysError("failed to persist Continuity relay outcome: " + err.Error())
		}
	})
}

// Kept as a focused test/helper seam for callers that only have successful
// evidence. Production relay observation uses ObserveRelayOutcome above.
func recordContinuityRecentRelaySuccess(event extension.RelaySuccessEvent) {
	recordContinuityRecentRelayOutcome(extension.RelayOutcomeEvent{
		Group:          event.Group,
		Model:          event.Model,
		ObservedAt:     event.ObservedAt,
		LatencyMs:      event.LatencyMs,
		Success:        true,
		StatusRelevant: true,
	})
}

func recordContinuityRecentRelayOutcome(event extension.RelayOutcomeEvent) bool {
	groupKey := event.Group
	modelID := event.Model
	if !validContinuityRecentRelayOutcomeKey(groupKey, continuityManagedGroupMaxLength) ||
		!validContinuityRecentRelayOutcomeKey(modelID, continuityRecentRelaySuccessMaxModelBytes) {
		return false
	}
	// Local validation, policy, and billing rejections are persisted so every
	// completed call is accounted for, but they carry no upstream health signal
	// and therefore must not consume space in the bounded live-status cache.
	if !event.Success && !event.StatusRelevant {
		return true
	}
	observedAt := event.ObservedAt.UTC().Truncate(time.Second)
	if observedAt.IsZero() {
		observedAt = time.Now().UTC().Truncate(time.Second)
	}
	latencyMs := continuityStatusLatencyMs(event.LatencyMs)
	pairKey := continuityGroupModelProbePairKey(groupKey, modelID)

	continuityRecentRelaySuccesses.Lock()
	wallNow := time.Now().UTC().Unix()
	if wallNow-continuityRecentRelaySuccesses.lastCleanupAt >= int64(continuityRecentRelayOutcomeCleanupInterval/time.Second) {
		cutoff := wallNow - int64(continuityRecentRelayOutcomeRetention/time.Second)
		for key, entry := range continuityRecentRelaySuccesses.byPair {
			latestAt := entry.evidence.LatestSuccessAt
			if entry.evidence.LatestFailureAt > latestAt {
				latestAt = entry.evidence.LatestFailureAt
			}
			if latestAt >= cutoff {
				continue
			}
			continuityRecentRelaySuccesses.order.Remove(entry.order)
			delete(continuityRecentRelaySuccesses.byPair, key)
		}
		continuityRecentRelaySuccesses.lastCleanupAt = wallNow
	}
	entry, exists := continuityRecentRelaySuccesses.byPair[pairKey]
	if !exists {
		capacity := continuityRecentRelaySuccessCapacity
		if capacity < 1 {
			capacity = 1
		}
		if len(continuityRecentRelaySuccesses.byPair) >= capacity {
			oldest := continuityRecentRelaySuccesses.order.Front()
			if oldest != nil {
				delete(continuityRecentRelaySuccesses.byPair, oldest.Value.(string))
				continuityRecentRelaySuccesses.order.Remove(oldest)
			}
		}
		order := continuityRecentRelaySuccesses.order.PushBack(pairKey)
		entry = &continuityRecentRelayOutcomeEntry{order: order}
		continuityRecentRelaySuccesses.byPair[pairKey] = entry
	}
	if event.Success {
		if observedAt.Unix() >= entry.evidence.LatestSuccessAt {
			entry.evidence.LatestSuccessAt = observedAt.Unix()
			entry.evidence.LatestSuccessLatencyMs = latencyMs
		}
	} else if event.StatusRelevant && observedAt.Unix() >= entry.evidence.LatestFailureAt {
		entry.evidence.LatestFailureAt = observedAt.Unix()
	}
	continuityRecentRelaySuccesses.order.MoveToBack(entry.order)
	continuityRecentRelaySuccesses.Unlock()
	return true
}

func persistContinuityRelayOutcome(event extension.RelayOutcomeEvent) error {
	observedAt := event.ObservedAt.UTC().Truncate(time.Second)
	if observedAt.IsZero() {
		observedAt = time.Now().UTC().Truncate(time.Second)
	}
	bucketTs := observedAt.Unix() - observedAt.Unix()%model.ContinuityRelayOutcomeBucketSeconds
	bucket := &model.ContinuityRelayOutcomeBucket{
		GroupKey:     event.Group,
		ModelID:      event.Model,
		BucketTs:     bucketTs,
		RequestCount: 1,
	}
	if event.Success {
		bucket.SuccessCount = 1
		bucket.SuccessLatencySumMs = continuityStatusLatencyMs(event.LatencyMs)
		bucket.LatestSuccessAt = observedAt.Unix()
		bucket.LatestSuccessLatencyMs = continuityStatusLatencyMs(event.LatencyMs)
	} else if event.StatusRelevant {
		bucket.FailureCount = 1
		bucket.LatestFailureAt = observedAt.Unix()
	} else {
		bucket.IgnoredFailureCount = 1
	}
	if err := model.UpsertContinuityRelayOutcomeBucket(bucket); err != nil {
		return err
	}
	now := time.Now().UTC().Unix()
	cleanupInterval := int64(continuityRecentRelayOutcomeCleanupInterval / time.Second)
	lastCleanup := continuityRelayOutcomeLastDBCleanup.Load()
	if now-lastCleanup >= cleanupInterval &&
		continuityRelayOutcomeLastDBCleanup.CompareAndSwap(lastCleanup, now) {
		cutoff := now - int64(continuityRelayOutcomePersistenceRetention/time.Second)
		if err := model.DeleteContinuityRelayOutcomeBucketsBefore(cutoff); err != nil {
			common.SysError("failed to clean old Continuity relay outcomes: " + err.Error())
		}
	}
	return nil
}

func validContinuityRecentRelayOutcomeKey(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func latestContinuityRecentRelaySuccess(
	groupKey string,
	modelID string,
	now time.Time,
	maxAge time.Duration,
) (continuityRecentRelaySuccess, bool) {
	if maxAge <= 0 {
		return continuityRecentRelaySuccess{}, false
	}
	pairKey := continuityGroupModelProbePairKey(groupKey, modelID)
	continuityRecentRelaySuccesses.RLock()
	entry, exists := continuityRecentRelaySuccesses.byPair[pairKey]
	evidence := continuityRecentRelayOutcome{}
	if exists {
		evidence = entry.evidence
	}
	continuityRecentRelaySuccesses.RUnlock()
	cutoff := now.UTC().Add(-maxAge).Unix()
	if !exists || evidence.LatestSuccessAt < cutoff || evidence.LatestSuccessAt > now.UTC().Unix() {
		bucketStart := cutoff - cutoff%model.ContinuityRelayOutcomeBucketSeconds
		rows, err := model.GetContinuityRelayOutcomeBucketsForPair(
			groupKey,
			modelID,
			bucketStart,
			now.UTC().Unix(),
		)
		if err != nil {
			return continuityRecentRelaySuccess{}, false
		}
		for _, row := range rows {
			if row.LatestSuccessAt >= evidence.LatestSuccessAt {
				evidence.LatestSuccessAt = row.LatestSuccessAt
				evidence.LatestSuccessLatencyMs = row.LatestSuccessLatencyMs
			}
		}
	}
	if evidence.LatestSuccessAt < cutoff || evidence.LatestSuccessAt > now.UTC().Unix() {
		return continuityRecentRelaySuccess{}, false
	}
	return continuityRecentRelaySuccess{
		ObservedAt: evidence.LatestSuccessAt,
		LatencyMs:  evidence.LatestSuccessLatencyMs,
	}, true
}

func snapshotContinuityRecentRelayOutcomes() map[string]continuityRecentRelayOutcome {
	continuityRecentRelaySuccesses.RLock()
	defer continuityRecentRelaySuccesses.RUnlock()
	snapshot := make(map[string]continuityRecentRelayOutcome, len(continuityRecentRelaySuccesses.byPair))
	for pairKey, entry := range continuityRecentRelaySuccesses.byPair {
		snapshot[pairKey] = entry.evidence
	}
	return snapshot
}

// continuityRecentRelaySuccess remains the compact evidence shape consumed by
// the existing active-probe coverage logic.
type continuityRecentRelaySuccess struct {
	ObservedAt int64
	LatencyMs  int64
}

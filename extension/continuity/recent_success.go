package continuity

import (
	"container/list"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/extension"
)

type continuityRecentRelaySuccess struct {
	ObservedAt int64
	LatencyMs  int64
}

type continuityRecentRelaySuccessEntry struct {
	evidence continuityRecentRelaySuccess
	order    *list.Element
}

const (
	continuityRecentRelaySuccessMaxEntries      = 4096
	continuityRecentRelaySuccessMaxModelBytes   = 255
	continuityRecentRelaySuccessRetention       = 2 * time.Hour
	continuityRecentRelaySuccessCleanupInterval = 5 * time.Minute
)

var continuityRecentRelaySuccessCapacity = continuityRecentRelaySuccessMaxEntries

var continuityRecentRelaySuccesses = struct {
	sync.RWMutex
	byPair        map[string]*continuityRecentRelaySuccessEntry
	order         *list.List
	lastCleanupAt int64
}{
	byPair: make(map[string]*continuityRecentRelaySuccessEntry),
	order:  list.New(),
}

func (plugin) ObserveRelaySuccess(event extension.RelaySuccessEvent) {
	recordContinuityRecentRelaySuccess(event)
}

func recordContinuityRecentRelaySuccess(event extension.RelaySuccessEvent) {
	groupKey := event.Group
	modelID := event.Model
	if !validContinuityRecentRelaySuccessKey(groupKey, continuityManagedGroupMaxLength) ||
		!validContinuityRecentRelaySuccessKey(modelID, continuityRecentRelaySuccessMaxModelBytes) {
		return
	}
	observedAt := event.ObservedAt.UTC().Truncate(time.Second)
	if observedAt.IsZero() {
		observedAt = time.Now().UTC().Truncate(time.Second)
	}
	latencyMs := continuityStatusLatencyMs(event.LatencyMs)
	evidence := continuityRecentRelaySuccess{
		ObservedAt: observedAt.Unix(),
		LatencyMs:  latencyMs,
	}
	pairKey := continuityGroupModelProbePairKey(groupKey, modelID)

	continuityRecentRelaySuccesses.Lock()
	wallNow := time.Now().UTC().Unix()
	if wallNow-continuityRecentRelaySuccesses.lastCleanupAt >= int64(continuityRecentRelaySuccessCleanupInterval/time.Second) {
		cutoff := wallNow - int64(continuityRecentRelaySuccessRetention/time.Second)
		for key, entry := range continuityRecentRelaySuccesses.byPair {
			if entry.evidence.ObservedAt >= cutoff {
				continue
			}
			continuityRecentRelaySuccesses.order.Remove(entry.order)
			delete(continuityRecentRelaySuccesses.byPair, key)
		}
		continuityRecentRelaySuccesses.lastCleanupAt = wallNow
	}
	current, exists := continuityRecentRelaySuccesses.byPair[pairKey]
	if exists && evidence.ObservedAt >= current.evidence.ObservedAt {
		current.evidence = evidence
		continuityRecentRelaySuccesses.order.MoveToBack(current.order)
	} else if !exists {
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
		continuityRecentRelaySuccesses.byPair[pairKey] = &continuityRecentRelaySuccessEntry{
			evidence: evidence,
			order:    order,
		}
	}
	continuityRecentRelaySuccesses.Unlock()
}

func validContinuityRecentRelaySuccessKey(value string, maxBytes int) bool {
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
	evidence := continuityRecentRelaySuccess{}
	if exists {
		evidence = entry.evidence
	}
	continuityRecentRelaySuccesses.RUnlock()
	if !exists {
		return continuityRecentRelaySuccess{}, false
	}
	cutoff := now.UTC().Add(-maxAge).Unix()
	if evidence.ObservedAt < cutoff || evidence.ObservedAt > now.UTC().Unix() {
		return continuityRecentRelaySuccess{}, false
	}
	return evidence, true
}

package continuity

import (
	"sort"
	"time"

	"github.com/QuantumNous/new-api/model"
)

const (
	continuityUserTrafficFailureWindow = 5 * time.Minute
	continuityUserTrafficSuccessWindow = time.Minute
)

type continuityRelayOutcomePairEvidence struct {
	Buckets                []model.ContinuityRelayOutcomeBucket
	LatestSuccessAt        int64
	LatestSuccessLatencyMs int64
	LatestFailureAt        int64
}

func loadContinuityRelayOutcomeEvidence(
	windowStart int64,
	windowEnd int64,
) (map[string]*continuityRelayOutcomePairEvidence, error) {
	rows, err := model.GetContinuityRelayOutcomeBuckets(windowStart, windowEnd)
	if err != nil {
		return nil, err
	}
	byPair := make(map[string]*continuityRelayOutcomePairEvidence)
	bucketIndexes := make(map[string]map[int64]int)
	for _, row := range rows {
		pairKey := continuityGroupModelProbePairKey(row.GroupKey, row.ModelID)
		evidence := byPair[pairKey]
		if evidence == nil {
			evidence = &continuityRelayOutcomePairEvidence{}
			byPair[pairKey] = evidence
			bucketIndexes[pairKey] = make(map[int64]int)
		}
		bucketIndexes[pairKey][row.BucketTs] = len(evidence.Buckets)
		evidence.Buckets = append(evidence.Buckets, row)
		mergeContinuityRelayOutcomeLatest(evidence, row)
	}

	for pairKey, recent := range snapshotContinuityRecentRelayOutcomes() {
		evidence := byPair[pairKey]
		if evidence == nil {
			evidence = &continuityRelayOutcomePairEvidence{}
			byPair[pairKey] = evidence
			bucketIndexes[pairKey] = make(map[int64]int)
		}
		mergeRecentContinuityRelayOutcome(
			evidence,
			bucketIndexes[pairKey],
			recent,
			windowStart,
			windowEnd,
		)
	}

	for _, evidence := range byPair {
		sort.Slice(evidence.Buckets, func(i, j int) bool {
			return evidence.Buckets[i].BucketTs < evidence.Buckets[j].BucketTs
		})
	}
	return byPair, nil
}

func mergeContinuityRelayOutcomeLatest(
	evidence *continuityRelayOutcomePairEvidence,
	row model.ContinuityRelayOutcomeBucket,
) {
	if row.LatestSuccessAt >= evidence.LatestSuccessAt {
		evidence.LatestSuccessAt = row.LatestSuccessAt
		evidence.LatestSuccessLatencyMs = row.LatestSuccessLatencyMs
	}
	if row.LatestFailureAt > evidence.LatestFailureAt {
		evidence.LatestFailureAt = row.LatestFailureAt
	}
}

func mergeRecentContinuityRelayOutcome(
	evidence *continuityRelayOutcomePairEvidence,
	indexes map[int64]int,
	recent continuityRecentRelayOutcome,
	windowStart int64,
	windowEnd int64,
) {
	for _, success := range []bool{true, false} {
		observedAt := recent.LatestFailureAt
		if success {
			observedAt = recent.LatestSuccessAt
		}
		if observedAt < windowStart || observedAt > windowEnd || observedAt <= 0 {
			continue
		}
		bucketTs := observedAt - observedAt%model.ContinuityRelayOutcomeBucketSeconds
		index, exists := indexes[bucketTs]
		if !exists {
			index = len(evidence.Buckets)
			indexes[bucketTs] = index
			evidence.Buckets = append(evidence.Buckets, model.ContinuityRelayOutcomeBucket{
				BucketTs: bucketTs,
			})
		}
		row := &evidence.Buckets[index]
		if success {
			if observedAt > row.LatestSuccessAt {
				row.RequestCount++
				row.SuccessCount++
				row.SuccessLatencySumMs += recent.LatestSuccessLatencyMs
				row.LatestSuccessAt = observedAt
				row.LatestSuccessLatencyMs = recent.LatestSuccessLatencyMs
			}
		} else if observedAt > row.LatestFailureAt {
			row.RequestCount++
			row.FailureCount++
			row.LatestFailureAt = observedAt
		}
	}
	if recent.LatestSuccessAt >= evidence.LatestSuccessAt {
		evidence.LatestSuccessAt = recent.LatestSuccessAt
		evidence.LatestSuccessLatencyMs = recent.LatestSuccessLatencyMs
	}
	if recent.LatestFailureAt > evidence.LatestFailureAt {
		evidence.LatestFailureAt = recent.LatestFailureAt
	}
}

func continuityCurrentUserTrafficStatus(
	evidence *continuityRelayOutcomePairEvidence,
	now time.Time,
) (string, int64, int64, bool) {
	if evidence == nil {
		return continuityModelStatusUnknown, 0, 0, false
	}
	nowUnix := now.UTC().Unix()
	failureCutoff := now.UTC().Add(-continuityUserTrafficFailureWindow).Unix()
	successCutoff := now.UTC().Add(-continuityUserTrafficSuccessWindow).Unix()
	coverageCutoff := now.UTC().Add(-continuityGroupModelRecentSuccessWindow).Unix()
	failure5m := evidence.LatestFailureAt >= failureCutoff && evidence.LatestFailureAt <= nowUnix
	success1m := evidence.LatestSuccessAt >= successCutoff && evidence.LatestSuccessAt <= nowUnix
	success20m := evidence.LatestSuccessAt >= coverageCutoff && evidence.LatestSuccessAt <= nowUnix
	if failure5m {
		checkedAt := evidence.LatestFailureAt
		if success1m {
			if evidence.LatestSuccessAt > checkedAt {
				checkedAt = evidence.LatestSuccessAt
			}
			return continuityModelStatusDegraded,
				checkedAt,
				evidence.LatestSuccessLatencyMs,
				true
		}
		return continuityModelStatusUnavailable, checkedAt, 0, true
	}
	if success20m {
		return continuityModelStatusOperational,
			evidence.LatestSuccessAt,
			evidence.LatestSuccessLatencyMs,
			true
	}
	return continuityModelStatusUnknown, 0, 0, false
}

func mergeContinuityRelayOutcomeHistory(
	historyByPair map[string][]continuityGroupModelStatusHistoryPoint,
	outcomesByPair map[string]*continuityRelayOutcomePairEvidence,
	windowStart int64,
	windowEnd int64,
) map[string][]continuityGroupModelStatusHistoryPoint {
	for pairKey, evidence := range outcomesByPair {
		pointsByBucket := make(map[int]continuityGroupModelStatusHistoryPoint)
		for _, point := range historyByPair[pairKey] {
			bucketIndex := continuityStatusHistoryBucketIndex(point.CheckedAt, windowStart, windowEnd)
			if bucketIndex >= 0 {
				pointsByBucket[bucketIndex] = point
			}
		}
		type trafficBucket struct {
			successCount int64
			failureCount int64
			latencySumMs int64
			checkedAt    int64
		}
		trafficByBucket := make(map[int]trafficBucket)
		for _, row := range evidence.Buckets {
			if row.BucketTs < windowStart || row.BucketTs >= windowEnd ||
				(row.SuccessCount <= 0 && row.FailureCount <= 0) {
				continue
			}
			checkedAt := row.LatestSuccessAt
			if row.LatestFailureAt > checkedAt {
				checkedAt = row.LatestFailureAt
			}
			bucketIndex := continuityStatusHistoryBucketIndex(checkedAt, windowStart, windowEnd)
			if bucketIndex < 0 {
				continue
			}
			aggregate := trafficByBucket[bucketIndex]
			aggregate.successCount += row.SuccessCount
			aggregate.failureCount += row.FailureCount
			aggregate.latencySumMs += row.SuccessLatencySumMs
			if checkedAt > aggregate.checkedAt {
				aggregate.checkedAt = checkedAt
			}
			trafficByBucket[bucketIndex] = aggregate
		}
		for bucketIndex, aggregate := range trafficByBucket {
			status := continuityModelStatusUnavailable
			if aggregate.successCount > 0 && aggregate.failureCount > 0 {
				status = continuityModelStatusDegraded
			} else if aggregate.successCount > 0 {
				status = continuityModelStatusOperational
			}
			point := continuityGroupModelStatusHistoryPoint{
				CheckedAt: aggregate.checkedAt,
				Status:    status,
			}
			if aggregate.successCount > 0 {
				latencyMs := continuityStatusLatencyMs(
					aggregate.latencySumMs / aggregate.successCount,
				)
				point.LatencyMs = &latencyMs
			}
			pointsByBucket[bucketIndex] = point
		}
		bucketIndexes := make([]int, 0, len(pointsByBucket))
		for bucketIndex := range pointsByBucket {
			bucketIndexes = append(bucketIndexes, bucketIndex)
		}
		sort.Ints(bucketIndexes)
		points := make([]continuityGroupModelStatusHistoryPoint, 0, len(bucketIndexes))
		for _, bucketIndex := range bucketIndexes {
			points = append(points, pointsByBucket[bucketIndex])
		}
		historyByPair[pairKey] = points
	}
	return historyByPair
}

func continuityStatusHistoryBucketIndex(checkedAt int64, windowStart int64, windowEnd int64) int {
	if checkedAt < windowStart || checkedAt > windowEnd {
		return -1
	}
	bucketIndex := int((checkedAt - windowStart) / continuityStatusHistoryIntervalSeconds)
	if bucketIndex >= continuityStatusHistoryPointLimit {
		bucketIndex = continuityStatusHistoryPointLimit - 1
	}
	return bucketIndex
}

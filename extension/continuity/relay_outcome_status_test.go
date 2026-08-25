package continuity

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCurrentUserTrafficStatusUsesOneFiveMinuteEvidenceWindow(t *testing.T) {
	now := time.Date(2030, time.January, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		evidence *continuityRelayOutcomePairEvidence
		status   string
		observed bool
	}{
		{
			name: "failure only is red",
			evidence: &continuityRelayOutcomePairEvidence{
				LatestFailureAt: now.Add(-4 * time.Minute).Unix(),
			},
			status: continuityModelStatusUnavailable, observed: true,
		},
		{
			name: "success and failure are yellow",
			evidence: &continuityRelayOutcomePairEvidence{
				LatestFailureAt:        now.Add(-4 * time.Minute).Unix(),
				LatestSuccessAt:        now.Add(-90 * time.Second).Unix(),
				LatestSuccessLatencyMs: 125,
			},
			status: continuityModelStatusDegraded, observed: true,
		},
		{
			name: "success only is green",
			evidence: &continuityRelayOutcomePairEvidence{
				LatestSuccessAt:        now.Add(-4 * time.Minute).Unix(),
				LatestSuccessLatencyMs: 250,
			},
			status: continuityModelStatusOperational, observed: true,
		},
		{
			name: "failure boundary is inclusive",
			evidence: &continuityRelayOutcomePairEvidence{
				LatestFailureAt: now.Add(-5 * time.Minute).Unix(),
			},
			status: continuityModelStatusUnavailable, observed: true,
		},
		{
			name: "success and failure boundary is inclusive",
			evidence: &continuityRelayOutcomePairEvidence{
				LatestFailureAt: now.Add(-5 * time.Minute).Unix(),
				LatestSuccessAt: now.Add(-5 * time.Minute).Unix(),
			},
			status: continuityModelStatusDegraded, observed: true,
		},
		{
			name: "stale traffic falls back",
			evidence: &continuityRelayOutcomePairEvidence{
				LatestFailureAt: now.Add(-6 * time.Minute).Unix(),
				LatestSuccessAt: now.Add(-6 * time.Minute).Unix(),
			},
			status: continuityModelStatusUnknown, observed: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, _, _, observed := continuityCurrentUserTrafficStatus(test.evidence, now)
			assert.Equal(t, test.status, status)
			assert.Equal(t, test.observed, observed)
		})
	}
}

func TestRelayTrafficHistoryOverridesProbeAndAggregatesEveryOutcome(t *testing.T) {
	windowStart := time.Date(2030, time.January, 1, 12, 0, 0, 0, time.UTC).Unix()
	windowEnd := windowStart + int64(24*time.Hour/time.Second)
	pairKey := continuityGroupModelProbePairKey("standard", "model-a")
	probeLatency := int64(999)
	history := map[string][]continuityGroupModelStatusHistoryPoint{
		pairKey: {
			{
				CheckedAt: windowStart + 5*continuityStatusHistoryIntervalSeconds + 10,
				Status:    continuityModelStatusOperational,
				LatencyMs: &probeLatency,
			},
		},
	}
	outcomes := map[string]*continuityRelayOutcomePairEvidence{
		pairKey: {
			Buckets: []model.ContinuityRelayOutcomeBucket{
				{
					BucketTs:            windowStart + 5*continuityStatusHistoryIntervalSeconds,
					SuccessCount:        2,
					FailureCount:        1,
					SuccessLatencySumMs: 300,
					LatestSuccessAt:     windowStart + 5*continuityStatusHistoryIntervalSeconds + 30,
					LatestFailureAt:     windowStart + 5*continuityStatusHistoryIntervalSeconds + 40,
				},
				{
					BucketTs:            windowStart + 6*continuityStatusHistoryIntervalSeconds,
					SuccessCount:        1,
					SuccessLatencySumMs: 250,
					LatestSuccessAt:     windowStart + 6*continuityStatusHistoryIntervalSeconds + 20,
				},
				{
					BucketTs:        windowStart + 7*continuityStatusHistoryIntervalSeconds,
					FailureCount:    3,
					LatestFailureAt: windowStart + 7*continuityStatusHistoryIntervalSeconds + 50,
				},
			},
		},
	}

	merged := mergeContinuityRelayOutcomeHistory(history, outcomes, windowStart, windowEnd)
	require.Len(t, merged[pairKey], 3)
	assert.Equal(t, continuityModelStatusDegraded, merged[pairKey][0].Status)
	require.NotNil(t, merged[pairKey][0].LatencyMs)
	assert.Equal(t, int64(150), *merged[pairKey][0].LatencyMs)
	assert.Equal(t, continuityModelStatusOperational, merged[pairKey][1].Status)
	assert.Equal(t, continuityModelStatusUnavailable, merged[pairKey][2].Status)
}

func TestRelayTrafficHistoryBucketsByObservedTimeAcrossShiftedWindowBoundary(t *testing.T) {
	windowStart := time.Date(2030, time.January, 1, 12, 0, 30, 0, time.UTC).Unix()
	windowEnd := windowStart + int64(24*time.Hour/time.Second)
	pairKey := continuityGroupModelProbePairKey("standard", "model-a")
	probeCheckedAt := windowStart + continuityStatusHistoryIntervalSeconds + 5
	trafficCheckedAt := probeCheckedAt + 5
	history := map[string][]continuityGroupModelStatusHistoryPoint{
		pairKey: {
			{
				CheckedAt: probeCheckedAt,
				Status:    continuityModelStatusUnavailable,
			},
		},
	}
	outcomes := map[string]*continuityRelayOutcomePairEvidence{
		pairKey: {
			Buckets: []model.ContinuityRelayOutcomeBucket{
				{
					BucketTs:            windowStart + continuityStatusHistoryIntervalSeconds - 30,
					SuccessCount:        1,
					SuccessLatencySumMs: 120,
					LatestSuccessAt:     trafficCheckedAt,
				},
			},
		},
	}

	merged := mergeContinuityRelayOutcomeHistory(history, outcomes, windowStart, windowEnd)
	require.Len(t, merged[pairKey], 1)
	assert.Equal(t, trafficCheckedAt, merged[pairKey][0].CheckedAt)
	assert.Equal(t, continuityModelStatusOperational, merged[pairKey][0].Status)
}

func TestContinuityRelayOutcomeBucketPersistsAllFinalCallsAndKeepsLatestTimes(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	bucketTs := time.Date(2030, time.January, 2, 12, 0, 0, 0, time.UTC).Unix()
	rows := []*model.ContinuityRelayOutcomeBucket{
		{
			GroupKey: "standard", ModelID: "model-a", BucketTs: bucketTs,
			RequestCount: 1, FailureCount: 1, LatestFailureAt: bucketTs + 40,
		},
		{
			GroupKey: "standard", ModelID: "model-a", BucketTs: bucketTs,
			RequestCount: 1, SuccessCount: 1, SuccessLatencySumMs: 125,
			LatestSuccessAt: bucketTs + 50, LatestSuccessLatencyMs: 125,
		},
		{
			GroupKey: "standard", ModelID: "model-a", BucketTs: bucketTs,
			RequestCount: 1, IgnoredFailureCount: 1,
		},
		{
			GroupKey: "standard", ModelID: "model-a", BucketTs: bucketTs,
			RequestCount: 1, SuccessCount: 1, SuccessLatencySumMs: 999,
			LatestSuccessAt: bucketTs + 20, LatestSuccessLatencyMs: 999,
		},
	}
	for _, row := range rows {
		require.NoError(t, model.UpsertContinuityRelayOutcomeBucket(row))
	}
	var stored model.ContinuityRelayOutcomeBucket
	require.NoError(t, database.First(&stored).Error)
	assert.Equal(t, int64(4), stored.RequestCount)
	assert.Equal(t, int64(2), stored.SuccessCount)
	assert.Equal(t, int64(1), stored.FailureCount)
	assert.Equal(t, int64(1), stored.IgnoredFailureCount)
	assert.Equal(t, int64(1124), stored.SuccessLatencySumMs)
	assert.Equal(t, bucketTs+50, stored.LatestSuccessAt)
	assert.Equal(t, int64(125), stored.LatestSuccessLatencyMs)
	assert.Equal(t, bucketTs+40, stored.LatestFailureAt)
}

package continuity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/extension"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroupModelStatusSnapshotReturnsExactConfiguredMatrixAndPassiveEvidence(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, database.AutoMigrate(&model.PerfMetric{}))

	now := time.Now().UTC().Truncate(time.Second)
	recentBucket := now.Add(-time.Hour).Truncate(time.Hour).Unix()
	staleBucket := now.Add(-25 * time.Hour).Truncate(time.Hour).Unix()

	require.NoError(t, database.Create(&[]model.Ability{
		{Group: "group-beta", Model: "model-z", ChannelId: 30, Enabled: true},
		{Group: "group-alpha", Model: "model-b", ChannelId: 20, Enabled: true},
		{Group: "group-alpha", Model: "model-c", ChannelId: 21, Enabled: true},
		{Group: "group-alpha", Model: "model-a", ChannelId: 10, Enabled: true},
		{Group: "group-alpha", Model: "model-a", ChannelId: 11, Enabled: true},
		{Group: "group-alpha", Model: "model-hidden", ChannelId: 12, Enabled: false},
	}).Error)
	require.NoError(t, database.Create(&[]model.PerfMetric{
		{
			ModelName:      "model-a",
			Group:          "group-alpha",
			BucketTs:       recentBucket,
			RequestCount:   4,
			SuccessCount:   4,
			TotalLatencyMs: 400,
		},
		{
			ModelName:      "model-b",
			Group:          "group-alpha",
			BucketTs:       recentBucket,
			RequestCount:   4,
			SuccessCount:   3,
			TotalLatencyMs: 1000,
		},
		{
			ModelName:      "model-c",
			Group:          "group-alpha",
			BucketTs:       recentBucket,
			RequestCount:   20000,
			SuccessCount:   19999,
			TotalLatencyMs: 2000000,
		},
		{
			ModelName:      "model-z",
			Group:          "group-beta",
			BucketTs:       staleBucket,
			RequestCount:   1,
			SuccessCount:   1,
			TotalLatencyMs: 50,
		},
		{
			ModelName:      "model-a",
			Group:          "not-configured",
			BucketTs:       recentBucket,
			RequestCount:   2,
			SuccessCount:   2,
			TotalLatencyMs: 60,
		},
	}).Error)

	snapshot, err := groupModelStatusSnapshot(now)
	require.NoError(t, err)

	assert.Equal(t, 1, snapshot.SchemaVersion)
	assert.Equal(t, now.Unix(), snapshot.GeneratedAt)
	assert.Equal(t, now.Add(-24*time.Hour).Unix(), snapshot.Window.StartAt)
	assert.Equal(t, now.Unix(), snapshot.Window.EndAt)
	require.Len(t, snapshot.Groups, 2)

	alpha := snapshot.Groups[0]
	assert.Equal(t, "group-alpha", alpha.GroupKey)
	assert.Equal(t, continuityModelStatusDegraded, alpha.Status)
	require.Len(t, alpha.Models, 3)

	operational := alpha.Models[0]
	assert.Equal(t, "model-a", operational.ModelID)
	assert.Equal(t, 2, operational.EligibleChannelCount)
	assert.Equal(t, continuityModelStatusOperational, operational.Status)
	assert.Equal(t, continuityStatusSourcePassiveTraffic, operational.StatusSource)
	require.NotNil(t, operational.LatencyMs)
	assert.Equal(t, int64(100), *operational.LatencyMs)
	require.NotNil(t, operational.LastCheckedAt)
	assert.Equal(t, recentBucket, *operational.LastCheckedAt)
	require.NotNil(t, operational.Evidence.Passive)
	assert.Equal(t, float64(100), operational.Evidence.Passive.SuccessRate)
	assert.Equal(t, int64(100), operational.Evidence.Passive.AverageLatencyMs)
	assert.Nil(t, operational.Evidence.ActiveProbe)

	degraded := alpha.Models[1]
	assert.Equal(t, "model-b", degraded.ModelID)
	assert.Equal(t, 1, degraded.EligibleChannelCount)
	assert.Equal(t, continuityModelStatusDegraded, degraded.Status)
	require.NotNil(t, degraded.Evidence.Passive)
	assert.Equal(t, float64(75), degraded.Evidence.Passive.SuccessRate)
	assert.Equal(t, int64(250), degraded.Evidence.Passive.AverageLatencyMs)

	highVolumeSingleFailure := alpha.Models[2]
	assert.Equal(t, "model-c", highVolumeSingleFailure.ModelID)
	assert.Equal(t, continuityModelStatusDegraded, highVolumeSingleFailure.Status)
	require.NotNil(t, highVolumeSingleFailure.Evidence.Passive)
	assert.InDelta(
		t,
		float64(19999)/float64(20000)*100,
		highVolumeSingleFailure.Evidence.Passive.SuccessRate,
		0.0000001,
	)

	beta := snapshot.Groups[1]
	assert.Equal(t, "group-beta", beta.GroupKey)
	assert.Equal(t, continuityModelStatusUnknown, beta.Status)
	require.Len(t, beta.Models, 1)
	assert.Equal(t, "model-z", beta.Models[0].ModelID)
	assert.Equal(t, continuityModelStatusUnknown, beta.Models[0].Status)
	assert.Equal(t, continuityStatusSourceNone, beta.Models[0].StatusSource)
	assert.Nil(t, beta.Models[0].LatencyMs)
	assert.Nil(t, beta.Models[0].LastCheckedAt)
	assert.Nil(t, beta.Models[0].Evidence.Passive)
	assert.Nil(t, beta.Models[0].Evidence.ActiveProbe)
}

func TestGroupModelStatusSnapshotClampsPassiveLatencyToGatewayContract(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, database.AutoMigrate(&model.PerfMetric{}))

	now := time.Now().UTC().Truncate(time.Second)
	recentBucket := now.Add(-time.Hour).Truncate(time.Hour).Unix()
	require.NoError(t, database.Create(&model.Ability{
		Group:     "standard",
		Model:     "long-running-model",
		ChannelId: 1,
		Enabled:   true,
	}).Error)
	require.NoError(t, database.Create(&model.PerfMetric{
		ModelName:      "long-running-model",
		Group:          "standard",
		BucketTs:       recentBucket,
		RequestCount:   1,
		SuccessCount:   1,
		TotalLatencyMs: continuityStatusMaxLatencyMs + 1,
	}).Error)

	snapshot, err := groupModelStatusSnapshot(now)
	require.NoError(t, err)
	require.Len(t, snapshot.Groups, 1)
	require.Len(t, snapshot.Groups[0].Models, 1)
	state := snapshot.Groups[0].Models[0]
	require.NotNil(t, state.LatencyMs)
	assert.Equal(t, continuityStatusMaxLatencyMs, *state.LatencyMs)
	require.NotNil(t, state.Evidence.Passive)
	assert.Equal(t, continuityStatusMaxLatencyMs, state.Evidence.Passive.AverageLatencyMs)
}

func TestGroupModelStatusSnapshotUsesExactRecentRelaySuccessImmediately(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, database.AutoMigrate(&model.PerfMetric{}))
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, database.Create(&model.Ability{
		Group:     "standard",
		Model:     "model-a",
		ChannelId: 1,
		Enabled:   true,
	}).Error)
	recordContinuityRecentRelaySuccess(extension.RelaySuccessEvent{
		Group:      "standard",
		Model:      "model-a",
		ObservedAt: now.Add(-10 * time.Second),
		LatencyMs:  125,
	})

	snapshot, err := groupModelStatusSnapshot(now)
	require.NoError(t, err)
	require.Len(t, snapshot.Groups, 1)
	require.Len(t, snapshot.Groups[0].Models, 1)
	state := snapshot.Groups[0].Models[0]
	assert.Equal(t, continuityModelStatusOperational, state.Status)
	assert.Equal(t, continuityStatusSourceRealTraffic, state.StatusSource)
	require.NotNil(t, state.Evidence.RealTraffic)
	assert.Equal(t, now.Add(-10*time.Second).Unix(), state.Evidence.RealTraffic.ObservedAt)
	assert.Nil(t, state.Evidence.Passive)
}

func TestGroupModelStatusSnapshotUsesPersistedTrafficCoveredTaskAfterStoreLoss(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, database.AutoMigrate(&model.PerfMetric{}))
	now := time.Now().UTC().Truncate(time.Second)
	observedAt := now.Add(-time.Minute).Unix()
	require.NoError(t, database.Create(&model.Ability{
		Group:     "standard",
		Model:     "model-a",
		ChannelId: 1,
		Enabled:   true,
	}).Error)
	resultJSON, err := common.Marshal(continuityGroupModelProbeResult{
		SchemaVersion: 1,
		CheckedAt:     now.Unix(),
		Pairs: []continuityGroupModelProbeEvidence{{
			GroupKey:  "standard",
			ModelID:   "model-a",
			Status:    continuityModelStatusOperational,
			Source:    continuityStatusSourceRealTraffic,
			CheckedAt: observedAt,
			LatencyMs: 140,
		}},
	})
	require.NoError(t, err)
	require.NoError(t, database.Create(&model.SystemTask{
		TaskID:    "systask_persisted_real_traffic",
		Type:      continuityGroupModelProbeTaskType,
		Status:    model.SystemTaskStatusSucceeded,
		Result:    string(resultJSON),
		UpdatedAt: now.Unix(),
	}).Error)

	snapshot, err := groupModelStatusSnapshot(now)
	require.NoError(t, err)
	state := snapshot.Groups[0].Models[0]
	assert.Equal(t, continuityStatusSourceRealTraffic, state.StatusSource)
	assert.Equal(t, continuityModelStatusOperational, state.Status)
	require.NotNil(t, state.Evidence.RealTraffic)
	assert.Equal(t, observedAt, state.Evidence.RealTraffic.ObservedAt)
}

func TestOlderRealTrafficDoesNotOverrideNewerManualProbeInsidePassiveBucket(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, database.AutoMigrate(&model.PerfMetric{}))
	now := time.Now().UTC().Truncate(time.Second)
	bucketSeconds := int64(perf_metrics_setting.GetBucketSeconds())
	bucketStart := now.Unix() - now.Unix()%bucketSeconds
	activeCheckedAt := now.Add(-time.Minute).Unix()
	realObservedAt := now.Add(-2 * time.Minute)
	require.NoError(t, database.Create(&model.Ability{
		Group:     "standard",
		Model:     "model-a",
		ChannelId: 1,
		Enabled:   true,
	}).Error)
	require.NoError(t, database.Create(&model.PerfMetric{
		ModelName:      "model-a",
		Group:          "standard",
		BucketTs:       bucketStart,
		RequestCount:   4,
		SuccessCount:   3,
		TotalLatencyMs: 400,
	}).Error)
	activeResultJSON, err := common.Marshal(continuityGroupModelProbeResult{
		SchemaVersion: 1,
		CheckedAt:     activeCheckedAt,
		Pairs: []continuityGroupModelProbeEvidence{{
			GroupKey:  "standard",
			ModelID:   "model-a",
			Status:    continuityModelStatusUnavailable,
			Source:    continuityStatusSourceActiveProbe,
			CheckedAt: activeCheckedAt,
		}},
	})
	require.NoError(t, err)
	require.NoError(t, database.Create(&model.SystemTask{
		TaskID:    "systask_newer_manual_probe",
		Type:      continuityGroupModelProbeTaskType,
		Status:    model.SystemTaskStatusSucceeded,
		Result:    string(activeResultJSON),
		UpdatedAt: activeCheckedAt,
	}).Error)
	recordContinuityRecentRelaySuccess(extension.RelaySuccessEvent{
		Group:      "standard",
		Model:      "model-a",
		ObservedAt: realObservedAt,
		LatencyMs:  100,
	})

	snapshot, err := groupModelStatusSnapshot(now)
	require.NoError(t, err)
	state := snapshot.Groups[0].Models[0]
	assert.Equal(t, continuityStatusSourcePassiveTraffic, state.StatusSource)
	assert.Equal(t, continuityModelStatusDegraded, state.Status)
	require.NotNil(t, state.Evidence.RealTraffic)
	assert.Equal(t, realObservedAt.Unix(), state.Evidence.RealTraffic.ObservedAt)
	require.NotNil(t, state.Evidence.ActiveProbe)
	assert.Equal(t, activeCheckedAt, state.Evidence.ActiveProbe.CheckedAt)
}

func TestGroupModelStatusSnapshotOmitsOnlyTheExactExcludedPairAndItsEvidence(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, database.AutoMigrate(&model.PerfMetric{}))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(
		`{"direct":1,"standard":1}`,
	))
	now := time.Now().UTC().Truncate(time.Second)
	recentBucket := now.Add(-time.Hour).Truncate(time.Hour).Unix()
	require.NoError(t, database.Create(&[]model.Ability{
		{Group: "direct", Model: "shared-model", ChannelId: 1, Enabled: true},
		{Group: "standard", Model: "shared-model", ChannelId: 2, Enabled: true},
	}).Error)
	require.NoError(t, database.Create(&[]model.PerfMetric{
		{
			ModelName:      "shared-model",
			Group:          "direct",
			BucketTs:       recentBucket,
			RequestCount:   4,
			SuccessCount:   4,
			TotalLatencyMs: 400,
		},
		{
			ModelName:      "shared-model",
			Group:          "standard",
			BucketTs:       recentBucket,
			RequestCount:   4,
			SuccessCount:   0,
			TotalLatencyMs: 4000,
		},
	}).Error)
	_, err := replaceContinuityGroupModelProbeExclusions(
		[]continuityGroupModelProbeExclusion{{
			GroupKey: "standard",
			ModelID:  "shared-model",
		}},
	)
	require.NoError(t, err)
	resultJSON, err := common.Marshal(continuityGroupModelProbeResult{
		SchemaVersion: continuityGroupModelStatusSchemaVersion,
		CheckedAt:     now.Unix(),
		Pairs: []continuityGroupModelProbeEvidence{
			{
				GroupKey:  "direct",
				ModelID:   "shared-model",
				Status:    continuityModelStatusOperational,
				CheckedAt: now.Unix(),
				LatencyMs: 125,
			},
			{
				GroupKey:  "standard",
				ModelID:   "shared-model",
				Status:    continuityModelStatusUnavailable,
				CheckedAt: now.Unix(),
				LatencyMs: 9000,
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, database.Create(&model.SystemTask{
		TaskID:    "systask_status_excluded_evidence",
		Type:      continuityGroupModelProbeTaskType,
		Status:    model.SystemTaskStatusSucceeded,
		Result:    string(resultJSON),
		UpdatedAt: now.Unix(),
	}).Error)

	snapshot, err := groupModelStatusSnapshot(now)
	require.NoError(t, err)
	require.Len(t, snapshot.Groups, 1)
	assert.Equal(t, "direct", snapshot.Groups[0].GroupKey)
	require.Len(t, snapshot.Groups[0].Models, 1)
	visible := snapshot.Groups[0].Models[0]
	assert.Equal(t, "shared-model", visible.ModelID)
	assert.Equal(t, continuityStatusSourceActiveProbe, visible.StatusSource)
	require.NotNil(t, visible.Evidence.Passive)
	assert.Equal(t, float64(100), visible.Evidence.Passive.SuccessRate)
	require.NotNil(t, visible.Evidence.ActiveProbe)
	assert.Equal(t, int64(125), visible.Evidence.ActiveProbe.LatencyMs)
	require.Len(t, visible.History.Points, 1)
	assert.Equal(t, continuityModelStatusOperational, visible.History.Points[0].Status)
}

func TestGroupModelStatusSnapshotReturnsBucketedProbeHistory(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, database.AutoMigrate(&model.PerfMetric{}))

	now := time.Date(2030, time.January, 2, 0, 0, 0, 0, time.UTC)
	windowStart := now.Add(-24 * time.Hour)
	require.NoError(t, database.Create(&model.Ability{
		Group:     "group-history",
		Model:     "model-history",
		ChannelId: 42,
		Enabled:   true,
	}).Error)

	result := continuityGroupModelProbeResult{
		SchemaVersion: continuityGroupModelStatusSchemaVersion,
		CheckedAt:     now.Unix(),
		Pairs: []continuityGroupModelProbeEvidence{
			{
				GroupKey:  "group-history",
				ModelID:   "model-history",
				Status:    continuityModelStatusOperational,
				CheckedAt: windowStart.Add(time.Minute).Unix(),
				LatencyMs: 300,
			},
			{
				GroupKey:  "group-history",
				ModelID:   "model-history",
				Status:    continuityModelStatusDegraded,
				CheckedAt: windowStart.Add(10 * time.Minute).Unix(),
				LatencyMs: 450,
			},
			{
				GroupKey:  "group-history",
				ModelID:   "model-history",
				Status:    continuityModelStatusUnavailable,
				CheckedAt: now.Add(-time.Minute).Unix(),
			},
			{
				GroupKey:  "group-history",
				ModelID:   "model-history",
				Status:    continuityModelStatusOperational,
				CheckedAt: windowStart.Add(-time.Second).Unix(),
				LatencyMs: 100,
			},
		},
	}
	resultJSON, err := common.Marshal(result)
	require.NoError(t, err)
	require.NoError(t, database.Create(&model.SystemTask{
		TaskID:    "systask_status_history_corrupt_old",
		Type:      continuityGroupModelProbeTaskType,
		Status:    model.SystemTaskStatusSucceeded,
		Result:    "{",
		UpdatedAt: now.Add(-time.Hour).Unix(),
	}).Error)
	require.NoError(t, database.Create(&model.SystemTask{
		TaskID:    "systask_status_history",
		Type:      continuityGroupModelProbeTaskType,
		Status:    model.SystemTaskStatusSucceeded,
		Result:    string(resultJSON),
		UpdatedAt: now.Unix(),
	}).Error)

	snapshot, err := groupModelStatusSnapshot(now)
	require.NoError(t, err)
	require.Len(t, snapshot.Groups, 1)
	require.Len(t, snapshot.Groups[0].Models, 1)
	history := snapshot.Groups[0].Models[0].History
	assert.Equal(t, windowStart.Unix(), history.WindowStartAt)
	assert.Equal(t, now.Unix(), history.WindowEndAt)
	assert.Equal(t, int64(20*60), history.IntervalSeconds)
	require.Len(t, history.Points, 2)
	assert.Equal(t, windowStart.Add(10*time.Minute).Unix(), history.Points[0].CheckedAt)
	assert.Equal(t, continuityModelStatusDegraded, history.Points[0].Status)
	require.NotNil(t, history.Points[0].LatencyMs)
	assert.Equal(t, int64(450), *history.Points[0].LatencyMs)
	assert.Equal(t, now.Add(-time.Minute).Unix(), history.Points[1].CheckedAt)
	assert.Equal(t, continuityModelStatusUnavailable, history.Points[1].Status)
	assert.Nil(t, history.Points[1].LatencyMs)
}

func TestGroupModelProbeHistoryKeepsAtMostSeventyTwoLatestBucketPoints(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	now := time.Date(2030, time.January, 2, 0, 0, 0, 0, time.UTC)

	pairs := make([]continuityGroupModelProbeEvidence, 0, 73)
	for index := 72; index >= 0; index-- {
		pairs = append(pairs, continuityGroupModelProbeEvidence{
			GroupKey:  "group-history",
			ModelID:   "model-history",
			Status:    continuityModelStatusOperational,
			CheckedAt: now.Add(-time.Duration(index) * 20 * time.Minute).Unix(),
			LatencyMs: int64(index + 1),
		})
	}
	resultJSON, err := common.Marshal(continuityGroupModelProbeResult{
		SchemaVersion: continuityGroupModelStatusSchemaVersion,
		CheckedAt:     now.Unix(),
		Pairs:         pairs,
	})
	require.NoError(t, err)
	require.NoError(t, database.Create(&model.SystemTask{
		TaskID:    "systask_status_history_limit",
		Type:      continuityGroupModelProbeTaskType,
		Status:    model.SystemTaskStatusSucceeded,
		Result:    string(resultJSON),
		UpdatedAt: now.Unix(),
	}).Error)

	historyByPair, windowStart, windowEnd, err := continuityGroupModelProbeHistory(now)
	require.NoError(t, err)
	assert.Equal(t, now.Add(-24*time.Hour).Unix(), windowStart)
	assert.Equal(t, now.Unix(), windowEnd)
	points := historyByPair[continuityGroupModelProbePairKey("group-history", "model-history")]
	require.Len(t, points, continuityStatusHistoryPointLimit)
	assert.Equal(t, now.Add(-24*time.Hour).Unix(), points[0].CheckedAt)
	assert.Equal(t, now.Unix(), points[len(points)-1].CheckedAt)
	require.NotNil(t, points[len(points)-1].LatencyMs)
	assert.Equal(t, int64(1), *points[len(points)-1].LatencyMs)
}

func TestGroupModelStatusEndpointRequiresSecretAndReturnsVersionedEnvelope(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, database.AutoMigrate(&model.PerfMetric{}))
	require.NoError(t, database.Create(&model.Ability{
		Group:     "group-contract",
		Model:     "model-contract",
		ChannelId: 42,
		Enabled:   true,
	}).Error)
	secret := strings.Repeat("s", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(plugin{}).Mount(router)

	unauthorizedRequest := httptest.NewRequest(
		http.MethodGet,
		"/internal/continuity/group-model-status",
		nil,
	)
	unauthorizedResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedResponse, unauthorizedRequest)
	require.Equal(t, http.StatusUnauthorized, unauthorizedResponse.Code)

	authorizedRequest := httptest.NewRequest(
		http.MethodGet,
		"/internal/continuity/group-model-status",
		nil,
	)
	authorizedRequest.Header.Set("Authorization", "Bearer "+secret)
	authorizedResponse := httptest.NewRecorder()
	router.ServeHTTP(authorizedResponse, authorizedRequest)
	require.Equal(t, http.StatusOK, authorizedResponse.Code)
	assert.Equal(t, "no-store", authorizedResponse.Header().Get("Cache-Control"))

	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			SchemaVersion int   `json:"schema_version"`
			GeneratedAt   int64 `json:"generated_at"`
			Window        struct {
				StartAt int64 `json:"start_at"`
				EndAt   int64 `json:"end_at"`
			} `json:"window"`
			Groups []struct {
				GroupKey string `json:"group_key"`
				Status   string `json:"status"`
				Models   []struct {
					ModelID              string `json:"model_id"`
					EligibleChannelCount int    `json:"eligible_channel_count"`
					Status               string `json:"status"`
					StatusSource         string `json:"status_source"`
					History              struct {
						WindowStartAt   int64                                    `json:"window_start_at"`
						WindowEndAt     int64                                    `json:"window_end_at"`
						IntervalSeconds int64                                    `json:"interval_seconds"`
						Points          []continuityGroupModelStatusHistoryPoint `json:"points"`
					} `json:"history"`
					Evidence struct {
						Passive     any `json:"passive"`
						ActiveProbe any `json:"active_probe"`
					} `json:"evidence"`
				} `json:"models"`
			} `json:"groups"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(authorizedResponse.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Empty(t, response.Message)
	assert.Equal(t, continuityGroupModelStatusSchemaVersion, response.Data.SchemaVersion)
	assert.NotZero(t, response.Data.GeneratedAt)
	require.Len(t, response.Data.Groups, 1)
	assert.Equal(t, "group-contract", response.Data.Groups[0].GroupKey)
	assert.Equal(t, continuityModelStatusUnknown, response.Data.Groups[0].Status)
	require.Len(t, response.Data.Groups[0].Models, 1)
	assert.Equal(t, "model-contract", response.Data.Groups[0].Models[0].ModelID)
	assert.Equal(t, 1, response.Data.Groups[0].Models[0].EligibleChannelCount)
	assert.Equal(t, continuityModelStatusUnknown, response.Data.Groups[0].Models[0].Status)
	assert.Equal(t, continuityStatusSourceNone, response.Data.Groups[0].Models[0].StatusSource)
	history := response.Data.Groups[0].Models[0].History
	assert.Equal(
		t,
		int64(24*time.Hour/time.Second),
		history.WindowEndAt-history.WindowStartAt,
	)
	assert.Equal(t, continuityStatusHistoryIntervalSeconds, history.IntervalSeconds)
	assert.Empty(t, history.Points)
	assert.Nil(t, response.Data.Groups[0].Models[0].Evidence.Passive)
	assert.Nil(t, response.Data.Groups[0].Models[0].Evidence.ActiveProbe)
}

func TestContinuityAggregateGroupStatusRequiresEveryModelToBeUnavailable(t *testing.T) {
	tests := []struct {
		name     string
		statuses []string
		expected string
	}{
		{
			name:     "all unavailable",
			statuses: []string{continuityModelStatusUnavailable, continuityModelStatusUnavailable},
			expected: continuityModelStatusUnavailable,
		},
		{
			name:     "unavailable and operational",
			statuses: []string{continuityModelStatusUnavailable, continuityModelStatusOperational},
			expected: continuityModelStatusDegraded,
		},
		{
			name:     "all operational",
			statuses: []string{continuityModelStatusOperational, continuityModelStatusOperational},
			expected: continuityModelStatusOperational,
		},
		{
			name:     "operational and unknown",
			statuses: []string{continuityModelStatusOperational, continuityModelStatusUnknown},
			expected: continuityModelStatusUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			models := make([]continuityRoutingModelState, 0, len(test.statuses))
			for _, status := range test.statuses {
				models = append(models, continuityRoutingModelState{Status: status})
			}
			assert.Equal(t, test.expected, continuityAggregateGroupStatus(models))
		})
	}
}

func TestGroupModelStatusSnapshotUsesTheNewestEvidenceSource(t *testing.T) {
	bucketDuration := time.Duration(perf_metrics_setting.GetBucketSeconds()) * time.Second
	recentBucketAgo := bucketDuration / 2
	tests := []struct {
		name             string
		passiveBucketAgo time.Duration
		activeCheckedAgo time.Duration
		expectedStatus   string
		expectedSource   string
		expectedLatency  *int64
	}{
		{
			name:             "newer passive traffic is not hidden by an older probe",
			passiveBucketAgo: recentBucketAgo,
			activeCheckedAgo: recentBucketAgo + time.Second,
			expectedStatus:   continuityModelStatusOperational,
			expectedSource:   continuityStatusSourcePassiveTraffic,
			expectedLatency: func() *int64 {
				value := int64(100)
				return &value
			}(),
		},
		{
			name:             "active probe inside the same bucket cannot hide a later passive sample",
			passiveBucketAgo: recentBucketAgo,
			activeCheckedAgo: recentBucketAgo - time.Second,
			expectedStatus:   continuityModelStatusOperational,
			expectedSource:   continuityStatusSourcePassiveTraffic,
			expectedLatency: func() *int64 {
				value := int64(100)
				return &value
			}(),
		},
		{
			name:             "active probe after a completed passive bucket takes precedence",
			passiveBucketAgo: 2 * bucketDuration,
			activeCheckedAgo: time.Second,
			expectedStatus:   continuityModelStatusUnavailable,
			expectedSource:   continuityStatusSourceActiveProbe,
			expectedLatency:  nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := setupContinuityManagedGroupServiceTest(t)
			require.NoError(t, database.AutoMigrate(&model.PerfMetric{}))
			now := time.Now().UTC().Truncate(time.Second)
			passiveBucket := now.Add(-test.passiveBucketAgo).Unix()
			activeCheckedAt := now.Add(-test.activeCheckedAgo).Unix()
			require.NoError(t, database.Create(&model.Ability{
				Group:     "standard",
				Model:     "model-a",
				ChannelId: 1,
				Enabled:   true,
			}).Error)
			require.NoError(t, database.Create(&model.PerfMetric{
				ModelName:      "model-a",
				Group:          "standard",
				BucketTs:       passiveBucket,
				RequestCount:   4,
				SuccessCount:   4,
				TotalLatencyMs: 400,
			}).Error)
			activeResult := continuityGroupModelProbeResult{
				SchemaVersion: 1,
				CheckedAt:     activeCheckedAt,
				Pairs: []continuityGroupModelProbeEvidence{{
					GroupKey:  "standard",
					ModelID:   "model-a",
					Status:    continuityModelStatusUnavailable,
					CheckedAt: activeCheckedAt,
				}},
			}
			activeResultJSON, err := common.Marshal(activeResult)
			require.NoError(t, err)
			require.NoError(t, database.Create(&model.SystemTask{
				TaskID: "systask_status_precedence",
				Type:   continuityGroupModelProbeTaskType,
				Status: model.SystemTaskStatusSucceeded,
				Result: string(activeResultJSON),
			}).Error)

			snapshot, err := groupModelStatusSnapshot(now)
			require.NoError(t, err)
			require.Len(t, snapshot.Groups, 1)
			require.Len(t, snapshot.Groups[0].Models, 1)
			status := snapshot.Groups[0].Models[0]
			assert.Equal(t, test.expectedStatus, status.Status)
			assert.Equal(t, test.expectedSource, status.StatusSource)
			assert.Equal(t, test.expectedLatency, status.LatencyMs)
			require.NotNil(t, status.Evidence.ActiveProbe)
			assert.Equal(t, continuityModelStatusUnavailable, status.Evidence.ActiveProbe.Status)
			require.NotNil(t, status.Evidence.Passive)
		})
	}
}

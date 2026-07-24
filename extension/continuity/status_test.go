package continuity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"

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
					Evidence             struct {
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

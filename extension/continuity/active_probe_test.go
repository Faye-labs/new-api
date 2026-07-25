package continuity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func installContinuityProbeTestDoubles(
	t *testing.T,
	probe func(context.Context, *model.Channel, string, string) controller.ChannelProbeResult,
	wait func(context.Context, time.Duration) error,
) {
	t.Helper()
	originalProbe := runContinuityChannelProbe
	originalWait := waitForContinuityProbe
	originalNow := continuityProbeNow
	runContinuityChannelProbe = probe
	waitForContinuityProbe = wait
	continuityProbeNow = func() time.Time {
		return time.Date(2026, time.July, 24, 17, 30, 0, 0, time.UTC)
	}
	t.Cleanup(func() {
		runContinuityChannelProbe = originalProbe
		waitForContinuityProbe = originalWait
		continuityProbeNow = originalNow
	})
}

func createContinuityProbePair(
	t *testing.T,
	databaseChannels []model.Channel,
	abilities []model.Ability,
) {
	t.Helper()
	state, err := loadContinuityGroupModelProbeExclusionState()
	require.NoError(t, err)
	if !state.Initialized {
		_, err := replaceContinuityGroupModelProbeExclusions(
			[]continuityGroupModelProbeExclusion{},
		)
		require.NoError(t, err)
	}
	require.NoError(t, model.DB.Create(&databaseChannels).Error)
	require.NoError(t, model.DB.Create(&abilities).Error)
}

func TestContinuityGroupModelProbeMakesNoProviderCallsBeforeExclusionsInitialize(t *testing.T) {
	tests := []struct {
		name string
		task *model.SystemTask
	}{
		{
			name: "scheduled",
			task: &model.SystemTask{
				ID:      1,
				Payload: `{}`,
			},
		},
		{
			name: "manual",
			task: &model.SystemTask{
				ID:      1,
				Payload: `{"manual":true}`,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setupContinuityManagedGroupServiceTest(t)
			require.NoError(t, model.DB.Create(&model.Channel{
				Id:     1,
				Name:   "provider",
				Key:    "key-1",
				Status: common.ChannelStatusEnabled,
			}).Error)
			require.NoError(t, model.DB.Create(&model.Ability{
				Group:     "standard",
				Model:     "model-a",
				ChannelId: 1,
				Enabled:   true,
			}).Error)

			probeCount := 0
			installContinuityProbeTestDoubles(t,
				func(
					_ context.Context,
					_ *model.Channel,
					_ string,
					_ string,
				) controller.ChannelProbeResult {
					probeCount++
					return controller.ChannelProbeResult{
						Status: controller.ChannelProbeStatusSucceeded,
					}
				},
				func(_ context.Context, _ time.Duration) error {
					return nil
				},
			)

			result, err := runContinuityGroupModelProbe(
				context.Background(),
				test.task,
				nil,
			)
			require.NoError(t, err)
			assert.Zero(t, probeCount)
			assert.Empty(t, result.Pairs)
			assert.Equal(t, continuityGroupModelProbeSummary{}, result.Summary)
		})
	}
}

func TestContinuityGroupModelProbeStopsAtFirstSuccessAndMarksFallbackDegraded(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	priorityHigh := int64(10)
	priorityLow := int64(5)
	createContinuityProbePair(t,
		[]model.Channel{
			{Id: 1, Name: "first", Key: "key-1", Status: common.ChannelStatusEnabled},
			{Id: 2, Name: "second", Key: "key-2", Status: common.ChannelStatusEnabled},
			{Id: 3, Name: "lower", Key: "key-3", Status: common.ChannelStatusEnabled},
		},
		[]model.Ability{
			{Group: "standard", Model: "model-a", ChannelId: 1, Enabled: true, Priority: &priorityHigh},
			{Group: "standard", Model: "model-a", ChannelId: 2, Enabled: true, Priority: &priorityHigh},
			{Group: "standard", Model: "model-a", ChannelId: 3, Enabled: true, Priority: &priorityLow},
		},
	)

	var probedChannelIDs []int
	installContinuityProbeTestDoubles(t,
		func(_ context.Context, channel *model.Channel, modelID string, groupKey string) controller.ChannelProbeResult {
			assert.Equal(t, "model-a", modelID)
			assert.Equal(t, "standard", groupKey)
			probedChannelIDs = append(probedChannelIDs, channel.Id)
			if channel.Id == 2 {
				return controller.ChannelProbeResult{
					Status:    controller.ChannelProbeStatusSucceeded,
					LatencyMs: 125,
				}
			}
			return controller.ChannelProbeResult{Status: controller.ChannelProbeStatusFailed}
		},
		func(_ context.Context, _ time.Duration) error {
			return assert.AnError
		},
	)

	result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2}, probedChannelIDs)
	require.Len(t, result.Pairs, 1)
	assert.Equal(t, continuityModelStatusDegraded, result.Pairs[0].Status)
	assert.Equal(t, int64(125), result.Pairs[0].LatencyMs)
	assert.Equal(t, 1, result.Pairs[0].NextRotation)
	assert.Equal(t, continuityGroupModelProbeSummary{
		Total:    1,
		Degraded: 1,
	}, result.Summary)
}

func TestContinuityGroupModelProbeSkipsPersistedCompatibilityPairs(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"standard":1}`))
	createContinuityProbePair(t,
		[]model.Channel{{
			Id:     1,
			Name:   "shared",
			Key:    "key-1",
			Status: common.ChannelStatusEnabled,
		}},
		[]model.Ability{
			{Group: "standard", Model: "compat-model", ChannelId: 1, Enabled: true},
			{Group: "standard", Model: "public-model", ChannelId: 1, Enabled: true},
		},
	)
	_, err := replaceContinuityGroupModelProbeExclusions(
		[]continuityGroupModelProbeExclusion{{
			GroupKey: "standard",
			ModelID:  "compat-model",
		}},
	)
	require.NoError(t, err)

	var probedModels []string
	installContinuityProbeTestDoubles(t,
		func(_ context.Context, _ *model.Channel, modelID string, _ string) controller.ChannelProbeResult {
			probedModels = append(probedModels, modelID)
			return controller.ChannelProbeResult{
				Status: controller.ChannelProbeStatusSucceeded,
			}
		},
		func(_ context.Context, _ time.Duration) error {
			return nil
		},
	)

	result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"public-model"}, probedModels)
	require.Len(t, result.Pairs, 1)
	assert.Equal(t, "public-model", result.Pairs[0].ModelID)
}

func TestContinuityGroupModelProbeExclusionsMatchExactGroupModelPair(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(
		`{"direct":1,"standard":1}`,
	))
	createContinuityProbePair(t,
		[]model.Channel{
			{
				Id:     1,
				Name:   "direct",
				Key:    "key-1",
				Status: common.ChannelStatusEnabled,
			},
			{
				Id:     2,
				Name:   "standard",
				Key:    "key-2",
				Status: common.ChannelStatusEnabled,
			},
		},
		[]model.Ability{
			{Group: "direct", Model: "shared-model", ChannelId: 1, Enabled: true},
			{Group: "standard", Model: "shared-model", ChannelId: 2, Enabled: true},
		},
	)
	_, err := replaceContinuityGroupModelProbeExclusions(
		[]continuityGroupModelProbeExclusion{{
			GroupKey: "standard",
			ModelID:  "shared-model",
		}},
	)
	require.NoError(t, err)

	var probedGroups []string
	installContinuityProbeTestDoubles(t,
		func(_ context.Context, _ *model.Channel, _ string, groupKey string) controller.ChannelProbeResult {
			probedGroups = append(probedGroups, groupKey)
			return controller.ChannelProbeResult{
				Status: controller.ChannelProbeStatusSucceeded,
			}
		},
		func(_ context.Context, _ time.Duration) error {
			return nil
		},
	)

	result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"direct"}, probedGroups)
	require.Len(t, result.Pairs, 1)
	assert.Equal(t, "direct", result.Pairs[0].GroupKey)
	assert.Equal(t, "shared-model", result.Pairs[0].ModelID)
}

func TestContinuityGroupModelProbeRechecksExclusionsBeforeEveryChannelAttempt(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"standard":1}`))
	createContinuityProbePair(t,
		[]model.Channel{
			{
				Id:     1,
				Name:   "first",
				Key:    "key-1",
				Status: common.ChannelStatusEnabled,
			},
			{
				Id:     2,
				Name:   "second",
				Key:    "key-2",
				Status: common.ChannelStatusEnabled,
			},
		},
		[]model.Ability{
			{Group: "standard", Model: "compat-model", ChannelId: 1, Enabled: true},
			{Group: "standard", Model: "compat-model", ChannelId: 2, Enabled: true},
		},
	)

	probeCount := 0
	installContinuityProbeTestDoubles(t,
		func(_ context.Context, _ *model.Channel, _ string, _ string) controller.ChannelProbeResult {
			probeCount++
			_, err := replaceContinuityGroupModelProbeExclusions(
				[]continuityGroupModelProbeExclusion{{
					GroupKey: "standard",
					ModelID:  "compat-model",
				}},
			)
			require.NoError(t, err)
			return controller.ChannelProbeResult{
				Status: controller.ChannelProbeStatusFailed,
			}
		},
		func(_ context.Context, _ time.Duration) error {
			return nil
		},
	)

	result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, probeCount)
	assert.Empty(t, result.Pairs)
	assert.Equal(t, continuityGroupModelProbeSummary{}, result.Summary)
}

func TestContinuityGroupModelProbeRotatesTopPriorityFirstChoiceAcrossRuns(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	priority := int64(10)
	createContinuityProbePair(t,
		[]model.Channel{
			{Id: 1, Name: "first", Key: "key-1", Status: common.ChannelStatusEnabled},
			{Id: 2, Name: "second", Key: "key-2", Status: common.ChannelStatusEnabled},
		},
		[]model.Ability{
			{Group: "standard", Model: "model-a", ChannelId: 1, Enabled: true, Priority: &priority},
			{Group: "standard", Model: "model-a", ChannelId: 2, Enabled: true, Priority: &priority},
		},
	)
	previousResult := continuityGroupModelProbeResult{
		SchemaVersion: 1,
		Pairs: []continuityGroupModelProbeEvidence{{
			GroupKey:     "standard",
			ModelID:      "model-a",
			Status:       continuityModelStatusOperational,
			CheckedAt:    common.GetTimestamp() - 60,
			NextRotation: 1,
		}},
	}
	previousJSON, err := common.Marshal(previousResult)
	require.NoError(t, err)
	require.NoError(t, model.DB.Create(&model.SystemTask{
		TaskID: "systask_previous_probe",
		Type:   continuityGroupModelProbeTaskType,
		Status: model.SystemTaskStatusSucceeded,
		Result: string(previousJSON),
	}).Error)

	var firstChannelID int
	installContinuityProbeTestDoubles(t,
		func(_ context.Context, channel *model.Channel, _ string, _ string) controller.ChannelProbeResult {
			if firstChannelID == 0 {
				firstChannelID = channel.Id
			}
			return controller.ChannelProbeResult{Status: controller.ChannelProbeStatusSucceeded}
		},
		func(_ context.Context, _ time.Duration) error {
			return nil
		},
	)

	result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, firstChannelID)
	require.Len(t, result.Pairs, 1)
	assert.Equal(t, 0, result.Pairs[0].NextRotation)
}

func TestContinuityGroupModelProbeRequiresSecondFullFailureBeforeUnavailable(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	priority := int64(10)
	createContinuityProbePair(t,
		[]model.Channel{
			{Id: 1, Name: "first", Key: "key-1", Status: common.ChannelStatusEnabled},
			{Id: 2, Name: "second", Key: "key-2", Status: common.ChannelStatusEnabled},
		},
		[]model.Ability{
			{Group: "compress", Model: "model-a", ChannelId: 1, Enabled: true, Priority: &priority},
			{Group: "compress", Model: "model-a", ChannelId: 2, Enabled: true, Priority: &priority},
		},
	)

	probeCount := 0
	waitCount := 0
	installContinuityProbeTestDoubles(t,
		func(_ context.Context, _ *model.Channel, _ string, _ string) controller.ChannelProbeResult {
			probeCount++
			return controller.ChannelProbeResult{Status: controller.ChannelProbeStatusFailed}
		},
		func(_ context.Context, delay time.Duration) error {
			waitCount++
			assert.Equal(t, continuityGroupModelProbeConfirmationDelay, delay)
			return nil
		},
	)

	result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 4, probeCount)
	assert.Equal(t, 1, waitCount)
	require.Len(t, result.Pairs, 1)
	assert.Equal(t, "compress", result.Pairs[0].GroupKey)
	assert.Equal(t, continuityModelStatusUnavailable, result.Pairs[0].Status)
	assert.Equal(t, 1, result.Summary.Unavailable)
}

func TestContinuityGroupModelProbeLeavesUnsupportedPairUnknownWithoutConfirmation(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	priority := int64(10)
	createContinuityProbePair(t,
		[]model.Channel{{
			Id:     1,
			Name:   "unsupported",
			Key:    "key-1",
			Status: common.ChannelStatusEnabled,
		}},
		[]model.Ability{{
			Group: "standard", Model: "model-a", ChannelId: 1, Enabled: true, Priority: &priority,
		}},
	)

	waitCount := 0
	installContinuityProbeTestDoubles(t,
		func(_ context.Context, _ *model.Channel, _ string, _ string) controller.ChannelProbeResult {
			return controller.ChannelProbeResult{Status: controller.ChannelProbeStatusUnsupported}
		},
		func(_ context.Context, _ time.Duration) error {
			waitCount++
			return nil
		},
	)

	result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Zero(t, waitCount)
	require.Len(t, result.Pairs, 1)
	assert.Equal(t, continuityModelStatusUnknown, result.Pairs[0].Status)
	assert.Equal(t, 1, result.Summary.Unknown)
}

func TestContinuityGroupModelProbeDoesNotConfirmMixedDeterministicAndUncertainFailures(t *testing.T) {
	uncertainStatuses := []controller.ChannelProbeStatus{
		controller.ChannelProbeStatusUnsupported,
		controller.ChannelProbeStatusIndeterminate,
	}
	for _, uncertainStatus := range uncertainStatuses {
		t.Run(string(uncertainStatus), func(t *testing.T) {
			setupContinuityManagedGroupServiceTest(t)
			priority := int64(10)
			createContinuityProbePair(t,
				[]model.Channel{
					{Id: 1, Name: "failed", Key: "key-1", Status: common.ChannelStatusEnabled},
					{Id: 2, Name: "uncertain", Key: "key-2", Status: common.ChannelStatusEnabled},
				},
				[]model.Ability{
					{Group: "standard", Model: "model-a", ChannelId: 1, Enabled: true, Priority: &priority},
					{Group: "standard", Model: "model-a", ChannelId: 2, Enabled: true, Priority: &priority},
				},
			)

			probeCount := 0
			waitCount := 0
			installContinuityProbeTestDoubles(t,
				func(_ context.Context, channel *model.Channel, _ string, _ string) controller.ChannelProbeResult {
					probeCount++
					if channel.Id == 1 {
						return controller.ChannelProbeResult{Status: controller.ChannelProbeStatusFailed}
					}
					return controller.ChannelProbeResult{Status: uncertainStatus}
				},
				func(_ context.Context, _ time.Duration) error {
					waitCount++
					return nil
				},
			)

			result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
			require.NoError(t, err)
			assert.Equal(t, 2, probeCount)
			assert.Zero(t, waitCount)
			require.Len(t, result.Pairs, 1)
			assert.Equal(t, continuityModelStatusUnknown, result.Pairs[0].Status)
			assert.Equal(t, 1, result.Summary.Unknown)
		})
	}
}

func TestGroupModelStatusChecksEndpointIsSecretAuthenticatedAndSingleFlight(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	secret := strings.Repeat("p", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(plugin{}).Mount(router)

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(
		unauthorized,
		httptest.NewRequest(http.MethodPost, "/internal/continuity/group-model-status/checks", nil),
	)
	require.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	firstRequest := httptest.NewRequest(
		http.MethodPost,
		"/internal/continuity/group-model-status/checks",
		nil,
	)
	firstRequest.Header.Set("Authorization", "Bearer "+secret)
	firstResponse := httptest.NewRecorder()
	router.ServeHTTP(firstResponse, firstRequest)
	require.Equal(t, http.StatusAccepted, firstResponse.Code)
	assert.Equal(t, "no-store", firstResponse.Header().Get("Cache-Control"))

	var firstBody struct {
		Success bool `json:"success"`
		Data    struct {
			TaskID  string                 `json:"task_id"`
			Status  model.SystemTaskStatus `json:"status"`
			Active  bool                   `json:"active"`
			Created bool                   `json:"created"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(firstResponse.Body.Bytes(), &firstBody))
	assert.True(t, firstBody.Success)
	assert.True(t, firstBody.Data.Created)
	assert.True(t, firstBody.Data.Active)
	assert.Equal(t, model.SystemTaskStatusPending, firstBody.Data.Status)
	require.NotEmpty(t, firstBody.Data.TaskID)

	secondRequest := httptest.NewRequest(
		http.MethodPost,
		"/internal/continuity/group-model-status/checks",
		nil,
	)
	secondRequest.Header.Set("Authorization", "Bearer "+secret)
	secondResponse := httptest.NewRecorder()
	router.ServeHTTP(secondResponse, secondRequest)
	require.Equal(t, http.StatusAccepted, secondResponse.Code)

	var secondBody struct {
		Data struct {
			TaskID  string `json:"task_id"`
			Created bool   `json:"created"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(secondResponse.Body.Bytes(), &secondBody))
	assert.False(t, secondBody.Data.Created)
	assert.Equal(t, firstBody.Data.TaskID, secondBody.Data.TaskID)

	getRequest := httptest.NewRequest(
		http.MethodGet,
		"/internal/continuity/group-model-status/checks",
		nil,
	)
	getRequest.Header.Set("Authorization", "Bearer "+secret)
	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, getRequest)
	require.Equal(t, http.StatusOK, getResponse.Code)
	assert.Contains(t, getResponse.Body.String(), firstBody.Data.TaskID)
}

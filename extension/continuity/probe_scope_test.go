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
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeSharedChannelEvidenceIsScopedToModelAndPass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		models   []string
		fails    bool
		attempts int
		waits    int
	}{
		{name: "same model across groups", models: []string{"model-a"}, attempts: 1},
		{name: "different models remain independent", models: []string{"model-a", "model-b"}, attempts: 2},
		{name: "confirmation sends a fresh request once", models: []string{"model-a"}, fails: true, attempts: 2, waits: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupContinuityManagedGroupServiceTest(t)
			var abilities []model.Ability
			for _, group := range []string{"turbo2.5", "turbo-1"} {
				for _, name := range tc.models {
					abilities = append(abilities, model.Ability{Group: group, Model: name, ChannelId: 1, Enabled: true})
				}
			}
			createContinuityProbePair(t, []model.Channel{{Id: 1, Name: "shared", Key: "test-key", Status: common.ChannelStatusEnabled}}, abilities)
			calls, waits := 0, 0
			installContinuityProbeTestDoubles(t, func(_ context.Context, _ *model.Channel, _, _ string) controller.ChannelProbeResult {
				calls++
				status := controller.ChannelProbeStatusSucceeded
				if tc.fails {
					status = controller.ChannelProbeStatusFailed
				}
				return controller.ChannelProbeResult{Status: status, LatencyMs: 123}
			}, func(context.Context, time.Duration) error { waits++; return nil })
			result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.attempts, calls)
			assert.Equal(t, calls, result.Summary.ProviderAttempts)
			assert.Equal(t, tc.waits, waits)
			require.Len(t, result.Pairs, len(abilities))
			for _, evidence := range result.Pairs {
				want := continuityModelStatusOperational
				if tc.fails {
					want = continuityModelStatusUnavailable
				}
				assert.Equal(t, want, evidence.Status)
			}
			// A new task must never borrow synthetic evidence from the preceding task.
			_, err = runContinuityGroupModelProbe(context.Background(), nil, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.attempts*2, calls)
		})
	}
}

func TestProbeSharedFailureKeepsEachGroupsOwnFallback(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	high, low := int64(10), int64(1)
	createContinuityProbePair(t,
		[]model.Channel{
			{Id: 1, Name: "shared-failure", Key: "test-key", Status: common.ChannelStatusEnabled},
			{Id: 2, Name: "fallback-a", Key: "test-key", Status: common.ChannelStatusEnabled},
			{Id: 3, Name: "fallback-b", Key: "test-key", Status: common.ChannelStatusEnabled},
		},
		[]model.Ability{
			{Group: "future-a", Model: "model", ChannelId: 1, Enabled: true, Priority: &high},
			{Group: "future-a", Model: "model", ChannelId: 2, Enabled: true, Priority: &low},
			{Group: "future-b", Model: "model", ChannelId: 1, Enabled: true, Priority: &high},
			{Group: "future-b", Model: "model", ChannelId: 3, Enabled: true, Priority: &low},
		})
	var calls []int
	installContinuityProbeTestDoubles(t, func(_ context.Context, ch *model.Channel, _, _ string) controller.ChannelProbeResult {
		calls = append(calls, ch.Id)
		if ch.Id == 1 {
			return controller.ChannelProbeResult{Status: controller.ChannelProbeStatusFailed}
		}
		return controller.ChannelProbeResult{Status: controller.ChannelProbeStatusSucceeded}
	}, func(context.Context, time.Duration) error {
		t.Fatal("fallback succeeded; must not confirm")
		return nil
	})
	result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, calls)
	assert.Equal(t, 3, result.Summary.ProviderAttempts)
	assert.Equal(t, 2, result.Summary.Degraded)
}

func TestProbeScopeFailsClosedAndRechecksRevocation(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	createContinuityProbePair(t,
		[]model.Channel{{Id: 1, Name: "first", Key: "test-key", Status: common.ChannelStatusEnabled}, {Id: 2, Name: "fallback", Key: "test-key", Status: common.ChannelStatusEnabled}},
		[]model.Ability{{Group: "new-group", Model: "model", ChannelId: 1, Enabled: true}, {Group: "new-group", Model: "model", ChannelId: 2, Enabled: true}})
	_, err := replaceContinuityGroupModelProbeExclusions(nil, nil)
	require.NoError(t, err)
	calls := 0
	installContinuityProbeTestDoubles(t, func(_ context.Context, _ *model.Channel, _, _ string) controller.ChannelProbeResult {
		calls++
		_, err := replaceContinuityGroupModelProbeExclusions(nil, []string{})
		require.NoError(t, err)
		return controller.ChannelProbeResult{Status: controller.ChannelProbeStatusFailed}
	}, func(context.Context, time.Duration) error { t.Fatal("revoked group must not confirm"); return nil })
	result, err := runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Empty(t, result.Pairs)
	assert.Zero(t, calls)
	_, err = replaceContinuityGroupModelProbeExclusions(nil, []string{"new-group"})
	require.NoError(t, err)
	// Legacy exclusions-only writers preserve the synchronized scope.
	_, err = replaceContinuityGroupModelProbeExclusions(nil)
	require.NoError(t, err)
	result, err = runContinuityGroupModelProbe(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Empty(t, result.Pairs)
}

func TestProbeScopeHTTPContractAndAtomicValidation(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	secret := strings.Repeat("s", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	router := gin.New()
	(plugin{}).Mount(router)
	for _, tc := range []struct {
		body   string
		code   int
		groups []string
	}{
		{`{"pairs":[],"enabled_group_keys":["future-group","turbo2.5"],"private":"POISON_CONFIG"}`, http.StatusOK, []string{"future-group", "turbo2.5"}},
		{`{"pairs":[],"enabled_group_keys":["duplicate","duplicate"]}`, http.StatusBadRequest, []string{"future-group", "turbo2.5"}},
		{`{"pairs":[]}`, http.StatusOK, []string{"future-group", "turbo2.5"}},
		{`{"pairs":[],"enabled_group_keys":[]}`, http.StatusOK, []string{}},
	} {
		req := httptest.NewRequest(http.MethodPut, "/internal/continuity/group-model-status/exclusions", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		require.Equal(t, tc.code, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), "POISON_CONFIG")
		state, err := loadContinuityGroupModelProbeExclusionState()
		require.NoError(t, err)
		assert.Equal(t, tc.groups, state.EnabledGroupKeys)
		if tc.code == http.StatusOK {
			var body struct {
				Data struct {
					Groups []string `json:"enabled_group_keys"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, tc.groups, body.Data.Groups)
		}
	}
}

package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestShouldRetryReleasesAffinityOnlyForRetryableCapacityFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	affinity := operation_setting.GetChannelAffinitySetting()
	originalAffinity := *affinity
	originalRetryRanges := append(
		[]operation_setting.StatusCodeRange(nil),
		operation_setting.AutomaticRetryStatusCodeRanges...,
	)
	t.Cleanup(func() {
		*affinity = originalAffinity
		operation_setting.AutomaticRetryStatusCodeRanges = originalRetryRanges
	})

	affinity.Enabled = true
	affinity.DefaultTTLSeconds = 60
	affinity.Rules = []operation_setting.ChannelAffinityRule{{
		Name:       "capacity-retry-test",
		ModelRegex: []string{"^claude-"},
		PathRegex:  []string{"/v1/messages"},
		KeySources: []operation_setting.ChannelAffinityKeySource{{
			Type: "request_header",
			Key:  "X-Test-Affinity",
		}},
		SkipRetryOnFailure: true,
	}}
	operation_setting.AutomaticRetryStatusCodeRanges = []operation_setting.StatusCodeRange{{
		Start: http.StatusTooManyRequests,
		End:   http.StatusTooManyRequests,
	}}

	newAffinityContext := func(value string, wantFound bool) *gin.Context {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		ctx.Request.Header.Set("X-Test-Affinity", value)
		channelID, found := service.GetPreferredChannelByAffinity(
			ctx,
			"claude-opus-test",
			"test-group",
		)
		require.Equal(t, wantFound, found)
		if found {
			service.MarkChannelAffinityUsed(ctx, "test-group", channelID)
		}
		require.True(t, service.ShouldSkipRetryAfterChannelAffinityFailure(ctx))
		return ctx
	}

	capacity := types.NewErrorWithStatusCode(
		errors.New("capacity exhausted"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusTooManyRequests,
	)
	seedContext := newAffinityContext("capacity", false)
	service.RecordChannelAffinity(seedContext, 17)
	capacityContext := newAffinityContext("capacity", true)
	require.True(t, shouldRetry(capacityContext, capacity, 1))
	require.False(t, service.ShouldSkipRetryAfterChannelAffinityFailure(capacityContext))
	newAffinityContext("capacity", false)

	noBudgetContext := newAffinityContext("no-budget", false)
	require.False(t, shouldRetry(noBudgetContext, capacity, 0))
	require.True(t, service.ShouldSkipRetryAfterChannelAffinityFailure(noBudgetContext))

	serverFailure := types.NewErrorWithStatusCode(
		errors.New("server failure"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusInternalServerError,
	)
	serverFailureContext := newAffinityContext("server-failure", false)
	require.False(t, shouldRetry(serverFailureContext, serverFailure, 1))
	require.True(t, service.ShouldSkipRetryAfterChannelAffinityFailure(serverFailureContext))
}

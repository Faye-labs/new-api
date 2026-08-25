package continuity

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/extension"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecentRelaySuccessStoreKeepsNewestExactPairEvidence(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	events := []extension.RelaySuccessEvent{
		{Group: "standard", Model: "model-a", ObservedAt: now.Add(-time.Minute), LatencyMs: 200},
		{Group: "standard", Model: "model-a", ObservedAt: now, LatencyMs: 125},
		{Group: "standard", Model: "model-a", ObservedAt: now.Add(-time.Hour), LatencyMs: 900},
		{Group: "standard", Model: "model-b", ObservedAt: now, LatencyMs: 50},
	}

	var writers sync.WaitGroup
	for _, event := range events {
		event := event
		writers.Add(1)
		go func() {
			defer writers.Done()
			recordContinuityRecentRelaySuccess(event)
		}()
	}
	writers.Wait()

	evidence, ok := latestContinuityRecentRelaySuccess(
		"standard",
		"model-a",
		now.Add(time.Second),
		20*time.Minute,
	)
	require.True(t, ok)
	assert.Equal(t, now.Unix(), evidence.ObservedAt)
	assert.Equal(t, int64(125), evidence.LatencyMs)

	_, ok = latestContinuityRecentRelaySuccess(
		"standard",
		"model-a",
		now.Add(21*time.Minute),
		20*time.Minute,
	)
	assert.False(t, ok)
	_, ok = latestContinuityRecentRelaySuccess(
		"other",
		"model-a",
		now,
		20*time.Minute,
	)
	assert.False(t, ok)
}

func TestRecentRelaySuccessStoreRejectsUnsafeKeysAndStaysBounded(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	originalCapacity := continuityRecentRelaySuccessCapacity
	continuityRecentRelaySuccessCapacity = 2
	t.Cleanup(func() {
		continuityRecentRelaySuccessCapacity = originalCapacity
	})
	now := time.Now().UTC().Truncate(time.Second)

	for _, event := range []extension.RelaySuccessEvent{
		{Group: " standard", Model: "model-a", ObservedAt: now},
		{Group: "standard", Model: "model\na", ObservedAt: now},
		{Group: "standard", Model: strings.Repeat("m", continuityRecentRelaySuccessMaxModelBytes+1), ObservedAt: now},
	} {
		recordContinuityRecentRelaySuccess(event)
	}
	continuityRecentRelaySuccesses.RLock()
	assert.Empty(t, continuityRecentRelaySuccesses.byPair)
	continuityRecentRelaySuccesses.RUnlock()

	for _, modelID := range []string{"model-a", "model-b", "model-c"} {
		recordContinuityRecentRelaySuccess(extension.RelaySuccessEvent{
			Group:      "standard",
			Model:      modelID,
			ObservedAt: now,
		})
	}
	continuityRecentRelaySuccesses.RLock()
	assert.Len(t, continuityRecentRelaySuccesses.byPair, 2)
	_, hasOldest := continuityRecentRelaySuccesses.byPair[continuityGroupModelProbePairKey("standard", "model-a")]
	continuityRecentRelaySuccesses.RUnlock()
	assert.False(t, hasOldest)
}

func TestRecentRelaySuccessStoreClampsLatencyToStatusContract(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, test := range []struct {
		modelID  string
		latency  int64
		expected int64
	}{
		{modelID: "at-limit", latency: continuityStatusMaxLatencyMs, expected: continuityStatusMaxLatencyMs},
		{modelID: "over-limit", latency: continuityStatusMaxLatencyMs + 1, expected: continuityStatusMaxLatencyMs},
	} {
		recordContinuityRecentRelaySuccess(extension.RelaySuccessEvent{
			Group:      "standard",
			Model:      test.modelID,
			ObservedAt: now,
			LatencyMs:  test.latency,
		})

		evidence, ok := latestContinuityRecentRelaySuccess(
			"standard",
			test.modelID,
			now,
			continuityUserTrafficWindow,
		)
		require.True(t, ok)
		assert.Equal(t, test.expected, evidence.LatencyMs)
	}
}

func TestRecentRelayOutcomeLoadsSuccessAndFailureFromPersistence(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, persistContinuityRelayOutcome(extension.RelayOutcomeEvent{
		Group:          "standard",
		Model:          "model-a",
		ObservedAt:     now.Add(-4 * time.Minute),
		LatencyMs:      125,
		Success:        true,
		StatusRelevant: true,
	}))
	require.NoError(t, persistContinuityRelayOutcome(extension.RelayOutcomeEvent{
		Group:          "standard",
		Model:          "model-a",
		ObservedAt:     now.Add(-2 * time.Minute),
		Success:        false,
		StatusRelevant: true,
	}))

	evidence, ok := latestContinuityRecentRelayOutcome(
		"standard",
		"model-a",
		now,
		continuityUserTrafficWindow,
	)
	require.True(t, ok)
	assert.Equal(t, now.Add(-4*time.Minute).Unix(), evidence.LatestSuccessAt)
	assert.Equal(t, int64(125), evidence.LatestSuccessLatencyMs)
	assert.Equal(t, now.Add(-2*time.Minute).Unix(), evidence.LatestFailureAt)
}

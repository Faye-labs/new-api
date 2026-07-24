package model

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cachePopulationResult struct {
	accepted bool
	err      error
}

func TestCachePopulationFenceOrdersMutationAfterStartedPopulation(t *testing.T) {
	revision := currentCachePopulationRevision()
	populationStarted := make(chan struct{})
	releasePopulation := make(chan struct{})
	populationResult := make(chan cachePopulationResult)
	mutationResult := make(chan error)

	var eventsMutex sync.Mutex
	events := make([]string, 0, 2)
	go func() {
		accepted, err := populateCacheAtRevision(revision, func() error {
			close(populationStarted)
			<-releasePopulation
			eventsMutex.Lock()
			events = append(events, "populate")
			eventsMutex.Unlock()
			return nil
		})
		populationResult <- cachePopulationResult{accepted: accepted, err: err}
	}()

	<-populationStarted
	go func() {
		err := invalidateCachesAfterMutation(func() error {
			eventsMutex.Lock()
			events = append(events, "invalidate")
			eventsMutex.Unlock()
			return nil
		})
		mutationResult <- err
	}()

	close(releasePopulation)
	population := <-populationResult
	require.NoError(t, population.err)
	assert.True(t, population.accepted)
	require.NoError(t, <-mutationResult)
	eventsMutex.Lock()
	defer eventsMutex.Unlock()
	require.Equal(t, []string{"populate", "invalidate"}, events)
}

func TestCachePopulationFenceRejectsReadStartedBeforeCompletedMutation(t *testing.T) {
	staleRevision := currentCachePopulationRevision()
	require.NoError(t, invalidateCachesAfterMutation(func() error { return nil }))

	populated := false
	accepted, err := populateCacheAtRevision(staleRevision, func() error {
		populated = true
		return nil
	})

	require.NoError(t, err)
	assert.False(t, accepted)
	assert.False(t, populated)
}

package model

import "sync"

var cachePopulationFence = struct {
	sync.RWMutex
	revision uint64
}{}

func currentCachePopulationRevision() uint64 {
	cachePopulationFence.RLock()
	defer cachePopulationFence.RUnlock()
	return cachePopulationFence.revision
}

func populateCacheAtRevision(revision uint64, populate func() error) (bool, error) {
	cachePopulationFence.RLock()
	defer cachePopulationFence.RUnlock()
	if cachePopulationFence.revision != revision {
		return false, nil
	}
	return true, populate()
}

func invalidateCachesAfterMutation(invalidate func() error) error {
	cachePopulationFence.Lock()
	defer cachePopulationFence.Unlock()
	cachePopulationFence.revision++
	return invalidate()
}

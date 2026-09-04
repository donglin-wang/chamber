package main

import (
	"sync"

	chamberImage "github.com/donglin-wang/chamber/pkg/image"
)

type operationLocks struct {
	mu    sync.Mutex
	locks map[string]*operationLock
}

type operationLock struct {
	mu   sync.Mutex
	refs int
}

var daemonOperationLocks = newOperationLocks()

func canonicalImageOperationLockKey(reference string) (string, string, error) {
	canonical, err := chamberImage.CanonicalImageReference(reference)
	if err != nil {
		return "", "", err
	}
	return "image:" + canonical, canonical, nil
}

func newOperationLocks() *operationLocks {
	return &operationLocks{
		locks: make(map[string]*operationLock),
	}
}

func (locks *operationLocks) with(key string, fn func()) {
	release := locks.acquire(key)
	defer release()
	fn()
}

func (locks *operationLocks) acquire(key string) func() {
	if locks == nil || key == "" {
		return func() {}
	}

	locks.mu.Lock()
	lock := locks.locks[key]
	if lock == nil {
		lock = &operationLock{}
		locks.locks[key] = lock
	}
	lock.refs++
	locks.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()

		locks.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(locks.locks, key)
		}
		locks.mu.Unlock()
	}
}

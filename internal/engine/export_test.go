package engine

import "os"

// SetRecoveryBatchLimitsForTesting configures custom recovery batch limits for testing and returns a restore function.
func SetRecoveryBatchLimitsForTesting(maxRecords int, maxBytes uint64) func() {
	recoveryBatchLimitsMu.Lock()
	prevRecords := recoveryBatchMaxRecords
	prevBytes := recoveryBatchMaxBytes
	recoveryBatchMaxRecords = maxRecords
	recoveryBatchMaxBytes = maxBytes
	recoveryBatchLimitsMu.Unlock()
	return func() {
		recoveryBatchLimitsMu.Lock()
		recoveryBatchMaxRecords = prevRecords
		recoveryBatchMaxBytes = prevBytes
		recoveryBatchLimitsMu.Unlock()
	}
}

// SetRecoveryPrePublishHookForTesting registers a testing hook called right before recovery acquires mu.Lock() to publish.
func SetRecoveryPrePublishHookForTesting(hook func(*Engine)) func() {
	recoveryPrePublishHookMu.Lock()
	recoveryPrePublishHook = hook
	recoveryPrePublishHookMu.Unlock()
	return func() {
		recoveryPrePublishHookMu.Lock()
		recoveryPrePublishHook = nil
		recoveryPrePublishHookMu.Unlock()
	}
}

// SetRecoveryPostPublishHookForTesting registers a testing hook called right after recovery state is published,
// before orphan cleanup and final marking of engineStateRecovered.
// If the hook returns an error, the recovery pipeline triggers full rollback.
func SetRecoveryPostPublishHookForTesting(hook func(*Engine) error) func() {
	recoveryPostPublishHookMu.Lock()
	recoveryPostPublishHook = hook
	recoveryPostPublishHookMu.Unlock()
	return func() {
		recoveryPostPublishHookMu.Lock()
		recoveryPostPublishHook = nil
		recoveryPostPublishHookMu.Unlock()
	}
}

// SetCleanerSyncDirFnForTesting overrides cleanerSyncDirFn for testing directory sync failures.
func SetCleanerSyncDirFnForTesting(fn func(*os.File) error) func() {
	prev := cleanerSyncDirFn
	cleanerSyncDirFn = fn
	return func() {
		cleanerSyncDirFn = prev
	}
}

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

// SetCleanerSyncDirFnForTesting overrides cleanerSyncDirFn for testing directory sync failures.
func SetCleanerSyncDirFnForTesting(fn func(*os.File) error) func() {
	prev := cleanerSyncDirFn
	cleanerSyncDirFn = fn
	return func() {
		cleanerSyncDirFn = prev
	}
}

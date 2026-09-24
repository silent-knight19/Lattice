package transport

import "os"

// SetPolicyPostOpenHookForTesting temporarily attaches a post-open hook to LoadAuthzPolicyFile and returns a restore closure.
// This is strictly a test seam located in export_test.go and is not part of the production API.
func SetPolicyPostOpenHookForTesting(fn func(path string, f *os.File) error) func() {
	policyHookMu.Lock()
	orig := policyPostOpenHook
	policyPostOpenHook = fn
	policyHookMu.Unlock()
	return func() {
		policyHookMu.Lock()
		policyPostOpenHook = orig
		policyHookMu.Unlock()
	}
}

package logger

// ScrubStringForTesting exposes internal scrubString for external test package verification.
func ScrubStringForTesting(s string) string {
	return scrubString(s)
}

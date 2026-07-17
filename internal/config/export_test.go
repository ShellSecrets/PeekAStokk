package config

// SetEtcConfigPathForTest overrides the system-wide config fallback for the
// duration of a test, so tests never touch the real /etc. It returns a
// restore function for use with t.Cleanup.
func SetEtcConfigPathForTest(path string) (restore func()) {
	prev := etcConfigPath
	etcConfigPath = path
	return func() { etcConfigPath = prev }
}

package worker

import "os"

// getenv is a tiny wrapper so we can swap in tests.
func getenv(key string) string { return os.Getenv(key) }

//go:build !corrald_test

package daemon

import "time"

// Production stubs: no test RPCs, no extra environment variable reads.

func defaultPathOptions() PathOptions {
	return PathOptions{}
}

func defaultLifecycleLockTimeout() time.Duration {
	return DefaultLifecycleLockTimeout
}

func applyTestServerConfig(_ *ServerConfig) {}

func registerTestMethods(_ *Server) {}

func shouldFailBeforeReady(_ bool) bool {
	return false
}

func shouldFailCutover() bool {
	return false
}

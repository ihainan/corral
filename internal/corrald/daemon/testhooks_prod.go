//go:build !corrald_test

package daemon

// Production stubs: no test RPCs, no extra environment variable reads.

func defaultPathOptions() PathOptions {
	return PathOptions{}
}

func applyTestServerConfig(_ *ServerConfig) {}

func registerTestMethods(_ *Server) {}

func shouldFailBeforeReady(_ bool) bool {
	return false
}

func shouldFailCutover() bool {
	return false
}

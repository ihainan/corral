//go:build !corrald_test

package daemon

import (
	"fmt"
	"time"
)

// Production stubs: no test RPCs, no extra environment variable reads.

func defaultPathOptions() PathOptions {
	return PathOptions{}
}

func defaultLifecycleLockTimeout() time.Duration {
	return DefaultLifecycleLockTimeout
}

func defaultUninstallSystemRunDir(uid uint32) string {
	return fmt.Sprintf("/run/user/%d", uid)
}

func defaultRunDirLockTimeout() time.Duration {
	return 200 * time.Millisecond
}

func applyTestServerConfig(_ *ServerConfig) {}

func registerTestMethods(_ *Server) {}

func shouldFailBeforeReady(_ bool) bool {
	return false
}

func shouldFailCutover() bool {
	return false
}

func waitCutoverTestGate() {}

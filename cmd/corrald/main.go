// Command corrald is the Corral host helper daemon.
package main

import (
	"os"

	"github.com/ihainan/corral/internal/corrald/agent"
	"github.com/ihainan/corral/internal/corrald/daemon"
	"github.com/ihainan/corral/internal/corrald/proto"
)

var (
	version = "dev"
	release = "0"
	commit  = ""
)

func main() {
	rel, err := proto.ParseRelease(release)
	if err != nil {
		rel = 0
	}
	proto.Version = version
	proto.ReleaseStr = release
	proto.Release = rel
	proto.Commit = commit

	daemon.RegisterModule(agent.Attach)

	os.Exit(daemon.Main(os.Args[1:], proto.BuildInfo{
		Version: version,
		Release: rel,
		Commit:  commit,
	}, os.Stdout, os.Stderr))
}

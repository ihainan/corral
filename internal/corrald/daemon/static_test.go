package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostCodeStaticConstraints verifies spec §1 and §4 static guarantees
// across all production host-side code (cmd/corrald, internal/corrald/proto,
// internal/corrald/daemon, internal/corrald/agent):
//   - No os/exec import or exec.Command / exec.CommandContext calls EXCEPT the
//     single reviewed allowlisted tmux executor in internal/corrald/agent/tmux_exec.go
//   - No cgo ("C") import and no net/http server/client import
//   - No non-unix net.Listen calls (no network port listeners)
//   - No os.Getenv / os.LookupEnv / os.Environ calls outside test-tagged files
//   - os.StartProcess is used only at the two reviewed self-exec sites
//     (serveDetached and dispatchHandoff)
func TestHostCodeStaticConstraints(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dirs := []string{
		filepath.Join(repoRoot, "cmd", "corrald"),
		filepath.Join(repoRoot, "internal", "corrald", "proto"),
		filepath.Join(repoRoot, "internal", "corrald", "daemon"),
		filepath.Join(repoRoot, "internal", "corrald", "agent"),
	}

	allowedStartProcess := map[string]bool{
		"internal/corrald/daemon/cli.go:serveDetached":       true,
		"internal/corrald/daemon/handoff.go:dispatchHandoff": true,
	}
	allowedOSExecImport := map[string]bool{
		"internal/corrald/agent/tmux_exec.go": true,
	}
	allowedExecCommandContext := map[string]bool{
		"internal/corrald/agent/tmux_exec.go:run": true,
	}

	fset := token.NewFileSet()
	checkedFiles := 0
	sawTmuxExec := false

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			fullPath := filepath.Join(dir, e.Name())
			src, err := os.ReadFile(fullPath)
			if err != nil {
				t.Fatal(err)
			}
			// Skip files exclusively compiled under -tags corrald_test.
			if isTestBuildTagged(string(src)) {
				continue
			}
			checkedFiles++

			relPath, _ := filepath.Rel(repoRoot, fullPath)
			relPath = filepath.ToSlash(relPath)
			if relPath == "internal/corrald/agent/tmux_exec.go" {
				sawTmuxExec = true
			}

			file, err := parser.ParseFile(fset, fullPath, src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", relPath, err)
			}

			for _, imp := range file.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				switch p {
				case "os/exec":
					if !allowedOSExecImport[relPath] {
						t.Errorf("%s imports forbidden package %q (only allowlisted tmux executor may import os/exec)", relPath, p)
					}
				case "C", "net/http":
					t.Errorf("%s imports forbidden package %q", relPath, p)
				}
			}

			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				fnName := fn.Name.Name
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					pkgIdent, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					qual := pkgIdent.Name + "." + sel.Sel.Name
					switch qual {
					case "exec.Command":
						t.Errorf("%s:%s calls forbidden %s", relPath, fnName, qual)
					case "exec.CommandContext":
						site := relPath + ":" + fnName
						if !allowedExecCommandContext[site] {
							t.Errorf("unreviewed exec.CommandContext call site: %s", site)
						}
					case "os.Getenv", "os.LookupEnv", "os.Environ":
						t.Errorf("%s:%s reads environment via %s (forbidden in production host code)", relPath, fnName, qual)
					case "net.Listen", "net.ListenTCP", "net.ListenUDP", "net.ListenPacket":
						t.Errorf("%s:%s calls %s (only net.ListenUnix is allowed)", relPath, fnName, qual)
					case "os.StartProcess":
						site := relPath + ":" + fnName
						if !allowedStartProcess[site] {
							t.Errorf("unreviewed os.StartProcess call site: %s", site)
						}
					}
					return true
				})
			}
		}
	}

	if checkedFiles < 15 {
		t.Fatalf("expected to scan at least 15 host production files, scanned %d", checkedFiles)
	}
	if !sawTmuxExec {
		t.Fatal("expected to scan internal/corrald/agent/tmux_exec.go")
	}
}

func isTestBuildTagged(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "package ") {
			return false
		}
		if trim == "//go:build corrald_test" {
			return true
		}
	}
	return false
}

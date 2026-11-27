package proto

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestManifestReadsSyncWithAgentCode verifies that:
//  1. Every proto.Read* capability constant referenced by production code under
//     internal/corrald/agent is declared in DefaultManifest().Reads.
//  2. Every entry in DefaultManifest().Reads is actually referenced in
//     production code (no stale or unused capability declarations).
//  3. DefaultManifest().Reads has no duplicate entries.
//  4. Production files in internal/corrald/agent route filesystem reads through
//     capability-checked entry points rather than calling raw os.ReadFile /
//     os.Stat / os.Readlink / filepath.Glob directly outside capread.go,
//     tmux_exec.go, and tails.go (PathValidator.OpenChecked).
func TestManifestReadsSyncWithAgentCode(t *testing.T) {
	// Parse manifest.go to map Read* constant identifiers -> string values.
	fset := token.NewFileSet()
	manifestFile, err := parser.ParseFile(fset, "manifest.go", nil, 0)
	if err != nil {
		t.Fatalf("parse manifest.go: %v", err)
	}
	readConstVal := map[string]string{}
	for _, decl := range manifestFile.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Read") || i >= len(vs.Values) {
					continue
				}
				bl, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(bl.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				readConstVal[name.Name] = val
			}
		}
	}
	if len(readConstVal) == 0 {
		t.Fatal("no Read* constants found in manifest.go")
	}

	// Scan production Go files in internal/corrald/agent.
	agentDir, err := filepath.Abs(filepath.Join("..", "agent"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatalf("read %s: %v", agentDir, err)
	}

	usedValues := map[string]bool{}
	rawReadExempt := map[string]bool{
		"capread.go":   true,
		"tmux_exec.go": true,
		"tails.go":     true,
	}
	forbiddenRawCalls := map[string]bool{
		"os.ReadFile":           true,
		"os.Open":               true,
		"os.OpenFile":           true,
		"os.Stat":               true,
		"os.Lstat":              true,
		"os.Readlink":           true,
		"os.ReadDir":            true,
		"filepath.Glob":         true,
		"filepath.EvalSymlinks": true,
	}

	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		scanned++
		fullPath := filepath.Join(agentDir, e.Name())
		f, err := parser.ParseFile(fset, fullPath, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := node.X.(*ast.Ident); ok && id.Name == "proto" && strings.HasPrefix(node.Sel.Name, "Read") {
					val, ok := readConstVal[node.Sel.Name]
					if !ok {
						t.Errorf("%s references unknown proto constant proto.%s", e.Name(), node.Sel.Name)
					} else {
						usedValues[val] = true
					}
				}
			case *ast.CallExpr:
				if rawReadExempt[e.Name()] {
					return true
				}
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				qual := id.Name + "." + sel.Sel.Name
				if forbiddenRawCalls[qual] {
					t.Errorf("%s calls %s directly instead of using a capability-declared reader", e.Name(), qual)
				}
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("expected to scan at least 1 production file in internal/corrald/agent")
	}

	manifest := DefaultManifest()
	declared := map[string]bool{}
	for _, r := range manifest.Reads {
		if declared[r] {
			t.Errorf("duplicate read capability in DefaultManifest().Reads: %q", r)
		}
		declared[r] = true
	}

	// Every used capability must be declared in DefaultManifest().Reads.
	var undeclared []string
	for u := range usedValues {
		if !declared[u] {
			undeclared = append(undeclared, u)
		}
	}
	sort.Strings(undeclared)
	if len(undeclared) > 0 {
		t.Errorf("capabilities used in internal/corrald/agent but missing from DefaultManifest().Reads: %v", undeclared)
	}

	// Every declared capability in DefaultManifest().Reads must be used in code.
	var unused []string
	for d := range declared {
		if !usedValues[d] {
			unused = append(unused, d)
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		t.Errorf("capabilities declared in DefaultManifest().Reads but unused in internal/corrald/agent: %v", unused)
	}
}

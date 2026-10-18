// Command never-public fails if storewarden's non-test Go code could make
// it reachable from outside the tailnet (docs/design.md, Listening; ADR
// 0003). Run it from the repository root: go run ./scripts/never-public.
//
// It reads source code, not types, so it catches the usual ways of opening
// a listener by accident. It is not meant to stop code written to evade it.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	problems, err := findProblems(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "never-public:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "never-public: %d problem(s); storewarden listens only through tsnet.Server.Listen\n", len(problems))
		os.Exit(1)
	}
}

// forbiddenMethods are method names that make storewarden reachable from
// outside the tailnet, whatever value they are called on. Matching on the
// name alone needs no type information, at the cost of flagging any other
// method that happens to share a name.
var forbiddenMethods = map[string]string{
	"ListenFunnel":      "tsnet's Funnel listener is reachable from the internet",
	"SetServeConfig":    "serve config can turn on Funnel or proxy to a host port",
	"ListenAndServe":    "http.Server.ListenAndServe opens a listener on the host",
	"ListenAndServeTLS": "http.Server.ListenAndServeTLS opens a listener on the host",
}

// hostListeners lists, by import path, the standard library names that open
// a listener on the host instead of inside tsnet.
var hostListeners = map[string][]string{
	"net": {
		"Listen", "ListenConfig", "ListenIP", "ListenMulticastUDP", "ListenPacket",
		"ListenTCP", "ListenUDP", "ListenUnix", "ListenUnixgram", "FileListener",
	},
	"net/http":          {"ListenAndServe", "ListenAndServeTLS"},
	"net/http/httptest": {"NewServer", "NewTLSServer", "NewUnstartedServer"},
	"crypto/tls":        {"Listen"},
}

// findProblems parses every non-test Go file under root and returns one
// "path:line: message" line for each problem, with paths relative to root.
func findProblems(root string) ([]string, error) {
	var problems []string
	// Per directory (one Go package): where tsnet is first imported, and
	// whether any file checks for the peer API server.
	tsnetImport := make(map[string]string)
	checksPeerAPI := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip what the go command skips: testdata, and directories
			// starting with "." or "_" (which also covers .git and
			// .claude/worktrees). root itself may be ".".
			name := d.Name()
			if path != root && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		report, err := checkFile(path, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		problems = append(problems, report.problems...)
		dir := filepath.Dir(path)
		if report.tsnetImport != "" && tsnetImport[dir] == "" {
			tsnetImport[dir] = report.tsnetImport
		}
		if report.checksPeerAPI {
			checksPeerAPI[dir] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, dir := range slices.Sorted(maps.Keys(tsnetImport)) {
		if !checksPeerAPI[dir] {
			problems = append(problems, tsnetImport[dir]+": imports tsnet but never checks buildfeatures.HasPeerAPIServer, "+
				"so a build without -tags ts_omit_peerapiserver would listen on every host interface (ADR 0003)")
		}
	}
	return problems, nil
}

// fileReport is what checkFile finds in one Go file.
type fileReport struct {
	problems      []string // "path:line: message"
	tsnetImport   string   // "path:line" of its tsnet import, or "" if none
	checksPeerAPI bool     // whether it reads buildfeatures.HasPeerAPIServer
}

// checkFile checks one Go file, naming it by rel.
func checkFile(path, rel string) (fileReport, error) {
	var r fileReport
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return r, err
	}
	for _, imp := range file.Imports {
		if imp.Path.Value == `"tailscale.com/tsnet"` {
			r.tsnetImport = fmt.Sprintf("%s:%d", rel, fset.Position(imp.Pos()).Line)
		}
	}
	imports := importsByName(file)
	addProblem := func(n ast.Node, what, why string) {
		line := fset.Position(n.Pos()).Line
		r.problems = append(r.problems, fmt.Sprintf("%s:%d: %s: %s", rel, line, what, why))
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		// pkg.Name, where pkg is one of the file's imports.
		if ident, ok := sel.X.(*ast.Ident); ok {
			if path, ok := imports[ident.Name]; ok {
				if slices.Contains(hostListeners[path], name) {
					addProblem(sel, path+"."+name, "opens a listener on the host")
				}
				if path == "tailscale.com/feature/buildfeatures" && name == "HasPeerAPIServer" {
					r.checksPeerAPI = true
				}
				return true
			}
		}
		// value.Method, on any value.
		if why, found := forbiddenMethods[name]; found {
			addProblem(sel, name, why)
		}
		return true
	})
	return r, nil
}

// importsByName maps the name each import is used by in file to its path,
// so that a renamed import such as stdnet "net" is still recognised.
func importsByName(file *ast.File) map[string]string {
	imports := make(map[string]string)
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imports[name] = path
	}
	return imports
}

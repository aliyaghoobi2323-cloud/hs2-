package censor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modPath = "github.com/hosseintaghipoursori-alt/hs2-tunnel"

// The step-3 §2.2 boundary: the detector feeds nothing. Outside this package
// only write-only hooks, pure classifiers, value types and the status
// renderer may be used — nothing that returns detector state, except the
// status block, and that only in the status writer/CLI.
var allowedUse = map[string]bool{
	"Active": true, "Init": true,
	"NoteDial": true, "NoteAccept": true, "NoteDeath": true, "NotePath": true, "NoteUsers": true, "NotePeerAborts": true, "NotePeerOrphan": true, "NotePeerOldDeath": true, "NotePeerGeneration": true, "NoteOwnHandover": true, "DeathAbort": true,
	"NoteOwnAbort": true, "NoteOwnAbortsReported": true, "NoteOwnRstClose": true, "NoteOwnStallClose": true, // b429/b430/o432 counters (write-only)
	"AddrOf": true, "ParseAddr": true,
	"ConnectStage": true, "HandshakeStage": true, "AcceptStage": true, "DeathOf": true,
	"Death": true, "PathSample": true, "DialStage": true,
	"SideKharej": true, "SideIran": true,
	"FrontTLS": true, "FrontReality": true, "FrontUnknown": true,
	"DialOK": true, "DialCert": true, "DialAuth": true,
	"AcceptOK": true, "AcceptAuthFail": true,
}

// status output: only these files may read the status block.
var statusUse = map[string]map[string]bool{
	"StatusJSON":  {"cmd/hs2/status.go": true},
	"Render":      {"cmd/hs2/status.go": true},
	"PeerLogLine": {"cmd/hs2/status.go": true}, // b429
}

func TestIsolationNoOneReadsDetectorState(t *testing.T) {
	root, _ := filepath.Abs("..")
	fset := token.NewFileSet()
	importers := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "third_party", "lab", ".git", "censor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		name := ""
		for _, im := range f.Imports {
			if v, _ := strconv.Unquote(im.Path.Value); v == modPath+"/censor" {
				name = "censor"
				if im.Name != nil {
					name = im.Name.Name
				}
			}
		}
		if name == "" {
			return nil
		}
		importers++
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		// every censor.X used must be allowed
		ast.Inspect(f, func(n ast.Node) bool {
			se, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := se.X.(*ast.Ident); !ok || id.Name != name {
				return true
			}
			x := se.Sel.Name
			if files, ok := statusUse[x]; ok {
				if !files[rel] {
					t.Errorf("%s: censor.%s (detector status) may only be read by the status writer", rel, x)
				}
				return true
			}
			if !allowedUse[x] {
				t.Errorf("%s: censor.%s is not on the write-only allow list", rel, x)
			}
			return true
		})
		// censor.Active() only as an if condition (to skip collecting args)
		ast.Inspect(f, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if se, ok := ce.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == "Active" {
				if id, ok := se.X.(*ast.Ident); ok && id.Name == name && !isIfCond(f, ce) {
					t.Errorf("%s: censor.Active() used outside an if condition", rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if importers == 0 {
		t.Fatal("no importer found: the walk is broken")
	}
}

func isIfCond(f *ast.File, call *ast.CallExpr) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if is, ok := n.(*ast.IfStmt); ok && is.Cond == call {
			found = true
		}
		return !found
	})
	return found
}

// The hooks return nothing (no value can flow back), and the package imports
// nothing from the module (it cannot reach the LB, dial or close paths).
func TestIsolationHooksReturnNothingAndNoModuleImports(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for fn, f := range pkg.Files {
			for _, im := range f.Imports {
				if v, _ := strconv.Unquote(im.Path.Value); strings.HasPrefix(v, modPath) {
					t.Errorf("%s imports %s: the detector must not reach the tunnel", fn, v)
				}
			}
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || !strings.HasPrefix(fd.Name.Name, "Note") {
					continue
				}
				if fd.Type.Results != nil && len(fd.Type.Results.List) > 0 {
					t.Errorf("hook %s returns a value", fd.Name.Name)
				}
			}
		}
	}
}

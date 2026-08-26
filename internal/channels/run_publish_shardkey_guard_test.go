package channels

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every message the Manager publishes on behalf of a run addresses the run by
// its composite local key, while the answer uses the bare chat id. Without
// ShardKey those hash to different dispatch workers and the answer can overtake
// the reasoning, quick ack, progress or retry notice that preceded it.
//
// Enumerating the publish sites in behavior tests leaves new ones uncovered —
// the invariant is structural, so assert it structurally: any
// bus.OutboundMessage literal that addresses rc.ChatID must also set ShardKey.
func TestRunScopedPublishesSetShardKey(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isOutboundMessageType(lit.Type) {
				return true
			}
			addressesRun := false
			hasShardKey := false
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "ChatID":
					if isSelectorOn(kv.Value, "rc", "ChatID") {
						addressesRun = true
					}
				case "ShardKey":
					hasShardKey = true
				}
			}
			if addressesRun {
				checked++
				if !hasShardKey {
					t.Errorf("%s: bus.OutboundMessage addresses rc.ChatID without ShardKey — "+
						"it will dispatch on a different shard than the run's answer and can be reordered against it",
						fset.Position(lit.Pos()))
				}
			}
			return true
		})
	}

	// A guard that matches nothing is not a guard.
	if checked == 0 {
		t.Fatal("found no run-scoped bus.OutboundMessage literals; the guard no longer matches the code")
	}
}

func isOutboundMessageType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "OutboundMessage" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "bus"
}

func isSelectorOn(expr ast.Expr, recv, field string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != field {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == recv
}

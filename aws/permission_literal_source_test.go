package aws

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// permissionLiteral matches table code (not comments) that names a permission
// error code or message: code that can turn a denial into "no data" outside the
// connection's per-call ignore check, where the ignored-error hook would not see
// it.
var permissionLiteral = regexp.MustCompile(`"(AccessDenied|AccessDeniedException|ForbiddenException|UnauthorizedOperation|UnauthorizedException|AuthorizationError)"|(?i)insufficient privileges|not authorized`)

// permissionLiteralReviewed lists the table files that may name one, with the
// reason. A new file fails the test: review it (return the error, or deniedAs
// with builtWithHook, so the hook reports the denial), then list it here.
var permissionLiteralReviewed = map[string]string{
	"table_aws_kms_key.go": "kmsKeyRotationStatusError: hides the denial only without the hook",
	"table_aws_serverlessapplicationrepository_application.go": "serverlessrepoPolicyError: deniedAs with the hook",
	"table_aws_backup_vault.go":                                "a get IgnoreConfig code: the per-call check reports it",
	"table_aws_bedrock_imported_model.go":                      `"not authorized to invoke this API operation" is the region-not-supported answer`,
	"table_aws_backup_framework.go":                            `"Insufficient privileges" swallowed: not in the catalog (Task #825 follow-up)`,
	"table_aws_backup_report_plan.go":                          `"Insufficient privileges" swallowed: not in the catalog (Task #825 follow-up)`,
}

func TestPermissionLiteralsAreReviewed(t *testing.T) {
	files, err := filepath.Glob("table_*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		// String literals in code only: comments are not parsed, and a column's
		// Description text is not code.
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !permissionLiteral.MatchString(lit.Value) {
				return true
			}
			if len(stack) >= 2 {
				if kv, ok := stack[len(stack)-2].(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Description" {
						return true
					}
				}
			}
			found[f] = true
			if _, ok := permissionLiteralReviewed[f]; !ok {
				t.Errorf("%s names a permission error outside the per-call check; review it for the ignored-error hook: %s",
					fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
	var stale []string
	for f := range permissionLiteralReviewed {
		if !found[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("reviewed files that no longer name one (remove them from the list): %v", stale)
	}
}

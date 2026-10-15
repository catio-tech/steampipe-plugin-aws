//nolint:staticcheck // SA1019: see ignored_error_hook.go.
package aws

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hiddenIgnore matches table code that applies the connection's ignore codes
// itself, outside the SDK's per-call check: with the ignored-error hook, such
// an error would be ignored and never reported (a denied list would read as
// empty). Each match must be guarded by builtWithHook on the same line.
var hiddenIgnore = regexp.MustCompile(`shouldIgnoreErrorPluginDefault\(\)\(|shouldIgnoreErrors\([^)]*\)\(|\.IgnoreErrorCodes\b|\.IgnoreErrorMessages\b`)

func TestNoHiddenIgnoreWithoutTheHookGuard(t *testing.T) {
	files, err := filepath.Glob("table_*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if !hiddenIgnore.MatchString(line) {
				continue
			}
			found++
			// The guard is on the line, or on the next line that tests the error
			// (the codes are read first, then checked).
			guarded := strings.Contains(line, "builtWithHook")
			for j := i + 1; j < len(lines) && j <= i+3 && !guarded; j++ {
				guarded = strings.Contains(lines[j], "builtWithHook")
			}
			if !guarded {
				t.Errorf("%s:%d applies the ignore codes itself without a builtWithHook guard: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if found == 0 {
		t.Error("the pattern found nothing: the test no longer checks the known places")
	}
}

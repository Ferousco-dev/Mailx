package smtpidentity

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The infrastructure identity is independent of transport, signing, DMARC and the
// send path: it can read configuration and DNS, and nothing may depend on it
// except the composition root.
func TestImportBoundaries(t *testing.T) {
	assertNoImports(t, ".", []string{"internal/smtp\"", "internal/delivery", "internal/worker", "internal/dkim", "internal/dmarc",
		"internal/queue", "internal/dispatch", "internal/api", "internal/secretbox"})
	for _, dir := range []string{"../smtp", "../delivery", "../worker", "../dkim", "../dmarc", "../spf", "../dispatch", "../queue", "../retry", "../api", "../bounce", "../outbound"} {
		assertNoImports(t, dir, []string{"internal/smtpidentity"})
	}
}

func assertNoImports(t *testing.T, dir string, forbidden []string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			for _, bad := range forbidden {
				if strings.Contains(imp.Path.Value, bad) {
					t.Errorf("%s imports %s", f, imp.Path.Value)
				}
			}
		}
	}
}

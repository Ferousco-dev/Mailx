package spf

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SPF is domain/infrastructure guidance, not transport or authorization. These
// import boundaries make the separation structural: SPF cannot reach the SMTP
// client, delivery routing, relay credentials, DKIM keys or the send path, and
// none of those can reach SPF.
func TestImportBoundaries(t *testing.T) {
	forbiddenForSPF := []string{"internal/smtp", "internal/delivery", "internal/worker", "internal/dkim", "internal/queue", "internal/dispatch", "internal/secretbox"}
	assertNoImports(t, ".", forbiddenForSPF)
	// The send/authorization/transport packages must not depend on SPF.
	for _, dir := range []string{"../smtp", "../delivery", "../worker", "../dkim", "../dispatch", "../queue", "../retry", "../bounce"} {
		assertNoImports(t, dir, []string{"internal/spf"})
	}
	// The API's send path (email_handler.go) must not reference SPF either.
	src, err := os.ReadFile("../api/email_handler.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(src)), "spf") {
		t.Fatal("the send handler must not consult SPF")
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

package dmarc

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DMARC consumes DKIM and SPF as facts through interfaces; it cannot reach the
// transport, signing keys, queue or send path, and none of those can reach it.
func TestImportBoundaries(t *testing.T) {
	assertNoImports(t, ".", []string{"internal/smtp", "internal/delivery", "internal/worker", "internal/dkim", "internal/spf",
		"internal/queue", "internal/dispatch", "internal/secretbox"})
	for _, dir := range []string{"../smtp", "../delivery", "../worker", "../dkim", "../spf", "../dispatch", "../queue", "../retry", "../bounce"} {
		assertNoImports(t, dir, []string{"internal/dmarc"})
	}
	src, err := os.ReadFile("../api/email_handler.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(src)), "dmarc") {
		t.Fatal("the send handler must not consult DMARC")
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

// RFC 9989: the DNS Tree Walk is the only authority for the Organizational Domain.
// The Public Suffix List, suffix matching and MailX's ownership-domain package must
// not appear anywhere in the DMARC production code.
func TestNoPSLOrSuffixMatchingInDMARCProduction(t *testing.T) {
	assertNoImports(t, ".", []string{"publicsuffix", "internal/domain"})
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"strings.HasSuffix", "EffectiveTLDPlusOne", "PublicSuffix("} {
			// Comments may mention them; code must not use them.
			for _, line := range strings.Split(string(src), "\n") {
				code, _, _ := strings.Cut(line, "//")
				if strings.Contains(code, banned) {
					t.Errorf("%s uses %s", f, banned)
				}
			}
		}
	}
}

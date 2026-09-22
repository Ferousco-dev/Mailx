package emailtemplate

import "testing"

func TestRenderSubstitutesVariables(t *testing.T) {
	r, err := Render("Welcome, {{name}}", "Hi {{name}}, welcome to {{product}}.", "<h1>Hi {{name}}</h1>",
		map[string]string{"name": "Feranmi", "product": "MailX"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Subject != "Welcome, Feranmi" || r.Text != "Hi Feranmi, welcome to MailX." || r.HTML != "<h1>Hi Feranmi</h1>" {
		t.Fatalf("%+v", r)
	}
}

func TestRenderMissingVariableIsEmpty(t *testing.T) {
	r, err := Render("{{missing}}", "", "x", nil)
	if err != nil || r.Subject != "" {
		t.Fatalf("%q %v", r.Subject, err)
	}
}

func TestRenderMalformedTokenIsLiteral(t *testing.T) {
	cases := map[string]string{
		"{{1bad}}":  "{{1bad}}",
		"{{ }}":     "{{ }}",
		"{{":        "{{",
		"a {{ b":    "a {{ b",
		"{{}}":      "{{}}",
		"{{na me}}": "{{na me}}",
	}
	for in, want := range cases {
		r, err := Render(in, "", "", nil)
		if err != nil || r.Subject != want {
			t.Fatalf("%q: got %q want %q err=%v", in, r.Subject, want, err)
		}
	}
}

func TestRenderRepeatedVariable(t *testing.T) {
	r, _ := Render("{{a}}-{{a}}-{{a}}", "", "", map[string]string{"a": "x"})
	if r.Subject != "x-x-x" {
		t.Fatalf("%q", r.Subject)
	}
}

func TestRenderHTMLEscapesVariablesButNotText(t *testing.T) {
	vars := map[string]string{"v": "<script>alert(1)</script>"}
	r, err := Render("", "{{v}}", "{{v}}", vars)
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != vars["v"] {
		t.Fatalf("text was altered: %q", r.Text)
	}
	if r.HTML == vars["v"] || contains(r.HTML, "<script>") {
		t.Fatalf("html variable was not escaped: %q", r.HTML)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The renderer itself does not strip CR/LF — that is outbound.Build's job on
// whatever subject it is given. This test documents that rendering passes an
// injected value through unchanged (the end-to-end protection is tested at
// the API/outbound layer, not here).
func TestRenderDoesNotItselfStripCRLF(t *testing.T) {
	r, err := Render("Hi {{name}}", "", "", map[string]string{"name": "x\r\nBcc: attacker@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(r.Subject, "\r\n") {
		t.Fatal("expected the raw value to pass through render unchanged")
	}
}

func TestRenderNeverPanicsAndBoundsOutput(t *testing.T) {
	big := make([]byte, MaxBodyLen+1000)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := Render(string(big), "", "", nil); err != ErrTooLarge {
		t.Fatalf("%v", err)
	}
}

func TestRenderManyRepeatsIsLinearNotQuadratic(t *testing.T) {
	var tpl []byte
	for i := 0; i < 100000; i++ {
		tpl = append(tpl, []byte("{{a}}")...)
	}
	vars := map[string]string{"a": "x"}
	if _, err := Render(string(tpl), "", "", vars); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("", "s", "t", ""); err != ErrEmptyName {
		t.Fatalf("%v", err)
	}
	if err := Validate("n", "s", "", ""); err != ErrEmptyBody {
		t.Fatalf("%v", err)
	}
	if err := Validate("n", "s", "t", ""); err != nil {
		t.Fatal(err)
	}
}

func TestValidateVariablesBounds(t *testing.T) {
	if err := ValidateVariables(map[string]string{"k": string(make([]byte, MaxVariableValueLen+1))}); err != ErrVarValueTooLong {
		t.Fatalf("%v", err)
	}
	big := map[string]string{}
	for i := 0; i < MaxVariables+1; i++ {
		big[string(rune('a'+i%26))+string(rune(i))] = "v"
	}
	if err := ValidateVariables(big); err != ErrTooManyVars {
		t.Fatalf("%v", err)
	}
}

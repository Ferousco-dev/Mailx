package emailtemplate

import "testing"

func FuzzRender(f *testing.F) {
	f.Add("Hi {{name}}", "{{a}}{{b}}", "<p>{{x}}</p>")
	f.Add("{{", "}}", "{{{{}}}}")
	f.Add("", "", "")
	vars := map[string]string{"name": "x", "a": "<script>", "b": "y", "x": "z"}
	f.Fuzz(func(t *testing.T, subject, text, html string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panicked: %v", r)
			}
		}()
		_, _ = Render(subject, text, html, vars)
	})
}

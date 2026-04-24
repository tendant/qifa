package secrets

import "testing"

func TestRender(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://db\nline2")
	out, err := Render(map[string]string{"APP_ENV": "production"}, []string{"DATABASE_URL"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if got == "" {
		t.Fatal("expected rendered env file")
	}
	if got != "APP_ENV=production\nDATABASE_URL=postgres://db\\nline2\n" &&
		got != "DATABASE_URL=postgres://db\\nline2\nAPP_ENV=production\n" {
		t.Fatalf("unexpected env file: %q", got)
	}
}

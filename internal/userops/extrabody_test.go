package userops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

const testExtraBody = `{"provider":{"only":["deepseek"],"allow_fallbacks":false}}`

// Covers Edit, and List as the read-back of what Edit persisted.
func TestEdit_ExtraBody(t *testing.T) {
	home := setupHome(t)
	seedConfig(t, []*config.Provider{openaiProvider("oai")}, "")
	readToml := func() string {
		b, err := os.ReadFile(filepath.Join(home, ".config", "cc-fleet", "providers.toml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	str := func(s string) *string { return &s }
	for _, c := range []struct {
		name, want, wantErr string // want: extra_body as List shows it after the edit
		req                 EditRequest
	}{
		{"set", testExtraBody, "", EditRequest{ExtraBody: str(testExtraBody)}},
		{"nil keeps it", testExtraBody, "", EditRequest{DefaultModel: str("gpt-4.1")}},
		{"reserved key is rejected", testExtraBody, `must not set "messages"`, EditRequest{ExtraBody: str(`{"messages":[]}`)}},
		{"empty clears it", "", "", EditRequest{ExtraBody: str("")}},
	} {
		c.req.Name = "oai"
		before := readToml()
		res, err := Edit(c.req)
		switch {
		case c.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("%s: Edit error = %v, want it to contain %q", c.name, err, c.wantErr)
			}
			if after := readToml(); after != before {
				t.Fatalf("%s: rejected edit changed providers.toml:\n%s", c.name, after)
			}
		case err != nil:
			t.Fatalf("%s: %v", c.name, err)
		case res.Provider.ExtraBody != c.want:
			t.Fatalf("%s: result extra_body = %q, want %q", c.name, res.Provider.ExtraBody, c.want)
		}
		list, err := List()
		if err != nil {
			t.Fatal(err)
		}
		v := list.Providers[0]
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		// The JSON key must exist exactly when the value is set.
		if v.ExtraBody != c.want || strings.Contains(string(b), `"extra_body"`) != (c.want != "") {
			t.Fatalf("%s: List extra_body = %q, JSON %s; want %q, key present only when set", c.name, v.ExtraBody, b, c.want)
		}
	}
	if b := readToml(); strings.Contains(b, "extra_body") { // the last row cleared it
		t.Fatalf("providers.toml still has an extra_body line after clearing:\n%s", b)
	}
}

func TestExportImport_ExtraBodyRoundTrip(t *testing.T) {
	setupHome(t)
	oai := openaiProvider("oai")
	oai.ExtraBody = testExtraBody
	seedConfig(t, []*config.Provider{oai}, "")

	ex, err := Export(ExportRequest{})
	if err != nil {
		t.Fatal(err)
	}
	seedConfig(t, nil, "") // fresh target
	if _, err := Import(ImportRequest{Bundle: ex.Bundle}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["oai"].ExtraBody; got != testExtraBody {
		t.Fatalf("extra_body after export+import = %q, want %q", got, testExtraBody)
	}
}

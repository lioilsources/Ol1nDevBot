package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExampleAppsYAMLParses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "apps.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := ParseAppsFile(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Apps) == 0 || f.Timezone != "Europe/Prague" || f.Discord.OpsRole != "ops" {
		t.Fatalf("unexpected: %+v", f)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"bad slug":       "apps: [{slug: Story Teller, name: X}]",
		"dup slug":       "apps: [{slug: a, name: A}, {slug: a, name: B}]",
		"dup service":    "apps: [{slug: a, name: A, services: [x]}, {slug: b, name: B, services: [x]}]",
		"bad milestone":  "apps: [{slug: a, name: A, milestones: [downloads_abc]}]",
		"bad repo":       "apps: [{slug: a, name: A, repos: [nope]}]",
		"missing bundle": "apps: [{slug: a, name: A, ios: {asc_app_id: '1'}}]",
	}
	for name, y := range cases {
		if _, err := ParseAppsFile([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRegistryOverridesAndLookups(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "apps.yaml")
	os.WriteFile(p, []byte(`
github: {owner: o, repos: [o/infra]}
apps:
  - {slug: a, name: A, services: [a-api], repos: [o/a, o/infra]}
`), 0o644)
	r := NewRegistry(p, func(context.Context) ([]App, error) {
		return []App{{Slug: "a", Name: "shadowed"}, {Slug: "b", Name: "B", Repos: []string{"o/b"}}}, nil
	})
	if err := r.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a, _ := r.App("a"); a.Name != "A" {
		t.Fatal("yaml must win over override")
	}
	if _, ok := r.App("b"); !ok {
		t.Fatal("override app missing")
	}
	if a, ok := r.AppForService("a-api"); !ok || a.Slug != "a" {
		t.Fatal("service lookup")
	}
	if got := r.Repos(); len(got) != 3 {
		t.Fatalf("repos: %v", got)
	}

	y, err := MarshalAppYAML(App{Slug: "c", Name: "C", IOS: &IOS{BundleID: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalAppYAML(y)
	if err != nil || back.IOS.BundleID != "x" {
		t.Fatalf("roundtrip: %v %+v", err, back)
	}
}

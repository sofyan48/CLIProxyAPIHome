package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelCatalogConfigRoundTrip(t *testing.T) {
	sources := ModelCatalogs{
		Catalog:      filepath.Join(t.TempDir(), "models.json"),
		CodexCatalog: "https://example.com/codex.json",
		DevinCatalog: "http://localhost/devin.json",
	}
	for _, version := range []string{"", "config-version: 8\n"} {
		t.Run(fmt.Sprint(version != ""), func(t *testing.T) {
			data, errMarshal := yaml.Marshal(map[string]any{"models": sources})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			data = append([]byte(version), data...)
			if errValidate := ValidateV8Config(data); errValidate != nil {
				t.Fatal(errValidate)
			}
			var cfg Config
			if errUnmarshal := yaml.Unmarshal(data, &cfg); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if cfg.Models != sources {
				t.Fatalf("loaded sources = %+v, want %+v", cfg.Models, sources)
			}
			saved, errSave := yaml.Marshal(&cfg)
			if errSave != nil {
				t.Fatal(errSave)
			}
			var reloaded Config
			if errReload := yaml.Unmarshal(saved, &reloaded); errReload != nil {
				t.Fatal(errReload)
			}
			if reloaded.Models != sources {
				t.Fatalf("saved sources = %+v, want %+v", reloaded.Models, sources)
			}

			var root map[string]any
			if errRoot := yaml.Unmarshal(data, &root); errRoot != nil {
				t.Fatal(errRoot)
			}
			normalized, errNormalize := NormalizeConfigRoot(root)
			if errNormalize != nil {
				t.Fatal(errNormalize)
			}
			legacy, errLegacy := LegacyConfigRoot(&cfg)
			if errLegacy != nil {
				t.Fatal(errLegacy)
			}
			for name, projection := range map[string]map[string]any{"normalized": normalized, "legacy": legacy} {
				if !reflect.DeepEqual(projection["models"], root["models"]) {
					t.Fatalf("%s models = %#v, want %#v", name, projection["models"], root["models"])
				}
			}
		})
	}
}

func TestModelCatalogConfigValidation(t *testing.T) {
	for _, field := range []string{"catalog", "codex-catalog", "devin-catalog"} {
		for _, source := range []string{"relative.json", "./models.json", "~/models.json", "ftp://example.com/models", "file:///tmp/models.json", "https:///models"} {
			t.Run(field+"/"+source, func(t *testing.T) {
				data := []byte(fmt.Sprintf("config-version: 8\nmodels:\n  %s: %q\n", field, source))
				original := ModelCatalogs{Catalog: "https://example.com/original.json"}
				cfg := Config{Models: original}
				if errUnmarshal := yaml.Unmarshal(data, &cfg); errUnmarshal == nil {
					t.Fatal("accepted invalid source")
				}
				if cfg.Models != original {
					t.Fatal("invalid catalog sources changed config")
				}
				if errValidate := ValidateV8Config(data); errValidate == nil {
					t.Fatal("v8 accepted invalid source")
				}
			})
		}
	}
	for _, raw := range []string{"models: {}", "models: {catalog: ''}", "models: {codex-catalog: 'https://example.com/models.json'}"} {
		data := []byte("config-version: 8\n" + raw)
		if errValidate := ValidateV8Config(data); errValidate != nil {
			t.Fatal(errValidate)
		}
		var cfg Config
		if errUnmarshal := yaml.Unmarshal(data, &cfg); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
	}
}

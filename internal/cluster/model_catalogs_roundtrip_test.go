package cluster

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	appconfig "github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

func TestModelCatalogsImportDatabaseExportRoundTrip(t *testing.T) {
	if appconfig.IsOAuthProviderRoot("models") {
		t.Fatal("models must use the generic config upsert path")
	}
	for _, layout := range []string{"legacy", "v8"} {
		t.Run(layout, func(t *testing.T) {
			ctx := context.Background()
			want := appconfig.ModelCatalogs{
				Catalog:      "https://example.test/models.json?revision=1&format=json",
				CodexCatalog: "http://localhost/codex.json",
				DevinCatalog: filepath.Join(t.TempDir(), "devin models.json"),
			}
			models := map[string]any{"catalog": want.Catalog, "codex-catalog": want.CodexCatalog, "devin-catalog": want.DevinCatalog}
			root := map[string]any{"models": models}
			if layout == "v8" {
				root["config-version"] = 8
			}
			data, errMarshal := yaml.Marshal(root)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			writeFile(t, configPath, string(data))
			dbPath := filepath.Join(t.TempDir(), "home.db")
			db, errOpenSQLite := OpenSQLite(ctx, dbPath)
			if errOpenSQLite != nil {
				t.Fatalf("OpenSQLite: %v", errOpenSQLite)
			}
			sqlDB, errDB := db.DB()
			if errDB != nil {
				t.Fatal(errDB)
			}
			// Only the currently open pool is owned by cleanup.
			t.Cleanup(func() {
				if sqlDB != nil {
					if errClose := sqlDB.Close(); errClose != nil {
						t.Errorf("close sqlite db: %v", errClose)
					}
				}
			})
			if errMigrate := AutoMigrate(db); errMigrate != nil {
				t.Fatal(errMigrate)
			}
			repo := NewRepository(db)
			if _, errImport := ImportLocalState(ctx, ImportOptions{ConfigPath: configPath, Repository: repo}); errImport != nil {
				t.Fatalf("ImportLocalState: %v", errImport)
			}
			snapshot, errSnapshot := repo.LoadConfigSnapshot(ctx)
			if errSnapshot != nil {
				t.Fatal(errSnapshot)
			}
			var persisted map[string]any
			if errUnmarshal := json.Unmarshal(snapshot["models"], &persisted); errUnmarshal != nil {
				t.Fatalf("decode models config row: %v", errUnmarshal)
			}
			if !reflect.DeepEqual(persisted, models) {
				t.Fatalf("persisted models = %#v, want %#v", persisted, models)
			}
			var record ConfigRecord
			if errFirst := db.Where("key = ?", "models").First(&record).Error; errFirst != nil {
				t.Fatalf("read models from config table: %v", errFirst)
			}
			if equal, errEqual := semanticJSONEqual(record.Value, snapshot["models"]); errEqual != nil || !equal {
				t.Fatalf("models row differs from snapshot: %v", errEqual)
			}

			// Reopening the database must not depend on the exchange file or old pool.
			if errRemove := os.Remove(configPath); errRemove != nil {
				t.Fatal(errRemove)
			}
			errClose := sqlDB.Close()
			sqlDB = nil
			if errClose != nil {
				t.Fatalf("close original sqlite db: %v", errClose)
			}
			reopenedDB, errReopen := OpenSQLite(ctx, dbPath)
			if errReopen != nil {
				t.Fatalf("reopen sqlite db: %v", errReopen)
			}
			sqlDB, errDB = reopenedDB.DB()
			if errDB != nil {
				t.Fatal(errDB)
			}
			repo = NewRepository(reopenedDB)
			reopenedSnapshot, errSnapshot := repo.LoadConfigSnapshot(ctx)
			if errSnapshot != nil {
				t.Fatal(errSnapshot)
			}
			if equal, errEqual := semanticJSONEqual(reopenedSnapshot["models"], snapshot["models"]); errEqual != nil || !equal {
				t.Fatalf("models row changed after reopening sqlite: %v", errEqual)
			}
			cfg, _, errLoad := repo.LoadConfigAsRuntimeConfig(ctx)
			if errLoad != nil {
				t.Fatalf("LoadConfigAsRuntimeConfig: %v", errLoad)
			}
			if cfg.Models != want {
				t.Fatalf("runtime models = %+v, want %+v", cfg.Models, want)
			}

			outputDir := t.TempDir()
			if _, errExport := ExportLocalState(ctx, ExportOptions{Repository: repo, OutputDir: outputDir, AuthDirName: "auth"}); errExport != nil {
				t.Fatalf("ExportLocalState: %v", errExport)
			}
			exported, errRead := os.ReadFile(filepath.Join(outputDir, "config.yaml"))
			if errRead != nil {
				t.Fatal(errRead)
			}
			var exportedRoot map[string]any
			if errUnmarshal := yaml.Unmarshal(exported, &exportedRoot); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if !reflect.DeepEqual(exportedRoot["models"], models) {
				t.Fatalf("exported models = %#v, want %#v", exportedRoot["models"], models)
			}
		})
	}
}

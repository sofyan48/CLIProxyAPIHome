package cluster

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// newModelAccessCatalogRepository creates independent channels with ten
// credential bindings per model, without unrelated runtime or billing tables.
func newModelAccessCatalogRepository(tb testing.TB, modelCount int) *Repository {
	tb.Helper()
	db, errOpen := OpenSQLite(context.Background(), filepath.Join(tb.TempDir(), "model-access.db"))
	if errOpen != nil {
		tb.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		tb.Fatal(errDB)
	}
	tb.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			tb.Error(errClose)
		}
	})
	if errMigrate := db.AutoMigrate(&APIKeyRecord{}, &ChannelGroupRecord{}, &ChannelGroupDetailRecord{}, &ModelGroupRecord{}, &ModelGroupDetailRecord{}); errMigrate != nil {
		tb.Fatal(errMigrate)
	}
	if errCreate := db.Create(&ModelGroupRecord{ID: 1, GroupName: "catalog"}).Error; errCreate != nil {
		tb.Fatal(errCreate)
	}
	groups := make([]ChannelGroupRecord, 0, modelCount)
	details := make([]ChannelGroupDetailRecord, 0, modelCount*10)
	models := make([]ModelGroupDetailRecord, 0, modelCount)
	for i := 1; i <= modelCount; i++ {
		channelID := uint(i)
		groups = append(groups, ChannelGroupRecord{ID: channelID, ChannelName: fmt.Sprintf("channel-%d", i)})
		for j := 0; j < 10; j++ {
			details = append(details, ChannelGroupDetailRecord{ChannelGroupID: channelID, AuthID: fmt.Sprintf("auth-%d-%d", i, j)})
		}
		channels, errChannels := modelGroupDetailChannelsJSON([]uint{channelID})
		if errChannels != nil {
			tb.Fatal(errChannels)
		}
		models = append(models, ModelGroupDetailRecord{ModelGroupID: 1, ModelID: fmt.Sprintf("model-%d", i), Channels: channels})
	}
	for _, rows := range []any{&groups, &details, &models} {
		if errCreate := db.CreateInBatches(rows, 100).Error; errCreate != nil {
			tb.Fatal(errCreate)
		}
	}
	if errCreate := db.Create(&APIKeyRecord{APIKey: "catalog-key", Channels: JSONB(`[]`), ModelGroups: JSONB(`[1]`)}).Error; errCreate != nil {
		tb.Fatal(errCreate)
	}
	return NewRepository(db)
}

func TestModelAccessForAPIKeyManyChannels(t *testing.T) {
	const modelCount = 1000
	repo := newModelAccessCatalogRepository(t, modelCount)
	policy, errPolicy := repo.ModelAccessForAPIKey(context.Background(), "catalog-key")
	if errPolicy != nil {
		t.Fatal(errPolicy)
	}
	for i := 1; i <= modelCount; i++ {
		model := fmt.Sprintf("model-%d", i)
		for j := 0; j < 10; j++ {
			authID := fmt.Sprintf("auth-%d-%d", i, j)
			if !policy.Allows(authID, model) {
				t.Fatalf("model %s denied its credential %s", model, authID)
			}
		}
		otherAuth := fmt.Sprintf("auth-%d-0", i%modelCount+1)
		if policy.Allows(otherAuth, model) {
			t.Fatalf("model %s allowed unrelated channel credential %s", model, otherAuth)
		}
	}
	if policy.Allows("auth-1-0", "unknown-model") {
		t.Fatal("model scope allowed an unlisted model")
	}
}

func TestModelAccessForAPIKeyOverlappingChannels(t *testing.T) {
	repo := newModelAccessCatalogRepository(t, 3)
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	duplicates := []ChannelGroupDetailRecord{
		{ChannelGroupID: 1, AuthID: " auth-1-0 "},
		{ChannelGroupID: 1, AuthID: " shared "},
		{ChannelGroupID: 1, AuthID: "   "},
		{ChannelGroupID: 2, AuthID: "shared"},
	}
	if errCreate := db.Create(&duplicates).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errUpdate := db.Model(&ModelGroupDetailRecord{}).Where("model_id = ?", "model-1").Update("channels", JSONB(`[2,1,2]`)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errUpdate := db.Model(&ChannelGroupRecord{}).Where("id = ?", 3).Update("disabled", true).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("base_scope=%v", scoped), func(t *testing.T) {
			if scoped {
				if errUpdate := db.Model(&APIKeyRecord{}).Where("api_key = ?", "catalog-key").Update("channels", JSONB(`[1]`)).Error; errUpdate != nil {
					t.Fatal(errUpdate)
				}
			}
			policy, errPolicy := repo.ModelAccessForAPIKey(context.Background(), "catalog-key")
			if errPolicy != nil {
				t.Fatal(errPolicy)
			}
			for _, test := range []struct {
				auth  string
				model string
				want  bool
			}{
				{auth: "auth-1-0", model: "MODEL-1(high)", want: true},
				{auth: "auth-2-0", model: "model-1", want: !scoped},
				{auth: "auth-2-0", model: "model-2", want: !scoped},
				{auth: "shared", model: "model-1", want: true},
				{auth: "shared", model: "model-2", want: true},
				{auth: "auth-1-0", model: "model-2", want: false},
				{auth: "auth-3-0", model: "model-3", want: false},
				{auth: "   ", model: "model-1", want: false},
			} {
				if got := policy.Allows(test.auth, test.model); got != test.want {
					t.Errorf("Allows(%q, %q) = %v, want %v", test.auth, test.model, got, test.want)
				}
			}
		})
	}
}

func BenchmarkModelAccessForAPIKey(b *testing.B) {
	for _, modelCount := range []int{100, 1000} {
		b.Run(fmt.Sprintf("models=%d/bindings=%d", modelCount, modelCount*10), func(b *testing.B) {
			repo := newModelAccessCatalogRepository(b, modelCount)
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				policy, errPolicy := repo.ModelAccessForAPIKey(ctx, "catalog-key")
				if errPolicy != nil {
					b.Fatal(errPolicy)
				}
				if !policy.Allows("auth-1-0", "model-1") || policy.Allows("auth-2-0", "model-1") {
					b.Fatal("unexpected model access policy")
				}
			}
		})
	}
}

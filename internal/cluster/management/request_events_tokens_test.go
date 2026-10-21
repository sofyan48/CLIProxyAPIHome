package management

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestManagementRecordTokensUseCompleteAccountingTotals(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}()
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "192.0.2.10", 0)

	cases := []struct {
		name, provider, tokens   string
		pending                  bool
		input, output, reasoning float64
		cacheRead, total         float64
	}{
		{name: "claude", provider: "claude", tokens: `"input_tokens":2,"output_tokens":2070,"cache_read_tokens":10000,"cache_creation_tokens":926,"total_tokens":12998`, input: 10928, output: 2070, cacheRead: 10000, total: 12998},
		{name: "gemini", provider: "gemini", tokens: `"input_tokens":61070,"output_tokens":50,"reasoning_tokens":139,"total_tokens":61259`, input: 61070, output: 189, reasoning: 139, total: 61259},
		{name: "inconsistent", provider: "gemini", tokens: `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":999`, input: 100, output: 20, reasoning: 10, total: 999},
		{name: "pending", provider: "gemini", tokens: `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":130`, pending: true, input: 100, output: 20, reasoning: 10, total: 130},
	}
	usageIDs := make(map[string]string, len(cases))
	for index, tc := range cases {
		payload := fmt.Sprintf(`{"timestamp":"2026-06-10T12:00:%02dZ","provider":%q,"model":"display-model","request_id":%q,"endpoint":"/v1/responses","tokens":{%s}}`, index, tc.provider, tc.name, tc.tokens)
		record, errAppend := repo.AppendUsage(ctx, payload, "192.0.2.10")
		if errAppend != nil {
			t.Fatalf("%s: %v", tc.name, errAppend)
		}
		if tc.pending {
			if errUpdate := db.Model(&cluster.UsageRecord{}).Where("id = ?", record.ID).Update("token_accounting_version", 0).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
		}
		usageIDs[tc.name] = fmt.Sprint(record.ID)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-events", handler.ListRequestEvents)
	engine.GET("/request-events/export", handler.ExportRequestEvents)
	engine.GET("/request-events/:id", handler.GetRequestEvent)
	engine.GET("/usage/records", handler.ListUsageRecords)
	engine.GET("/usage/records/:id", handler.GetUsageRecord)
	engine.GET("/usage/export", handler.ExportUsageRecords)

	eventList := listTokensByRequestID(t, engine, "/request-events?limit=10")
	usageList := listTokensByRequestID(t, engine, "/usage/records?limit=10")
	eventExport := exportRowsByRequestID(t, engine, "/request-events/export?format=jsonl")
	usageExport := exportRowsByRequestID(t, engine, "/usage/export?format=jsonl")

	for _, tc := range cases {
		var eventDetail struct {
			Event struct {
				Tokens map[string]any `json:"tokens"`
			} `json:"event"`
		}
		serveRequestEventJSON(t, engine, "/request-events/evt_"+usageIDs[tc.name]+"?include_payload=true&include_logs=true&include_related=true", &eventDetail)
		var usageDetail struct {
			Record struct {
				Tokens map[string]any `json:"tokens"`
			} `json:"record"`
		}
		serveRequestEventJSON(t, engine, "/usage/records/"+usageIDs[tc.name], &usageDetail)
		for source, tokens := range map[string]map[string]any{
			"request-events list":   eventList[tc.name],
			"request-events detail": eventDetail.Event.Tokens,
			"request-events export": eventExport[tc.name],
			"usage records list":    usageList[tc.name],
			"usage records detail":  usageDetail.Record.Tokens,
			"usage export":          usageExport[tc.name],
		} {
			if tokens["input_tokens"] != tc.input || tokens["output_tokens"] != tc.output ||
				tokens["reasoning_tokens"] != tc.reasoning || tokens["cache_read_tokens"] != tc.cacheRead || tokens["total_tokens"] != tc.total {
				t.Fatalf("%s %s tokens = %#v", tc.name, source, tokens)
			}
		}
	}
}

type tokenBreakdownCase struct {
	name, provider, tokens string
	quality                string
	total                  float64
	inputTotal, uncached   float64
	cacheRead, cacheWrite  float64
	outputTotal            float64
	nonReasoning           float64
	reasoning              float64
	unclassified           float64
}

func TestRequestEventsExposeTokenBreakdown(t *testing.T) {
	ctx := context.Background()
	db, errOpen := cluster.OpenSQLite(ctx, filepath.Join(t.TempDir(), "home.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	sqlDB, errDB := db.DB()
	if errDB != nil {
		t.Fatal(errDB)
	}
	defer func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Errorf("close sqlite db: %v", errClose)
		}
	}()
	if errMigrate := cluster.AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "192.0.2.10", 0)

	cases := []tokenBreakdownCase{
		{
			name: "claude", provider: "claude",
			tokens:       `"input_tokens":2,"output_tokens":2070,"cache_read_tokens":10000,"cache_creation_tokens":926,"total_tokens":12998`,
			quality:      "complete",
			total:        12998,
			inputTotal:   10928,
			uncached:     2,
			cacheRead:    10000,
			cacheWrite:   926,
			outputTotal:  2070,
			nonReasoning: 2070,
		},
		{
			name: "inconsistent", provider: "gemini",
			tokens:       `"input_tokens":100,"output_tokens":20,"reasoning_tokens":10,"total_tokens":999`,
			quality:      "inconsistent",
			total:        999,
			unclassified: 999,
		},
	}
	usageIDs := make(map[string]string, len(cases))
	for index, tc := range cases {
		payload := fmt.Sprintf(`{"timestamp":"2026-06-10T12:00:%02dZ","provider":%q,"model":"display-model","request_id":%q,"endpoint":"/v1/responses","tokens":{%s}}`, index, tc.provider, tc.name, tc.tokens)
		record, errAppend := repo.AppendUsage(ctx, payload, "192.0.2.10")
		if errAppend != nil {
			t.Fatalf("%s: %v", tc.name, errAppend)
		}
		usageIDs[tc.name] = fmt.Sprint(record.ID)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/request-events", handler.ListRequestEvents)
	engine.GET("/request-events/:id", handler.GetRequestEvent)

	var list struct {
		Items []struct {
			RequestID      string         `json:"request_id"`
			TokenBreakdown map[string]any `json:"token_breakdown"`
		} `json:"items"`
	}
	serveRequestEventJSON(t, engine, "/request-events?limit=10", &list)
	listBreakdowns := make(map[string]map[string]any, len(list.Items))
	for _, item := range list.Items {
		listBreakdowns[item.RequestID] = item.TokenBreakdown
	}

	for _, tc := range cases {
		var detail struct {
			Event struct {
				TokenBreakdown map[string]any `json:"token_breakdown"`
			} `json:"event"`
		}
		serveRequestEventJSON(t, engine, "/request-events/evt_"+usageIDs[tc.name], &detail)
		for source, breakdown := range map[string]map[string]any{
			"list":   listBreakdowns[tc.name],
			"detail": detail.Event.TokenBreakdown,
		} {
			assertTokenBreakdown(t, tc.name, source, breakdown, tc)
		}
	}
}

func assertTokenBreakdown(t *testing.T, name, source string, breakdown map[string]any, tc tokenBreakdownCase) {
	t.Helper()
	if breakdown == nil {
		t.Fatalf("%s %s token_breakdown missing", name, source)
	}
	input, _ := breakdown["input"].(map[string]any)
	output, _ := breakdown["output"].(map[string]any)
	if input == nil || output == nil {
		t.Fatalf("%s %s token_breakdown = %#v", name, source, breakdown)
	}
	checks := map[string]struct{ got, want any }{
		"schema_version":              {breakdown["schema_version"], float64(2)},
		"quality":                     {breakdown["quality"], tc.quality},
		"total_tokens":                {breakdown["total_tokens"], tc.total},
		"input.total_tokens":          {input["total_tokens"], tc.inputTotal},
		"input.uncached_tokens":       {input["uncached_tokens"], tc.uncached},
		"input.cache_read_tokens":     {input["cache_read_tokens"], tc.cacheRead},
		"input.cache_write_tokens":    {input["cache_write_tokens"], tc.cacheWrite},
		"output.total_tokens":         {output["total_tokens"], tc.outputTotal},
		"output.non_reasoning_tokens": {output["non_reasoning_tokens"], tc.nonReasoning},
		"output.reasoning_tokens":     {output["reasoning_tokens"], tc.reasoning},
		"unclassified_tokens":         {breakdown["unclassified_tokens"], tc.unclassified},
	}
	for field, check := range checks {
		if check.got != check.want {
			t.Errorf("%s %s token_breakdown.%s = %#v, want %#v", name, source, field, check.got, check.want)
		}
	}
}

func listTokensByRequestID(t *testing.T, engine *gin.Engine, path string) map[string]map[string]any {
	t.Helper()
	var list struct {
		Items []struct {
			RequestID string         `json:"request_id"`
			Tokens    map[string]any `json:"tokens"`
		} `json:"items"`
	}
	serveRequestEventJSON(t, engine, path, &list)
	out := make(map[string]map[string]any, len(list.Items))
	for _, item := range list.Items {
		out[item.RequestID] = item.Tokens
	}
	return out
}

func exportRowsByRequestID(t *testing.T, engine *gin.Engine, path string) map[string]map[string]any {
	t.Helper()
	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body=%s", path, resp.Code, resp.Body.String())
	}
	out := map[string]map[string]any{}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		row := map[string]any{}
		if errDecode := json.Unmarshal(scanner.Bytes(), &row); errDecode != nil {
			t.Fatalf("decode %s: %v", path, errDecode)
		}
		requestID, _ := row["request_id"].(string)
		out[requestID] = row
	}
	if errScan := scanner.Err(); errScan != nil {
		t.Fatal(errScan)
	}
	return out
}

func serveRequestEventJSON(t *testing.T, engine *gin.Engine, path string, target any) {
	t.Helper()
	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body=%s", path, resp.Code, resp.Body.String())
	}
	if errDecode := json.Unmarshal(resp.Body.Bytes(), target); errDecode != nil {
		t.Fatalf("decode %s: %v", path, errDecode)
	}
}

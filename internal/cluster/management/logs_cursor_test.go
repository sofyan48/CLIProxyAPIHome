package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

func TestGetLogsV8CursorValidation(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	handler := NewHandler(cluster.NewRepository(db), nil, "", 0)
	engine := gin.New()
	engine.GET("/v8/management/observability/logs", handler.GetLogsV8)
	engine.GET("/v0/management/logs", handler.GetLogs)

	for _, name := range []string{"anchor_id", "before_id", "after_id"} {
		for _, value := range []string{"0", "-1", "abc", "1.5", "9223372036854775808", "18446744073709551615", "18446744073709551616"} {
			resp := httptest.NewRecorder()
			engine.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/v8/management/observability/logs?"+name+"="+value, nil))
			var body struct {
				Error string `json:"error"`
			}
			if errDecode := json.Unmarshal(resp.Body.Bytes(), &body); errDecode != nil {
				t.Fatal(errDecode)
			}
			if resp.Code != http.StatusBadRequest || body.Error != "invalid_"+name {
				t.Fatalf("%s=%s: status %d, body %s", name, value, resp.Code, resp.Body.String())
			}
		}
	}

	for _, name := range []string{"before_id", "after_id"} {
		resp := httptest.NewRecorder()
		engine.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/v8/management/observability/logs?"+name+"=1", nil))
		var body struct {
			Error string `json:"error"`
		}
		if errDecode := json.Unmarshal(resp.Body.Bytes(), &body); errDecode != nil {
			t.Fatal(errDecode)
		}
		if resp.Code != http.StatusConflict || body.Error != "log_cursor_expired" {
			t.Fatalf("missing %s: status %d, body %s", name, resp.Code, resp.Body.String())
		}
	}

	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/v0/management/logs?anchor_id=invalid&before_id=1", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("legacy request: status %d, body %s", resp.Code, resp.Body.String())
	}
	var legacy map[string]json.RawMessage
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &legacy); errDecode != nil {
		t.Fatal(errDecode)
	}
	if _, exists := legacy["anchor_id"]; exists {
		t.Fatal("legacy logs unexpectedly expose cursor pagination")
	}
}

func TestGetLogsV8CursorDatabaseError(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	record := cluster.AppLogRecord{Timestamp: time.Now().UTC(), Line: "cursor"}
	if errCreate := db.Create(&record).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errRegister := db.Callback().Row().Before("gorm:row").Register("test:cursor-read-error", func(tx *gorm.DB) {
		if len(tx.Statement.Selects) == 1 && tx.Statement.Selects[0] == "id, timestamp" {
			tx.AddError(errors.New("injected cursor read failure"))
		}
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	engine := gin.New()
	engine.GET("/logs", NewHandler(cluster.NewRepository(db), nil, "", 0).GetLogsV8)
	resp := httptest.NewRecorder()
	engine.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/logs?before_id=1", nil))
	var body struct {
		Error string `json:"error"`
	}
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &body); errDecode != nil {
		t.Fatal(errDecode)
	}
	if resp.Code != http.StatusInternalServerError || body.Error != "log_load_failed" {
		t.Fatalf("cursor database failure: status %d, body %s", resp.Code, resp.Body.String())
	}
}

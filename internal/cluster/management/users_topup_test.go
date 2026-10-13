package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestUserManagementTopupContract(t *testing.T) {
	handler, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()
	engine.POST("/users/:id/topup/approve", handler.ApproveUserTopup)
	name, otherName, credits := "topup", "no-topup", 10.0
	user, errCreate := handler.repo.CreateUser(t.Context(), cluster.UserUpdate{Username: &name, Credits: &credits})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	other, errOther := handler.repo.CreateUser(t.Context(), cluster.UserUpdate{Username: &otherName})
	if errOther != nil {
		t.Fatal(errOther)
	}
	request, _, errRequest := handler.repo.CreateBillingRechargeRequest(t.Context(), user.ID, 12.5, "audit")
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	decodeUser := func(item map[string]json.RawMessage, wantPending bool) {
		t.Helper()
		raw, present := item["pending_topup"]
		if !present {
			t.Fatal("pending_topup omitted")
		}
		if !wantPending {
			if string(raw) != "null" {
				t.Fatalf("pending=%s", raw)
			}
			return
		}
		var pending cluster.BillingRechargeRequestRecord
		if errDecode := json.Unmarshal(raw, &pending); errDecode != nil {
			t.Fatal(errDecode)
		}
		if pending.ID != request.ID || pending.UserID != user.ID || pending.Amount != 12.5 || pending.Note != "audit" || pending.Status != "pending" || pending.CreatedAt.IsZero() {
			t.Fatalf("pending=%+v", pending)
		}
	}
	response := performUserManagementRequest(t, engine, http.MethodGet, "/users", "")
	var list struct {
		Users []map[string]json.RawMessage `json:"users"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &list); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Code != 200 || len(list.Users) != 2 {
		t.Fatalf("list=%d %s", response.Code, response.Body.String())
	}
	for _, item := range list.Users {
		var id uint
		if errDecode := json.Unmarshal(item["id"], &id); errDecode != nil {
			t.Fatal(errDecode)
		}
		decodeUser(item, id == user.ID)
	}
	response = performUserManagementRequest(t, engine, http.MethodGet, fmt.Sprintf("/users/%d", user.ID), "")
	var detail struct {
		User map[string]json.RawMessage `json:"user"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &detail); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	decodeUser(detail.User, true)
	path := fmt.Sprintf("/users/%d/topup/approve", user.ID)
	for _, body := range []string{"", `{}`, `null`, `[]`, `{"request_id":null}`, `{"request_id":0}`, `{"request_id":-1}`, `{"request_id":1.5}`, `{"request_id":"1"}`, `{"request_id":true}`, `{"request_id":1e309}`, `{"request_id":18446744073709551615}`} {
		response = performUserManagementRequest(t, engine, http.MethodPost, path, body)
		if response.Code != 400 || !strings.Contains(response.Body.String(), "invalid_request_id") {
			t.Fatalf("body=%s response=%d %s", body, response.Code, response.Body.String())
		}
	}
	for _, tc := range []struct{ path, body string }{
		{fmt.Sprintf("/users/%d/topup/approve", other.ID), fmt.Sprintf(`{"request_id":%d}`, request.ID)},
		{path, `{"request_id":999999}`},
		{"/users/999999/topup/approve", fmt.Sprintf(`{"request_id":%d}`, request.ID)},
	} {
		response = performUserManagementRequest(t, engine, http.MethodPost, tc.path, tc.body)
		if response.Code != 404 || !strings.Contains(response.Body.String(), "topup_request_not_found") {
			t.Fatalf("response=%d %s", response.Code, response.Body.String())
		}
	}
	var firstRecordID string
	for i := 0; i < 2; i++ {
		response = performUserManagementRequest(t, engine, http.MethodPost, path, fmt.Sprintf(`{"request_id":%d,"amount":999999,"user_id":%d}`, request.ID, other.ID))
		var approval struct {
			User   map[string]json.RawMessage `json:"user"`
			Record struct {
				ID            string  `json:"id"`
				Amount        float64 `json:"amount"`
				UserID        uint    `json:"user_id"`
				BalanceBefore float64 `json:"balance_before"`
				BalanceAfter  float64 `json:"balance_after"`
				Operator      string  `json:"operator"`
				Note          string  `json:"note"`
			} `json:"record"`
			CurrentBalance float64 `json:"current_balance"`
		}
		if errDecode := json.Unmarshal(response.Body.Bytes(), &approval); errDecode != nil {
			t.Fatal(errDecode)
		}
		if response.Code != 200 || approval.CurrentBalance != 22.5 || approval.Record.Amount != 12.5 || approval.Record.UserID != user.ID || approval.Record.BalanceBefore != 10 || approval.Record.BalanceAfter != 22.5 || approval.Record.Operator != "admin" || approval.Record.Note != "audit" {
			t.Fatalf("approval=%d %s", response.Code, response.Body.String())
		}
		decodeUser(approval.User, false)
		if i == 0 {
			firstRecordID = approval.Record.ID
		} else if approval.Record.ID != firstRecordID {
			t.Fatal("replay created another record")
		}
	}
	ledger, errLedger := handler.repo.ListBillingBalanceRecords(t.Context(), cluster.BillingBalanceQuery{UserID: &user.ID})
	if errLedger != nil || ledger.Total != 1 {
		t.Fatalf("ledger=%+v error=%v", ledger, errLedger)
	}
}

package userapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestUserBillingRechargeAndBalanceRecordsSessionScope(t *testing.T) {
	t.Parallel()
	handler, closeRepo := newUserBillingTestHandler(t)
	defer closeRepo()
	first, second := seedUserBillingCharges(t, handler)
	token := createUserBillingBearerToken(t, handler, first.ID)
	router := gin.New()
	Register(router.Group("/user"), handler)

	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(resp, req)
		want := http.StatusOK
		if method == http.MethodPost {
			want = http.StatusAccepted
		}
		if resp.Code != want {
			t.Fatalf("%s %s: status=%d body=%s", method, path, resp.Code, resp.Body.String())
		}
		return resp
	}
	resp := request(http.MethodPost, fmt.Sprintf("/user/billing/recharge?user_id=%d", second.ID),
		fmt.Sprintf(`{"amount":12.5,"note":" self recharge ","user_id":%d,"operator":"admin","type":"deduct"}`, second.ID))
	var recharge struct {
		Request        cluster.BillingRechargeRequestRecord `json:"request"`
		CurrentBalance float64                              `json:"current_balance"`
	}
	if errDecode := json.Unmarshal(resp.Body.Bytes(), &recharge); errDecode != nil {
		t.Fatal(errDecode)
	}
	if recharge.Request.ID == 0 || recharge.Request.UserID != first.ID || recharge.Request.Status != "pending" || recharge.Request.Note != "self recharge" || recharge.Request.Amount != 12.5 || recharge.CurrentBalance != 99 {
		t.Fatalf("unexpected recharge: %+v", recharge)
	}
	ctx := context.Background()
	ledger, errLedger := handler.repo.ListBillingBalanceRecords(ctx, cluster.BillingBalanceQuery{UserID: &first.ID})
	if errLedger != nil || ledger.Total != 0 {
		t.Fatalf("ledger=%+v error=%v", ledger, errLedger)
	}
	var pending struct {
		Request *cluster.BillingRechargeRequestRecord `json:"request"`
	}
	if errDecode := json.Unmarshal(request(http.MethodGet, fmt.Sprintf("/user/billing/recharge-request?user_id=%d", second.ID), "").Body.Bytes(), &pending); errDecode != nil {
		t.Fatal(errDecode)
	}
	if pending.Request == nil || pending.Request.ID != recharge.Request.ID {
		t.Fatalf("pending=%+v", pending)
	}
	conflict := httptest.NewRecorder()
	conflictReq := httptest.NewRequest(http.MethodPost, "/user/billing/recharge", strings.NewReader(`{"amount":1}`))
	conflictReq.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(conflict, conflictReq)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "topup_pending") {
		t.Fatalf("conflict=%d %s", conflict.Code, conflict.Body.String())
	}
	if _, approved, errApprove := handler.repo.ApproveBillingRechargeRequest(ctx, first.ID, recharge.Request.ID); errApprove != nil || approved.BalanceBefore != 99 || approved.BalanceAfter != 111.5 {
		t.Fatalf("approval=%+v error=%v", approved, errApprove)
	}
	ledger, errLedger = handler.repo.ListBillingBalanceRecords(ctx, cluster.BillingBalanceQuery{UserID: &first.ID})
	if errLedger != nil || ledger.Total != 1 || ledger.Records[0].Operator != "admin" || ledger.Records[0].Note != "self recharge" {
		t.Fatalf("ledger=%+v error=%v", ledger, errLedger)
	}
	if errDecode := json.Unmarshal(request(http.MethodGet, "/user/billing/recharge-request", "").Body.Bytes(), &pending); errDecode != nil {
		t.Fatal(errDecode)
	}
	if pending.Request != nil {
		t.Fatal("approved request still pending")
	}
	other, errOther := handler.repo.GetUser(ctx, second.ID)
	if errOther != nil || other.Credits != 99 {
		t.Fatalf("other user=%+v error=%v", other, errOther)
	}
	if _, errSeed := handler.repo.ApplyBillingBalanceRecord(ctx, cluster.BillingBalanceUpdate{UserID: second.ID, Type: cluster.BillingBalanceTypeRecharge, Amount: 90, Note: "private admin note"}); errSeed != nil {
		t.Fatal(errSeed)
	}
	next := request(http.MethodPost, "/user/billing/recharge", `{"amount":0.25}`)
	if errDecode := json.Unmarshal(next.Body.Bytes(), &recharge); errDecode != nil {
		t.Fatal(errDecode)
	}
	if _, _, errApprove := handler.repo.ApproveBillingRechargeRequest(ctx, first.ID, recharge.Request.ID); errApprove != nil {
		t.Fatal(errApprove)
	}

	for _, offset := range []int{0, 1, 2} {
		respList := request(http.MethodGet, fmt.Sprintf("/user/billing/balance-records?user_id=%d&limit=1&offset=%d", second.ID, offset), "")
		var list struct {
			Items  []map[string]json.RawMessage `json:"items"`
			Total  int64                        `json:"total"`
			Limit  int                          `json:"limit"`
			Offset int                          `json:"offset"`
		}
		if errDecode := json.Unmarshal(respList.Body.Bytes(), &list); errDecode != nil {
			t.Fatal(errDecode)
		}
		wantItems := 1
		if offset == 2 {
			wantItems = 0
		}
		if list.Total != 2 || list.Limit != 1 || list.Offset != offset || len(list.Items) != wantItems {
			t.Fatalf("unexpected page: %+v", list)
		}
		for _, item := range list.Items {
			for _, key := range []string{"user_id", "operator", "note"} {
				if _, exists := item[key]; exists {
					t.Fatalf("unexpected field %s", key)
				}
			}
		}
	}
	respOverview := request(http.MethodGet, fmt.Sprintf("/user/billing/overview?user_id=%d", second.ID), "")

	var envelope struct {
		Overview map[string]json.RawMessage `json:"overview"`
	}
	if errDecode := json.Unmarshal(respOverview.Body.Bytes(), &envelope); errDecode != nil {
		t.Fatal(errDecode)
	}

	for key, want := range map[string]float64{
		"current_balance": 111.75, "total_charge_amount": 1, "total_recharge_amount": 12.75,
		"total_deduct_amount": 0, "request_count": 1, "input_tokens": 1200, "output_tokens": 300,
		"cache_tokens": 0, "today_spend": 1, "month_spend": 1,
	} {
		var got float64
		if errDecode := json.Unmarshal(envelope.Overview[key], &got); errDecode != nil {
			t.Fatalf("%s: %v", key, errDecode)
		}
		if got != want {
			t.Fatalf("%s=%v, want %v", key, got, want)
		}
	}
}

func TestUserBillingRechargeRejectsInvalidAmounts(t *testing.T) {
	t.Parallel()
	handler, closeRepo := newUserBillingTestHandler(t)
	defer closeRepo()
	user, _ := seedUserBillingCharges(t, handler)
	token := createUserBillingBearerToken(t, handler, user.ID)
	for _, body := range []string{
		`{}`, `{"amount":null}`, `{"amount":0}`, `{"amount":-1}`, `{"amount":"1"}`,
		`{"amount":1e309}`, `{"amount":NaN}`, `{"amount":Infinity}`, `{"amount":true}`,
	} {
		t.Run(body, func(t *testing.T) {
			resp := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(resp)
			c.Request = httptest.NewRequest(http.MethodPost, "/user/billing/recharge", strings.NewReader(body))
			c.Request.Header.Set("Authorization", "Bearer "+token)
			handler.RechargeCurrentUserBillingBalance(c)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
		})
	}
	ledger, errLedger := handler.repo.ListBillingBalanceRecords(context.Background(), cluster.BillingBalanceQuery{UserID: &user.ID})
	if errLedger != nil || ledger.Total != 0 {
		t.Fatalf("ledger=%+v error=%v", ledger, errLedger)
	}
	updated, errUser := handler.repo.GetUser(context.Background(), user.ID)
	if errUser != nil || updated.Credits != 99 {
		t.Fatalf("user=%+v error=%v", updated, errUser)
	}
}

func TestUserBillingBalanceRoutesAuthenticationAndValidation(t *testing.T) {
	t.Parallel()
	handler, closeRepo := newUserBillingTestHandler(t)
	defer closeRepo()
	user, _ := seedUserBillingCharges(t, handler)
	token := createUserBillingBearerToken(t, handler, user.ID)
	router := gin.New()
	Register(router.Group("/user"), handler)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/user/billing/balance-records"},
		{http.MethodPost, "/user/billing/recharge"},
		{http.MethodGet, "/user/billing/recharge-request"},
	} {
		for _, auth := range []string{"", "Bearer invalid"} {
			resp := httptest.NewRecorder()
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{"amount":1,"user_id":1}`))
			req.Header.Set("Authorization", auth)
			router.ServeHTTP(resp, req)
			if resp.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status=%d", route.path, resp.Code)
			}
		}
	}
	for _, query := range []string{"limit=0", "limit=abc", "offset=-1", "offset=abc", "from=bad", "to=bad", "from=2026-07-02&to=2026-07-01"} {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/user/billing/balance-records?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", query, resp.Code, resp.Body.String())
		}
	}
}

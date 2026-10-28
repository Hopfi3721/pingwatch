package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hopfi3721/pingwatch/internal/monitor"
	"github.com/Hopfi3721/pingwatch/internal/storage"
)

type testAPI struct {
	handler http.Handler
	store   *storage.SQLiteStore
}

func newTestAPI(t *testing.T) testAPI {
	t.Helper()

	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := storage.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := storage.NewSQLiteStore(db)

	return testAPI{
		handler: New(store, log.New(testWriter{t}, "", 0)).Handler(),
		store:   store,
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

func (api testAPI) request(t *testing.T, method, path, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	rec := httptest.NewRecorder()
	api.handler.ServeHTTP(rec, req)

	return rec
}

func validBody() string {
	return `{"name":"API","url":"https://example.com/healthz"}`
}

func createTarget(t *testing.T, api testAPI) targetView {
	t.Helper()

	rec := api.request(t, http.MethodPost, "/api/targets", validBody(), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}

	var view targetView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatalf("decode target: %v", err)
	}

	return view
}

func saveResult(t *testing.T, api testAPI, targetID int64, status monitor.Status, latencyMS int64, checkedAt time.Time) {
	t.Helper()

	result := &monitor.Result{
		TargetID:  targetID,
		Status:    status,
		LatencyMS: latencyMS,
		CheckedAt: checkedAt,
	}
	if status == monitor.StatusDown {
		result.StatusCode = http.StatusInternalServerError
		result.Error = "unexpected status 500"
	}
	if err := api.store.SaveResult(context.Background(), result); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
}

func TestHealthz(t *testing.T) {
	api := newTestAPI(t)

	rec := api.request(t, http.MethodGet, "/healthz", "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func TestCreateTargetAppliesDefaults(t *testing.T) {
	api := newTestAPI(t)

	rec := api.request(t, http.MethodPost, "/api/targets", validBody(), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}

	var view targetView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if view.ID == 0 {
		t.Fatal("created target has zero ID")
	}
	if view.IntervalSeconds != monitor.DefaultIntervalSeconds {
		t.Errorf("IntervalSeconds = %d, want %d", view.IntervalSeconds, monitor.DefaultIntervalSeconds)
	}
	if view.TimeoutSeconds != monitor.DefaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, want %d", view.TimeoutSeconds, monitor.DefaultTimeoutSeconds)
	}
	if !view.Enabled {
		t.Error("Enabled = false, want true by default")
	}
	if view.Status != monitor.StatusUnknown {
		t.Errorf("Status = %q, want unknown before the first check", view.Status)
	}
	if view.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	if want := "/api/targets/" + itoa(view.ID); rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestCreateTargetValidation(t *testing.T) {
	api := newTestAPI(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "empty name", body: `{"name":"  ","url":"https://example.com"}`},
		{name: "empty url", body: `{"name":"API","url":""}`},
		{name: "no scheme", body: `{"name":"API","url":"example.com"}`},
		{name: "ftp", body: `{"name":"API","url":"ftp://example.com"}`},
		{name: "interval too small", body: `{"name":"API","url":"https://example.com","interval_seconds":1}`},
		{name: "timeout too big", body: `{"name":"API","url":"https://example.com","timeout_seconds":120}`},
		{name: "timeout not less than interval", body: `{"name":"API","url":"https://example.com","interval_seconds":10,"timeout_seconds":10}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := api.request(t, http.MethodPost, "/api/targets", tt.body, "application/json")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}

			var resp errorResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if resp.Error == "" {
				t.Error("error message is empty")
			}
		})
	}
}

func TestCreateTargetBadPayload(t *testing.T) {
	api := newTestAPI(t)

	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
	}{
		{name: "malformed json", body: `{"name":`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"name":"API","url":"https://example.com","bogus":true}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "two objects", body: `{"name":"API","url":"https://example.com"}{"name":"Web"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "wrong content type", body: validBody(), contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{
			name:        "body too large",
			body:        `{"name":"API","url":"https://example.com/` + strings.Repeat("a", maxBodyBytes) + `"}`,
			contentType: "application/json",
			wantStatus:  http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := api.request(t, http.MethodPost, "/api/targets", tt.body, tt.contentType)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestCreateTargetDisabled(t *testing.T) {
	api := newTestAPI(t)

	body := `{"name":"API","url":"https://example.com","enabled":false}`
	rec := api.request(t, http.MethodPost, "/api/targets", body, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}

	var view targetView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Enabled {
		t.Error("Enabled = true, want false")
	}
}

func TestGetTarget(t *testing.T) {
	api := newTestAPI(t)
	created := createTarget(t, api)

	rec := api.request(t, http.MethodGet, "/api/targets/"+itoa(created.ID), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	var view targetView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.ID != created.ID || view.Name != created.Name || view.URL != created.URL {
		t.Errorf("got %+v, want %+v", view, created)
	}
	if view.Status != monitor.StatusUnknown {
		t.Errorf("Status = %q, want unknown", view.Status)
	}
}

func TestGetTargetNotFound(t *testing.T) {
	api := newTestAPI(t)

	rec := api.request(t, http.MethodGet, "/api/targets/999", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestInvalidTargetIDs(t *testing.T) {
	api := newTestAPI(t)

	for _, path := range []string{"/api/targets/abc", "/api/targets/0", "/api/targets/-1"} {
		if rec := api.request(t, http.MethodGet, path, "", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, rec.Code)
		}
		if rec := api.request(t, http.MethodDelete, path, "", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("DELETE %s status = %d, want 400", path, rec.Code)
		}
	}
}

func TestListTargetsIncludesLastResult(t *testing.T) {
	api := newTestAPI(t)
	first := createTarget(t, api)
	second := createTarget(t, api)

	saveResult(t, api, first.ID, monitor.StatusDown, 800, time.Now())

	rec := api.request(t, http.MethodGet, "/api/targets", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	var resp listResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(resp.Items))
	}

	if resp.Items[0].ID != first.ID || resp.Items[0].Status != monitor.StatusDown {
		t.Errorf("first item = %+v, want down status", resp.Items[0])
	}
	if resp.Items[0].LastError == "" || resp.Items[0].LastCheckedAt == nil {
		t.Error("down target view must include last error and check time")
	}
	if resp.Items[1].ID != second.ID || resp.Items[1].Status != monitor.StatusUnknown {
		t.Errorf("second item = %+v, want unknown status", resp.Items[1])
	}
}

func TestListTargetsEmptyIsArray(t *testing.T) {
	api := newTestAPI(t)

	rec := api.request(t, http.MethodGet, "/api/targets", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"items":[]`) {
		t.Errorf("body = %s, want empty array, not null", body)
	}
}

func TestListResults(t *testing.T) {
	api := newTestAPI(t)
	target := createTarget(t, api)

	base := time.Now().UTC().Truncate(time.Millisecond)
	saveResult(t, api, target.ID, monitor.StatusUp, 10, base)
	saveResult(t, api, target.ID, monitor.StatusDown, 300, base.Add(time.Second))
	saveResult(t, api, target.ID, monitor.StatusUp, 20, base.Add(2*time.Second))

	rec := api.request(t, http.MethodGet, "/api/targets/"+itoa(target.ID)+"/results?limit=2", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	var resp resultsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Limit != 2 {
		t.Errorf("Limit = %d, want 2", resp.Limit)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("len(items) = %d, want 2", len(resp.Items))
	}
	if resp.Items[0].Status != monitor.StatusUp || resp.Items[0].LatencyMS != 20 {
		t.Errorf("first item = %+v, want newest result", resp.Items[0])
	}
	if resp.Items[1].Status != monitor.StatusDown {
		t.Errorf("second item = %+v, want down", resp.Items[1])
	}
}

func TestListResultsValidation(t *testing.T) {
	api := newTestAPI(t)
	target := createTarget(t, api)
	path := "/api/targets/" + itoa(target.ID) + "/results"

	for _, query := range []string{"?limit=0", "?limit=501", "?limit=abc", "?limit=-5"} {
		if rec := api.request(t, http.MethodGet, path+query, "", ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", query, rec.Code)
		}
	}

	if rec := api.request(t, http.MethodGet, "/api/targets/999/results", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown target status = %d, want 404", rec.Code)
	}
}

func TestStats(t *testing.T) {
	api := newTestAPI(t)
	target := createTarget(t, api)

	base := time.Now()
	saveResult(t, api, target.ID, monitor.StatusUp, 100, base)
	saveResult(t, api, target.ID, monitor.StatusUp, 200, base.Add(time.Second))
	saveResult(t, api, target.ID, monitor.StatusDown, 300, base.Add(2*time.Second))

	rec := api.request(t, http.MethodGet, "/api/targets/"+itoa(target.ID)+"/stats", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	var stats monitor.Stats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stats.Total != 3 || stats.Up != 2 || stats.Down != 1 {
		t.Errorf("stats = %+v, want total/up/down = 3/2/1", stats)
	}
	if stats.UptimePercent != 66.67 {
		t.Errorf("UptimePercent = %v, want 66.67", stats.UptimePercent)
	}
	if stats.AvgLatencyMS != 200 {
		t.Errorf("AvgLatencyMS = %d, want 200", stats.AvgLatencyMS)
	}

	if rec := api.request(t, http.MethodGet, "/api/targets/999/stats", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown target status = %d, want 404", rec.Code)
	}
}

func TestDeleteTarget(t *testing.T) {
	api := newTestAPI(t)
	target := createTarget(t, api)
	path := "/api/targets/" + itoa(target.ID)
	saveResult(t, api, target.ID, monitor.StatusUp, 10, time.Now())

	if rec := api.request(t, http.MethodDelete, path, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	if rec := api.request(t, http.MethodGet, path, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete status = %d, want 404", rec.Code)
	}
	if rec := api.request(t, http.MethodDelete, path, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("double delete status = %d, want 404", rec.Code)
	}
	if rec := api.request(t, http.MethodGet, path+"/results", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("results after delete status = %d, want 404", rec.Code)
	}
}

func TestDashboard(t *testing.T) {
	api := newTestAPI(t)

	rec := api.request(t, http.MethodGet, "/", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{"pingwatch", "/api/targets", "обновление"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard does not contain %q", want)
		}
	}

	if rec := api.request(t, http.MethodGet, "/unknown", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	api := newTestAPI(t)

	rec := api.request(t, http.MethodPut, "/api/targets", "{}", "application/json")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}

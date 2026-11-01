package octopus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "sk_test_SECRETKEY"

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL+"/v1/", testKey, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c, srv
}

func TestNewClientRejectsEmptyKey(t *testing.T) {
	if _, err := NewClient(DefaultBaseURL, "", nil); err == nil {
		t.Error("expected error")
	}
}

func TestBasicAuthAndReadOnly(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != testKey || p != "" {
			t.Errorf("basic auth = %q,%q,%v", u, p, ok)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/v1/accounts/A-TEST0001/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"number":"A-TEST0001"}`)
	}))
	a, err := c.Account(context.Background(), "A-TEST0001")
	if err != nil || a.Number != "A-TEST0001" {
		t.Fatalf("got %+v, %v", a, err)
	}
}

func TestPagination(t *testing.T) {
	var srvURL string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "":
			fmt.Fprintf(w, `{"count":3,"next":"%s/v1/electricity-meter-points/M/meters/S/consumption/?page=2","results":[
				{"consumption":1,"interval_start":"2026-11-01T00:00:00Z","interval_end":"2026-11-01T00:30:00Z"},
				{"consumption":2,"interval_start":"2026-11-01T00:30:00Z","interval_end":"2026-11-01T01:00:00Z"}]}`, srvURL)
		case "2":
			fmt.Fprint(w, `{"count":3,"next":null,"results":[
				{"consumption":3,"interval_start":"2026-11-01T01:00:00Z","interval_end":"2026-11-01T01:30:00Z"}]}`)
		}
	}))
	srvURL = srv.URL
	rs, err := c.Consumption(context.Background(), Electricity, "M", "S", time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[2].Consumption != 3 {
		t.Errorf("got %+v", rs)
	}
}

func TestRefusesNextLinkToOtherHost(t *testing.T) {
	var evil int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&evil, 1)
	}))
	defer other.Close()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"next":%q,"results":[]}`, other.URL+"/steal")
	}))
	_, err := c.Consumption(context.Background(), Gas, "M", "S", time.Now(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "unexpected host") {
		t.Errorf("err = %v", err)
	}
	if atomic.LoadInt32(&evil) != 0 {
		t.Error("request was sent to the other host")
	}
}

func TestRetries(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []int
		wantCalls int32
		wantErr   bool
		notFound  bool
	}{
		{"429 then ok", []int{429, 200}, 2, false, false},
		{"500 500 ok", []int{500, 503, 200}, 3, false, false},
		{"404 is not retried", []int{404}, 1, true, true},
		{"400 is not retried", []int{400}, 1, true, false},
		{"always 429 gives up", []int{429, 429, 429, 429, 429, 429, 429}, 6, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := int(atomic.AddInt32(&calls, 1)) - 1
				code := tc.statuses[len(tc.statuses)-1]
				if i < len(tc.statuses) {
					code = tc.statuses[i]
				}
				if code == 429 {
					w.Header().Set("Retry-After", "1")
				}
				w.WriteHeader(code)
				if code == 200 {
					fmt.Fprint(w, `{"number":"A-TEST0001"}`)
				}
			}))
			_, err := c.Account(context.Background(), "A-TEST0001")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if got := atomic.LoadInt32(&calls); got != tc.wantCalls {
				t.Errorf("calls = %d, want %d", got, tc.wantCalls)
			}
			if tc.notFound && !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	var slept []time.Duration
	var calls int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	if _, err := c.Account(context.Background(), "A"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept = %v", slept)
	}
}

func TestErrorsNeverContainKey(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", 400)
	}))
	_, err := c.Account(context.Background(), "A")
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Errorf("err = %v", err)
	}
	srv.Close() // connection failure path
	c.retries = 1
	_, err = c.Account(context.Background(), "A")
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Errorf("err = %v", err)
	}
}

func TestProductCode(t *testing.T) {
	tests := []struct {
		tariff  string
		want    string
		wantErr bool
	}{
		{"E-1R-FLUX-IMPORT-23-02-14-A", "FLUX-IMPORT-23-02-14", false},
		{"E-1R-FLUX-EXPORT-23-02-14-A", "FLUX-EXPORT-23-02-14", false},
		{"G-1R-VAR-22-11-01-A", "VAR-22-11-01", false},
		{"E-2R-GO-VAR-22-10-14-A", "", true},
		{"nonsense", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.tariff, func(t *testing.T) {
			got, err := ProductCode(tc.tariff)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("got %q, %v", got, err)
			}
		})
	}
}

func TestUnitRatesPath(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/v1/products/FLUX-IMPORT-23-02-14/electricity-tariffs/E-1R-FLUX-IMPORT-23-02-14-A/standard-unit-rates/"
		if r.URL.Path != want {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"results":[{"value_exc_vat":20,"value_inc_vat":21,"valid_from":"2026-01-01T00:00:00Z","valid_to":null,"payment_method":null}]}`)
	}))
	rs, err := c.UnitRates(context.Background(), "E-1R-FLUX-IMPORT-23-02-14-A", time.Now(), time.Now())
	if err != nil || len(rs) != 1 || rs[0].ValueIncVAT != 21 || rs[0].ValidTo != nil {
		t.Fatalf("got %+v, %v", rs, err)
	}
}

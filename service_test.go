package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JamesWoolfenden/octo-mcp/internal/octopus"
)

// fakeAPI serves fixture responses shaped like the Octopus REST API, using
// fake identifiers. Nothing here talks to the real service.
type fakeAPI struct {
	srv *httptest.Server
	// perSlot is the value of every half-hour reading, by meter point.
	perSlot map[string]float64
	// drop removes readings (by interval start) for a meter point.
	drop map[string]func(time.Time) bool
}

const (
	impMPAN = "1200000000001"
	expMPAN = "1200000000002"
	gasMPRN = "9900000001"
)

var dataStart = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) // GMT already: clocks went back on 25 Oct
var dataEnd = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		perSlot: map[string]float64{impMPAN: 0.5, expMPAN: 0.25},
		drop:    map[string]func(time.Time) bool{},
	}
	account, err := os.ReadFile("testdata/account.json")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/accounts/A-TEST0001/", func(w http.ResponseWriter, r *http.Request) { w.Write(account) })
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v1/")
		switch {
		case strings.Contains(p, "-meter-points/") && strings.HasSuffix(p, "/consumption/"):
			f.consumption(w, r, strings.Split(p, "/")[1])
		case strings.HasSuffix(p, "/standard-unit-rates/"):
			f.unitRates(w, p)
		case strings.HasSuffix(p, "/standing-charges/"):
			f.standing(w, p)
		default:
			http.NotFound(w, r)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) service(t *testing.T, now time.Time) *Service {
	t.Helper()
	c, err := octopus.NewClient(f.srv.URL+"/v1/", "key", f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(c, "A-TEST0001")
	s.now = func() time.Time { return now }
	return s
}

func writePage(w http.ResponseWriter, r *http.Request, results []map[string]any) {
	const size = 100
	page := 1
	fmt.Sscan(r.URL.Query().Get("page"), &page)
	lo, hi := (page-1)*size, page*size
	if lo > len(results) {
		lo = len(results)
	}
	if hi > len(results) {
		hi = len(results)
	}
	var next any
	if hi < len(results) {
		q := r.URL.Query()
		q.Set("page", fmt.Sprint(page+1))
		next = "http://" + r.Host + r.URL.Path + "?" + q.Encode()
	}
	json.NewEncoder(w).Encode(map[string]any{"count": len(results), "next": next, "results": results[lo:hi]})
}

func (f *fakeAPI) consumption(w http.ResponseWriter, r *http.Request, point string) {
	v, ok := f.perSlot[point]
	if !ok { // e.g. the gas meter that isn't reporting
		writePage(w, r, nil)
		return
	}
	from, _ := time.Parse(time.RFC3339, r.URL.Query().Get("period_from"))
	to, _ := time.Parse(time.RFC3339, r.URL.Query().Get("period_to"))
	var rows []map[string]any
	for t := dataStart; t.Before(dataEnd); t = t.Add(30 * time.Minute) {
		if t.Before(from) || !t.Before(to) {
			continue
		}
		if d := f.drop[point]; d != nil && d(t) {
			continue
		}
		rows = append(rows, map[string]any{"consumption": v, "interval_start": t.Format(time.RFC3339), "interval_end": t.Add(30 * time.Minute).Format(time.RFC3339)})
	}
	if r.URL.Query().Get("order_by") == "-period" { // newest first, as LatestReading expects
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
		if len(rows) > 1 {
			rows = rows[:1]
		}
	}
	writePage(w, r, rows)
}

func rate(inc float64, from time.Time, to *time.Time) map[string]any {
	m := map[string]any{"value_inc_vat": inc, "value_exc_vat": inc / 1.05, "valid_from": from.Format(time.RFC3339), "valid_to": nil, "payment_method": nil}
	if to != nil {
		m["valid_to"] = to.Format(time.RFC3339)
	}
	return m
}

func (f *fakeAPI) unitRates(w http.ResponseWriter, p string) {
	var rs []map[string]any
	at := func(day time.Time, h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	switch {
	case strings.Contains(p, "FLUX-IMPORT"):
		// Flux-shaped card, one set per GMT day: off-peak 8p, peak 40p, day 25p.
		for d := dataStart.AddDate(0, 0, -2); d.Before(dataEnd.AddDate(0, 0, 2)); d = d.AddDate(0, 0, 1) {
			bounds := []struct {
				a, b int
				p    float64
			}{{0, 2, 25}, {2, 5, 8}, {5, 16, 25}, {16, 19, 40}, {19, 24, 25}}
			for _, b := range bounds {
				to := at(d, b.b)
				rs = append(rs, rate(b.p, at(d, b.a), &to))
			}
		}
	case strings.Contains(p, "FLUX-EXPORT"):
		rs = append(rs, rate(15, dataStart.AddDate(0, -1, 0), nil))
	default:
		http.Error(&discard{}, "", 404)
	}
	json.NewEncoder(w).Encode(map[string]any{"count": len(rs), "next": nil, "results": rs})
}

type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

func (f *fakeAPI) standing(w http.ResponseWriter, p string) {
	var rs []map[string]any
	if strings.Contains(p, "FLUX-IMPORT") {
		rs = append(rs, rate(50, dataStart.AddDate(0, -1, 0), nil))
	}
	json.NewEncoder(w).Encode(map[string]any{"count": len(rs), "next": nil, "results": rs})
}

func at(h string) time.Time {
	t, err := time.Parse(time.RFC3339, h)
	if err != nil {
		panic(err)
	}
	return t
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.0051 }

func TestGetAccountMasksIdentifiers(t *testing.T) {
	s := newFakeAPI(t).service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	for _, secret := range []string{"A-TEST0001", impMPAN, expMPAN, gasMPRN, "IMPSERIAL01", "EXPSERIAL01", "E6S00000001"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("output leaks %q: %s", secret, b)
		}
	}
	if out.Account != "******0001" {
		t.Errorf("account = %q", out.Account)
	}
	kinds := map[string]string{}
	for _, m := range out.Properties[0].Meters {
		kinds[m.Kind] = m.CurrentTariff
	}
	if kinds["import"] != "E-1R-FLUX-IMPORT-23-02-14-A" || kinds["export"] != "E-1R-FLUX-EXPORT-23-02-14-A" || kinds["gas"] != "G-1R-VAR-22-11-01-A" {
		t.Errorf("current tariffs = %v", kinds)
	}
	if n := len(out.Properties[0].Meters[0].Agreements); n != 2 {
		t.Errorf("import agreements = %d, want 2 (past and current)", n)
	}
}

func TestGetConsumption(t *testing.T) {
	f := newFakeAPI(t)
	s := f.service(t, at("2026-11-02T12:00:00Z"))
	tests := []struct {
		name      string
		meter     string
		from, to  string
		group     string
		wantTotal float64
		wantN     int
		wantState string
	}{
		{"import by day", "import", "2026-10-26", "2026-11-01", "day", 168, 7, "ok"},
		{"export by day", "export", "2026-10-26", "2026-11-01", "day", 84, 7, "ok"},
		{"import by week", "import", "2026-10-26", "2026-11-01", "week", 168, 1, "ok"},
		{"import by month", "import", "2026-10-26", "2026-11-01", "month", 168, 2, "ok"},
		{"half hour", "import", "2026-10-26", "2026-10-26", "half_hour", 24, 48, "ok"},
		{"spans beyond data is partial", "import", "2026-10-26", "2026-11-03", "day", 168, 9, "partial"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := s.GetConsumption(context.Background(), tc.meter, tc.from, tc.to, tc.group, "")
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != tc.wantState || out.TotalKWh == nil || !approx(*out.TotalKWh, tc.wantTotal) || len(out.Buckets) != tc.wantN {
				t.Errorf("status=%s total=%v buckets=%d notes=%v", out.Status, out.TotalKWh, len(out.Buckets), out.Notes)
			}
		})
	}
	if _, err := s.GetConsumption(context.Background(), "import", "2026-10-01", "2026-10-31", "half_hour", ""); err == nil {
		t.Error("expected half_hour range limit error")
	}
	if _, err := s.GetConsumption(context.Background(), "solar", "2026-10-26", "2026-10-27", "day", ""); err == nil {
		t.Error("expected unknown meter error")
	}
}

func TestGasNoDataIsNotZero(t *testing.T) {
	s := newFakeAPI(t).service(t, at("2026-11-02T00:00:00Z"))
	out, err := s.GetConsumption(context.Background(), "gas", "2026-10-26", "2026-11-01", "day", "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no_data" || out.TotalKWh != nil || len(out.Buckets) != 0 {
		t.Errorf("got %+v", out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), `"total_kwh":0`) {
		t.Errorf("gas reported as zero: %s", b)
	}
	if !strings.Contains(strings.Join(out.Notes, " "), "NOT zero") {
		t.Errorf("notes = %v", out.Notes)
	}

	costs, err := s.GetCosts(context.Background(), "gas", "2026-10-26", "2026-11-01", "")
	if err != nil || costs.Gas == nil || costs.Gas.Status != "no_data" || costs.Gas.TotalPounds != 0 || costs.NetCostPounds != nil {
		t.Errorf("costs = %+v, err %v", costs, err)
	}
}

func TestGasM3Conversion(t *testing.T) {
	f := newFakeAPI(t)
	f.perSlot[gasMPRN] = 0.1 // m³ per half hour
	s := f.service(t, at("2026-11-02T00:00:00Z"))
	want := 0.1 * 48 * 1.02264 * 39.5 / 3.6

	tests := []struct {
		unit     string
		wantKWh  float64
		wantNote string
	}{
		{"", want, "SMETS2"}, // E6S serial: auto-detected as m³
		{"m3", want, "gas_unit=m3"},
		{"kwh", 4.8, "gas_unit=kwh"}, // override: values taken as kWh unchanged
	}
	for _, tc := range tests {
		t.Run("unit="+tc.unit, func(t *testing.T) {
			out, err := s.GetConsumption(context.Background(), "gas", "2026-10-26", "2026-10-26", "day", tc.unit)
			if err != nil {
				t.Fatal(err)
			}
			if out.TotalKWh == nil || math.Abs(*out.TotalKWh-tc.wantKWh) > 0.001 {
				t.Errorf("total = %v, want %v", out.TotalKWh, tc.wantKWh)
			}
			if !strings.Contains(strings.Join(out.Notes, " "), tc.wantNote) {
				t.Errorf("notes = %v, want mention of %q", out.Notes, tc.wantNote)
			}
		})
	}
}

func TestGetCosts(t *testing.T) {
	s := newFakeAPI(t).service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetCosts(context.Background(), "", "2026-10-26", "2026-11-01", "")
	if err != nil {
		t.Fatal(err)
	}
	imp, exp := out.Import, out.Export
	if imp == nil || exp == nil {
		t.Fatalf("got %+v", out)
	}
	// Per day: off-peak 3 kWh×8p + peak 3 kWh×40p + day 18 kWh×25p = 594p; ×7 = £41.58.
	if imp.Status != "ok" || !approx(imp.EnergyPounds, 41.58) || !approx(imp.StandingPounds, 3.50) || !approx(imp.TotalPounds, 45.08) || !approx(imp.KWh, 168) {
		t.Errorf("import = %+v", imp)
	}
	// Export: 12 kWh/day × 15p × 7 = £12.60, no standing charge.
	if exp.Status != "ok" || !approx(exp.TotalPounds, 12.60) || exp.StandingPounds != 0 {
		t.Errorf("export = %+v", exp)
	}
	if out.NetCostPounds == nil || !approx(*out.NetCostPounds, 32.48) {
		t.Errorf("net = %v", out.NetCostPounds)
	}
	bands := map[string]BandOut{}
	for _, b := range imp.Bands {
		bands[b.Band] = b
	}
	if !approx(bands["off-peak"].KWh, 21) || !approx(bands["off-peak"].Pounds, 1.68) || !approx(bands["peak"].Pounds, 8.40) || !approx(bands["day"].Pounds, 31.50) {
		t.Errorf("bands = %+v", bands)
	}
	if len(imp.Tariffs) != 1 || imp.Tariffs[0] != "E-1R-FLUX-IMPORT-23-02-14-A" {
		t.Errorf("tariffs = %v (the pre-2024 agreement must not be used)", imp.Tariffs)
	}
}

// A meter that reports zero kWh in every slot (e.g. export overnight) used to
// give a 0/0 band average, and the NaN made the JSON encoding fail.
func TestGetCostsZeroEnergyBandEncodes(t *testing.T) {
	f := newFakeAPI(t)
	f.perSlot[expMPAN] = 0
	s := f.service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetCosts(context.Background(), "export", "2026-10-26", "2026-11-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("result must be JSON-encodable: %v", err)
	}
	for _, b := range out.Export.Bands {
		if b.AvgP != 0 {
			t.Errorf("band %s average = %v, want 0 for no energy", b.Band, b.AvgP)
		}
	}
}

func TestGetCostsMissingReadingsIsPartial(t *testing.T) {
	f := newFakeAPI(t)
	f.drop[impMPAN] = func(t time.Time) bool { return t.Day() == 28 } // lose a whole day
	s := f.service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetCosts(context.Background(), "import", "2026-10-26", "2026-11-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Import.Status != "partial" || out.Import.Readings != 6*48 || out.Import.Expected != 7*48 {
		t.Errorf("import = %+v", out.Import)
	}
	if out.NetCostPounds != nil {
		t.Error("net must only be given when both sides were requested")
	}
}

func TestCheckDataGaps(t *testing.T) {
	f := newFakeAPI(t)
	// Import loses 10:00-12:00 UTC on 28 Oct (4 slots) and a single slot on 29 Oct.
	f.drop[impMPAN] = func(t time.Time) bool {
		if t.Day() == 28 && t.Hour() >= 10 && t.Hour() < 12 {
			return true
		}
		return t.Day() == 29 && t.Hour() == 3 && t.Minute() == 0
	}
	s := f.service(t, at("2026-11-02T00:00:00Z"))
	out, err := s.CheckDataGaps(context.Background(), "", 7, 60)
	if err != nil {
		t.Fatal(err)
	}
	byMeter := map[string]MeterGapsOut{}
	for _, m := range out.Meters {
		byMeter[m.Meter] = m
	}
	imp := byMeter["import"]
	if imp.Status != "gaps" || len(imp.Gaps) != 1 || imp.Gaps[0].Missing != 4 || imp.Gaps[0].Hours != 2 {
		t.Errorf("import = %+v (single missing slot must be ignored below 60 min)", imp)
	}
	if exp := byMeter["export"]; exp.Status != "ok" || len(exp.Gaps) != 0 || exp.Readings != exp.Expected {
		t.Errorf("export = %+v", exp)
	}
	gas := byMeter["gas"]
	if gas.Status != "no_data" || gas.Readings != 0 || len(gas.Gaps) != 1 || gas.Gaps[0].Missing != 7*48 {
		t.Errorf("gas = %+v", gas)
	}
	if !strings.Contains(strings.Join(gas.Notes, " "), "not zero") {
		t.Errorf("gas notes = %v", gas.Notes)
	}

	// min gap of 30 minutes picks up the single slot too.
	out, _ = s.CheckDataGaps(context.Background(), "import", 7, 30)
	if len(out.Meters) != 1 || len(out.Meters[0].Gaps) != 2 {
		t.Errorf("30 min threshold: %+v", out.Meters)
	}
}

func TestCheckDataGapsLimits(t *testing.T) {
	s := newFakeAPI(t).service(t, at("2026-11-02T00:00:00Z"))
	if _, err := s.CheckDataGaps(context.Background(), "", 365, 0); err == nil {
		t.Error("expected days limit error")
	}
	if _, err := s.CheckDataGaps(context.Background(), "solar", 7, 0); err == nil {
		t.Error("expected unknown meter error")
	}
}

func TestGetRatesLabelsFluxBands(t *testing.T) {
	s := newFakeAPI(t).service(t, at("2026-11-02T12:00:00Z"))
	out, err := s.GetRates(context.Background(), "", "import", "2026-11-01", "2026-11-01")
	if err != nil {
		t.Fatal(err)
	}
	if out.Tariff != "E-1R-FLUX-IMPORT-23-02-14-A" {
		t.Errorf("tariff = %s", out.Tariff)
	}
	got := map[string]float64{}
	for _, r := range out.UnitRates {
		got[r.Band] = r.IncVAT
	}
	if got["off-peak"] != 8 || got["peak"] != 40 || got["day"] != 25 {
		t.Errorf("bands = %v", got)
	}
	if len(out.StandingCharges) != 1 || out.StandingCharges[0].IncVAT != 50 {
		t.Errorf("standing = %+v", out.StandingCharges)
	}
}

func TestMask(t *testing.T) {
	tests := map[string]string{"": "", "abc": "***", "A-12345678": "******5678", "2000000000000": "*********0000"}
	for in, want := range tests {
		if got := mask(in); got != want {
			t.Errorf("mask(%q) = %q, want %q", in, got, want)
		}
	}
}

package calc

import (
	"math"
	"testing"
	"time"
)

func ldn(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, London)
}

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseRange(t *testing.T) {
	tests := []struct {
		name, from, to string
		wantStart      time.Time
		wantEnd        time.Time
		wantErr        bool
	}{
		{"dates, to inclusive", "2026-10-26", "2026-11-01", ldn(2026, 10, 26, 0, 0), ldn(2026, 11, 2, 0, 0), false},
		{"single day", "2026-10-26", "2026-10-26", ldn(2026, 10, 26, 0, 0), ldn(2026, 10, 27, 0, 0), false},
		{"BST start is UTC-1", "2026-07-01", "2026-07-01", utc(2026, 6, 30, 23, 0), utc(2026, 7, 1, 23, 0), false},
		{"rfc3339 exclusive", "2026-10-26T00:00:00Z", "2026-10-27T00:00:00Z", utc(2026, 10, 26, 0, 0), utc(2026, 10, 27, 0, 0), false},
		{"end before start", "2026-10-27", "2026-10-26", time.Time{}, time.Time{}, true},
		{"garbage", "yesterday", "2026-10-26", time.Time{}, time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, e, err := ParseRange(tc.from, tc.to)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !s.Equal(tc.wantStart) || !e.Equal(tc.wantEnd) {
				t.Errorf("got %v..%v want %v..%v", s, e, tc.wantStart, tc.wantEnd)
			}
		})
	}
}

func TestFluxBand(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{"GMT off-peak start", utc(2026, 12, 1, 2, 0), "off-peak"},
		{"GMT off-peak last slot", utc(2026, 12, 1, 4, 30), "off-peak"},
		{"GMT day after off-peak", utc(2026, 12, 1, 5, 0), "day"},
		{"GMT peak start", utc(2026, 12, 1, 16, 0), "peak"},
		{"GMT peak end", utc(2026, 12, 1, 19, 0), "day"},
		// In BST local 02:00 is 01:00 UTC: the band must follow local time.
		{"BST off-peak is 01:00 UTC", utc(2026, 7, 1, 1, 0), "off-peak"},
		{"BST 02:00 UTC is 03:00 local", utc(2026, 7, 1, 2, 0), "off-peak"},
		{"BST 00:30 UTC is 01:30 local", utc(2026, 7, 1, 0, 30), "day"},
		{"BST peak is 15:00 UTC", utc(2026, 7, 1, 15, 0), "peak"},
		{"BST 16:00 UTC is 17:00 local peak", utc(2026, 7, 1, 16, 0), "peak"},
		{"BST 18:00 UTC is 19:00 local day", utc(2026, 7, 1, 18, 0), "day"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FluxBand(tc.at); got != tc.want {
				t.Errorf("FluxBand(%v) = %s, want %s", tc.at, got, tc.want)
			}
		})
	}
}

func halfHours(start time.Time, n int, v float64) []Reading {
	rs := make([]Reading, n)
	for i := range rs {
		rs[i] = Reading{Start: start.Add(time.Duration(i) * HalfHour), Value: v}
	}
	return rs
}

func TestGroupDSTDays(t *testing.T) {
	tests := []struct {
		name         string
		day          time.Time // local midnight
		wantExpected int
	}{
		{"normal day", ldn(2026, 6, 10, 0, 0), 48},
		{"spring forward is 23h", ldn(2026, 3, 29, 0, 0), 46},
		{"autumn back is 25h", ldn(2026, 10, 25, 0, 0), 50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			end := tc.day.AddDate(0, 0, 1)
			n := int(end.Sub(tc.day) / HalfHour)
			bs, err := Group(halfHours(tc.day, n, 1), "day", tc.day, end)
			if err != nil {
				t.Fatal(err)
			}
			if len(bs) != 1 {
				t.Fatalf("got %d buckets, want 1", len(bs))
			}
			b := bs[0]
			if b.Expected != tc.wantExpected || b.Intervals != tc.wantExpected || !near(b.Value, float64(tc.wantExpected)) {
				t.Errorf("bucket = %+v, want expected/intervals %d", b, tc.wantExpected)
			}
		})
	}
}

func TestGroupBoundariesUseLocalTime(t *testing.T) {
	// 23:30 UTC on 30 Jun is 00:30 BST on 1 Jul: it belongs to 1 July.
	start, end := ldn(2026, 6, 30, 0, 0), ldn(2026, 7, 2, 0, 0)
	rs := []Reading{
		{Start: utc(2026, 6, 30, 22, 30), Value: 1}, // 23:30 BST on 30 Jun
		{Start: utc(2026, 6, 30, 23, 30), Value: 2}, // 00:30 BST on 1 Jul
	}
	bs, err := Group(rs, "day", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || !near(bs[0].Value, 1) || !near(bs[1].Value, 2) {
		t.Errorf("buckets = %+v", bs)
	}
}

func TestGroupPeriods(t *testing.T) {
	tests := []struct {
		name       string
		group      string
		start, end time.Time
		wantStarts []time.Time
	}{
		{"week starts Monday", "week", ldn(2026, 10, 28, 0, 0), ldn(2026, 11, 3, 0, 0),
			[]time.Time{ldn(2026, 10, 26, 0, 0), ldn(2026, 11, 2, 0, 0)}},
		{"month", "month", ldn(2026, 10, 15, 0, 0), ldn(2026, 12, 2, 0, 0),
			[]time.Time{ldn(2026, 10, 1, 0, 0), ldn(2026, 11, 1, 0, 0), ldn(2026, 12, 1, 0, 0)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bs, err := Group(nil, tc.group, tc.start, tc.end)
			if err != nil {
				t.Fatal(err)
			}
			if len(bs) != len(tc.wantStarts) {
				t.Fatalf("got %d buckets, want %d", len(bs), len(tc.wantStarts))
			}
			for i, b := range bs {
				if !b.Start.Equal(tc.wantStarts[i]) {
					t.Errorf("bucket %d starts %v, want %v", i, b.Start, tc.wantStarts[i])
				}
				if b.Intervals != 0 {
					t.Errorf("bucket %d has readings", i)
				}
			}
		})
	}
}

func TestGroupPartialBucketExpectedIsClipped(t *testing.T) {
	// Range starts mid-month: expected only counts the requested part.
	bs, _ := Group(nil, "month", ldn(2026, 11, 29, 0, 0), ldn(2026, 12, 1, 0, 0))
	if len(bs) != 1 || bs[0].Expected != 96 {
		t.Errorf("got %+v, want one bucket expecting 96", bs)
	}
}

func TestGroupUnknown(t *testing.T) {
	if _, err := Group(nil, "fortnight", utc(2026, 1, 1, 0, 0), utc(2026, 1, 2, 0, 0)); err == nil {
		t.Error("expected error")
	}
}

func TestFindGaps(t *testing.T) {
	start := utc(2026, 11, 1, 0, 0)
	end := utc(2026, 11, 1, 6, 0) // 12 slots
	all := halfHours(start, 12, 1)
	drop := func(idx ...int) []Reading {
		skip := map[int]bool{}
		for _, i := range idx {
			skip[i] = true
		}
		var out []Reading
		for i, r := range all {
			if !skip[i] {
				out = append(out, r)
			}
		}
		return out
	}
	tests := []struct {
		name   string
		rs     []Reading
		minGap time.Duration
		want   []Gap
	}{
		{"complete", all, time.Hour, nil},
		{"no readings at all", nil, time.Hour, []Gap{{start, end, 12}}},
		{"one slot below threshold", drop(3), time.Hour, nil},
		{"one slot at threshold 30m", drop(3), 30 * time.Minute, []Gap{{utc(2026, 11, 1, 1, 30), utc(2026, 11, 1, 2, 0), 1}}},
		{"two separate gaps", drop(2, 3, 8, 9, 10), time.Hour, []Gap{
			{utc(2026, 11, 1, 1, 0), utc(2026, 11, 1, 2, 0), 2},
			{utc(2026, 11, 1, 4, 0), utc(2026, 11, 1, 5, 30), 3},
		}},
		{"leading and trailing", drop(0, 1, 10, 11), time.Hour, []Gap{
			{start, utc(2026, 11, 1, 1, 0), 2},
			{utc(2026, 11, 1, 5, 0), end, 2},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FindGaps(tc.rs, start, end, tc.minGap)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			for i := range got {
				if !got[i].From.Equal(tc.want[i].From) || !got[i].To.Equal(tc.want[i].To) || got[i].Slots != tc.want[i].Slots {
					t.Errorf("gap %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestExpectedSlots(t *testing.T) {
	tests := []struct {
		name       string
		start, end time.Time
		want       int
	}{
		{"one day", utc(2026, 11, 1, 0, 0), utc(2026, 11, 2, 0, 0), 48},
		{"unaligned start rounds up", utc(2026, 11, 1, 0, 10), utc(2026, 11, 1, 1, 0), 1},
		{"unaligned end rounds down", utc(2026, 11, 1, 0, 0), utc(2026, 11, 1, 1, 20), 2},
		{"empty", utc(2026, 11, 1, 0, 10), utc(2026, 11, 1, 0, 20), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpectedSlots(tc.start, tc.end); got != tc.want {
				t.Errorf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestRateSeriesAt(t *testing.T) {
	to := utc(2026, 1, 2, 0, 0)
	s := NewRateSeries([]RatePeriod{
		{From: utc(2026, 1, 2, 0, 0), Pence: 30},
		{From: utc(2026, 1, 1, 0, 0), To: &to, Pence: 20}, // deliberately unsorted
	})
	tests := []struct {
		name   string
		at     time.Time
		want   float64
		wantOK bool
	}{
		{"before any", utc(2025, 12, 31, 23, 30), 0, false},
		{"first period", utc(2026, 1, 1, 12, 0), 20, true},
		{"boundary belongs to next", utc(2026, 1, 2, 0, 0), 30, true},
		{"open ended", utc(2030, 1, 1, 0, 0), 30, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := s.At(tc.at)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("At = %v,%v want %v,%v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestCostFluxBands(t *testing.T) {
	// One GMT day of 0.5 kWh per slot on a Flux-shaped rate card.
	day := utc(2026, 12, 1, 0, 0)
	to := func(h int) *time.Time { t := day.Add(time.Duration(h) * time.Hour); return &t }
	rates := NewRateSeries([]RatePeriod{
		{From: day, To: to(2), Pence: 25},
		{From: *to(2), To: to(5), Pence: 8},
		{From: *to(5), To: to(16), Pence: 25},
		{From: *to(16), To: to(19), Pence: 40},
		{From: *to(19), To: to(24), Pence: 25},
	})
	res := Cost(halfHours(day, 48, 0.5), rates, FluxBand)
	if res.Unpriced != 0 {
		t.Fatalf("unpriced = %d", res.Unpriced)
	}
	want := map[string][2]float64{"off-peak": {3, 24}, "peak": {3, 120}, "day": {18, 450}}
	for band, w := range want {
		b := res.Bands[band]
		if b == nil || !near(b.KWh, w[0]) || !near(b.Pence, w[1]) {
			t.Errorf("band %s = %+v, want kWh %v pence %v", band, b, w[0], w[1])
		}
	}
	if !near(res.Pence, 594) || !near(res.KWh, 24) {
		t.Errorf("total = %v p / %v kWh", res.Pence, res.KWh)
	}
}

func TestCostUnpriced(t *testing.T) {
	rates := NewRateSeries([]RatePeriod{{From: utc(2026, 1, 1, 1, 0), Pence: 10}})
	res := Cost(halfHours(utc(2026, 1, 1, 0, 0), 4, 1), rates, nil)
	if res.Unpriced != 2 || !near(res.Pence, 20) || !near(res.KWh, 4) {
		t.Errorf("got %+v", res)
	}
}

func TestStandingCharge(t *testing.T) {
	sc := NewRateSeries([]RatePeriod{{From: utc(2020, 1, 1, 0, 0), Pence: 50}})
	tests := []struct {
		name       string
		start, end time.Time
		wantP      float64
		wantDays   float64
		unpriced   int
	}{
		{"seven days", ldn(2026, 10, 26, 0, 0), ldn(2026, 11, 2, 0, 0), 350, 7, 0},
		{"DST 25h day still one day", ldn(2026, 10, 25, 0, 0), ldn(2026, 10, 26, 0, 0), 50, 1, 0},
		{"half a day", ldn(2026, 10, 26, 0, 0), ldn(2026, 10, 26, 12, 0), 25, 0.5, 0},
		{"before rate exists", utc(2019, 12, 30, 0, 0), utc(2019, 12, 31, 0, 0), 0, 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, d, u := StandingCharge(sc, tc.start, tc.end)
			if math.Abs(p-tc.wantP) > 1e-6 || math.Abs(d-tc.wantDays) > 1e-6 || u != tc.unpriced {
				t.Errorf("got %v p, %v days, %d unpriced; want %v, %v, %d", p, d, u, tc.wantP, tc.wantDays, tc.unpriced)
			}
		})
	}
}

func TestGasM3ToKWh(t *testing.T) {
	// 1 m³ × 1.02264 × 39.5 / 3.6
	want := 1.02264 * 39.5 / 3.6
	if got := GasM3ToKWh(1); !near(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if got := GasM3ToKWh(0); got != 0 {
		t.Errorf("got %v want 0", got)
	}
	if got := GasM3ToKWh(1); got < 11.2 || got > 11.3 {
		t.Errorf("1 m³ should be about 11.22 kWh, got %v", got)
	}
}

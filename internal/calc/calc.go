// Package calc holds the pure time, grouping, gap and cost logic. It has no
// network dependencies so it can be tested exhaustively.
package calc

import (
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // Windows may have no system zoneinfo; embed it.
)

// London is the zone Flux bands and billing days are defined in. The API
// itself speaks UTC.
var London = mustLoad("Europe/London")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// HalfHour is the length of one smart-meter reading interval.
const HalfHour = 30 * time.Minute

// Gas volume to energy: m³ × volume correction × calorific value (MJ/m³) / 3.6.
const (
	GasVolumeCorrection = 1.02264
	GasCalorificValue   = 39.5
)

// GasM3ToKWh converts a gas volume in cubic metres to kWh.
func GasM3ToKWh(m3 float64) float64 {
	return m3 * GasVolumeCorrection * GasCalorificValue / 3.6
}

// Reading is a value (kWh) for the half hour starting at Start.
type Reading struct {
	Start time.Time
	Value float64
}

// ParseRange parses a from/to pair. Each bound is a YYYY-MM-DD date (UK local
// midnight; a date-only "to" is inclusive of that whole day) or an RFC3339
// timestamp (an exclusive end).
func ParseRange(from, to string) (time.Time, time.Time, error) {
	start, _, err := parseBound(from)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("from: %w", err)
	}
	end, dateOnly, err := parseBound(to)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("to: %w", err)
	}
	if dateOnly {
		end = end.AddDate(0, 0, 1)
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end %s is not after start %s", to, from)
	}
	return start, end, nil
}

func parseBound(s string) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if t, err := time.ParseInLocation("2006-01-02", s, London); err == nil {
		return t, true, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}
	return time.Time{}, false, fmt.Errorf("%q is not a YYYY-MM-DD date or RFC3339 timestamp", s)
}

// Bucket is one grouped period of consumption. Value is meaningful only when
// Intervals > 0; Expected is how many half hours the period should contain
// (23h and 25h DST days are handled).
type Bucket struct {
	Start     time.Time
	Value     float64
	Intervals int
	Expected  int
}

func dayStart(t time.Time) time.Time {
	l := t.In(London)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, London)
}

func bucketStart(t time.Time, g string) time.Time {
	d := dayStart(t)
	switch g {
	case "week":
		return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7)) // Monday
	case "month":
		return time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, London)
	}
	return d
}

func nextBucket(t time.Time, g string) time.Time {
	switch g {
	case "week":
		return t.AddDate(0, 0, 7)
	case "month":
		return t.AddDate(0, 1, 0)
	}
	return t.AddDate(0, 0, 1)
}

// Group aggregates readings into half_hour, day, week (Monday start) or month
// buckets in UK local time, covering every period in [start, end) including
// those with no readings.
func Group(rs []Reading, g string, start, end time.Time) ([]Bucket, error) {
	switch g {
	case "half_hour":
		out := make([]Bucket, 0, len(rs))
		for _, r := range rs {
			if r.Start.Before(start) || !r.Start.Before(end) {
				continue
			}
			out = append(out, Bucket{Start: r.Start, Value: r.Value, Intervals: 1, Expected: 1})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
		return out, nil
	case "day", "week", "month":
	default:
		return nil, fmt.Errorf("unknown grouping %q (use half_hour, day, week or month)", g)
	}
	var out []Bucket
	idx := map[int64]int{}
	for b := bucketStart(start, g); b.Before(end); b = nextBucket(b, g) {
		cs, ce := later(b, start), earlier(nextBucket(b, g), end)
		idx[b.Unix()] = len(out)
		out = append(out, Bucket{Start: b, Expected: int(ce.Sub(cs) / HalfHour)})
	}
	for _, r := range rs {
		if r.Start.Before(start) || !r.Start.Before(end) {
			continue
		}
		if i, ok := idx[bucketStart(r.Start, g).Unix()]; ok {
			out[i].Value += r.Value
			out[i].Intervals++
		}
	}
	return out, nil
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Gap is a run of consecutive missing half-hour readings covering [From, To).
type Gap struct {
	From, To time.Time
	Slots    int
}

func ceilHalfHour(t time.Time) time.Time {
	tt := t.Truncate(HalfHour)
	if tt.Before(t) {
		tt = tt.Add(HalfHour)
	}
	return tt
}

// ExpectedSlots is how many whole half-hour intervals fit in [start, end).
func ExpectedSlots(start, end time.Time) int {
	n := int(end.Truncate(HalfHour).Sub(ceilHalfHour(start)) / HalfHour)
	if n < 0 {
		return 0
	}
	return n
}

// FindGaps lists runs of missing half hours in [start, end) of at least minGap.
func FindGaps(rs []Reading, start, end time.Time, minGap time.Duration) []Gap {
	have := make(map[int64]struct{}, len(rs))
	for _, r := range rs {
		have[r.Start.Unix()] = struct{}{}
	}
	var gaps []Gap
	var cur *Gap
	flush := func() {
		if cur != nil && time.Duration(cur.Slots)*HalfHour >= minGap {
			gaps = append(gaps, *cur)
		}
		cur = nil
	}
	for t := ceilHalfHour(start); !t.Add(HalfHour).After(end); t = t.Add(HalfHour) {
		if _, ok := have[t.Unix()]; ok {
			flush()
			continue
		}
		if cur == nil {
			cur = &Gap{From: t}
		}
		cur.To = t.Add(HalfHour)
		cur.Slots++
	}
	flush()
	return gaps
}

// RatePeriod is a price in pence valid from From until To (nil = open ended).
type RatePeriod struct {
	From  time.Time
	To    *time.Time
	Pence float64
}

// RateSeries answers "what was the price at this instant".
type RateSeries struct{ p []RatePeriod }

func NewRateSeries(ps []RatePeriod) RateSeries {
	c := append([]RatePeriod(nil), ps...)
	sort.Slice(c, func(i, j int) bool { return c[i].From.Before(c[j].From) })
	return RateSeries{p: c}
}

func (s RateSeries) Len() int { return len(s.p) }

func (s RateSeries) At(t time.Time) (float64, bool) {
	i := sort.Search(len(s.p), func(i int) bool { return s.p[i].From.After(t) }) - 1
	if i < 0 {
		return 0, false
	}
	p := s.p[i]
	if p.To != nil && !t.Before(*p.To) {
		return 0, false
	}
	return p.Pence, true
}

// FluxBand names the Octopus Flux time band of an instant. Bands are fixed in
// UK local time: off-peak 02:00-05:00, peak 16:00-19:00, otherwise day.
func FluxBand(t time.Time) string {
	l := t.In(London)
	m := l.Hour()*60 + l.Minute()
	switch {
	case m >= 2*60 && m < 5*60:
		return "off-peak"
	case m >= 16*60 && m < 19*60:
		return "peak"
	}
	return "day"
}

// BandCost is energy and cost within one band.
type BandCost struct {
	KWh   float64
	Pence float64
}

// CostResult is the energy cost of a set of readings.
type CostResult struct {
	KWh      float64
	Pence    float64
	Unpriced int // readings with no matching rate (excluded from Pence)
	Bands    map[string]*BandCost
}

// Cost prices each reading at the rate in force at its start. band may be nil.
func Cost(rs []Reading, rates RateSeries, band func(time.Time) string) CostResult {
	res := CostResult{Bands: map[string]*BandCost{}}
	for _, r := range rs {
		res.KWh += r.Value
		p, ok := rates.At(r.Start)
		if !ok {
			res.Unpriced++
			continue
		}
		name := "all"
		if band != nil {
			name = band(r.Start)
		}
		b := res.Bands[name]
		if b == nil {
			b = &BandCost{}
			res.Bands[name] = b
		}
		b.KWh += r.Value
		b.Pence += r.Value * p
		res.Pence += r.Value * p
	}
	return res
}

// StandingCharge sums the daily charge over [start, end), counting each UK
// local day (proportionally for partial days). days is the number of
// day-equivalents charged; unpriced counts days with no rate.
func StandingCharge(sc RateSeries, start, end time.Time) (pence, days float64, unpriced int) {
	for d := dayStart(start); d.Before(end); d = d.AddDate(0, 0, 1) {
		next := d.AddDate(0, 0, 1)
		cs, ce := later(d, start), earlier(next, end)
		frac := float64(ce.Sub(cs)) / float64(next.Sub(d))
		v, ok := sc.At(cs)
		if !ok {
			unpriced++
			continue
		}
		pence += v * frac
		days += frac
	}
	return pence, days, unpriced
}

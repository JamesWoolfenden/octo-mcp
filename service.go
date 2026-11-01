package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/JamesWoolfenden/octo-mcp/internal/calc"
	"github.com/JamesWoolfenden/octo-mcp/internal/octopus"
)

const (
	accountTTL     = time.Hour
	maxRangeDays   = 400
	maxHalfHourDay = 7
	maxGapDays     = 90
)

var exportTariff = regexp.MustCompile(`(?i)(export|outgoing)`)

// Service implements the tools on top of the Octopus client.
type Service struct {
	api     *octopus.Client
	account string
	now     func() time.Time

	mu     sync.Mutex
	acct   *octopus.Account
	acctAt time.Time
}

func NewService(api *octopus.Client, account string) *Service {
	return &Service{api: api, account: account, now: time.Now}
}

// meter is one physical meter, discovered from the account.
type meter struct {
	Kind       string // import, export, gas
	Fuel       octopus.Fuel
	Point      string // MPAN or MPRN
	Serial     string
	Agreements []octopus.Agreement
	MovedIn    string
	MovedOut   string
}

func (s *Service) getAccount(ctx context.Context) (*octopus.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acct != nil && s.now().Sub(s.acctAt) < accountTTL {
		return s.acct, nil
	}
	a, err := s.api.Account(ctx, s.account)
	if err != nil {
		return nil, fmt.Errorf("fetching account: %w", err)
	}
	s.acct, s.acctAt = a, s.now()
	return a, nil
}

func lastSerial(ms []octopus.Meter) string {
	if len(ms) == 0 {
		return ""
	}
	return ms[len(ms)-1].SerialNumber
}

func isExport(mp octopus.ElectricityMeterPoint) bool {
	if mp.IsExport {
		return true
	}
	for _, a := range mp.Agreements {
		if exportTariff.MatchString(a.TariffCode) {
			return true
		}
	}
	return false
}

func buildMeters(a *octopus.Account) []meter {
	var out []meter
	for _, p := range a.Properties {
		for _, mp := range p.ElectricityMeterPoints {
			kind := "import"
			if isExport(mp) {
				kind = "export"
			}
			out = append(out, meter{kind, octopus.Electricity, mp.MPAN, lastSerial(mp.Meters), mp.Agreements, p.MovedInAt, p.MovedOutAt})
		}
		for _, mp := range p.GasMeterPoints {
			out = append(out, meter{"gas", octopus.Gas, mp.MPRN, lastSerial(mp.Meters), mp.Agreements, p.MovedInAt, p.MovedOutAt})
		}
	}
	return out
}

// findMeter returns the meter of the given kind, preferring a current property.
func (s *Service) findMeter(ctx context.Context, kind string) (meter, error) {
	a, err := s.getAccount(ctx)
	if err != nil {
		return meter{}, err
	}
	var fallback *meter
	for _, m := range buildMeters(a) {
		m := m
		if m.Kind != kind || m.Serial == "" {
			continue
		}
		if m.MovedOut == "" {
			return m, nil
		}
		if fallback == nil {
			fallback = &m
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return meter{}, fmt.Errorf("no %s meter found on this account", kind)
}

func (s *Service) currentTariff(m meter) string {
	now := s.now()
	for _, a := range m.Agreements {
		if !a.ValidFrom.After(now) && (a.ValidTo == nil || now.Before(*a.ValidTo)) {
			return a.TariffCode
		}
	}
	return ""
}

// mask hides all but the last four characters of an identifier.
func mask(id string) string {
	if len(id) <= 4 {
		return strings.Repeat("*", len(id))
	}
	return strings.Repeat("*", len(id)-4) + id[len(id)-4:]
}

func local(t time.Time) string { return t.In(calc.London).Format(time.RFC3339) }

func round(v float64, dp int) float64 {
	p := math.Pow10(dp)
	return math.Round(v*p) / p
}

func pounds(pence float64) float64 { return math.Round(pence) / 100 }

// ---------------------------------------------------------------- readings

// gasInM3 decides whether gas readings are cubic metres. SMETS2 gas serials
// ("E6S...") report m³; override with gas_unit.
func gasInM3(serial, unit string) (bool, string) {
	switch strings.ToLower(unit) {
	case "m3":
		return true, "gas_unit=m3 given: readings treated as m³"
	case "kwh":
		return false, "gas_unit=kwh given: readings treated as kWh"
	}
	if strings.HasPrefix(strings.ToUpper(serial), "E6S") {
		return true, "gas meter looks like SMETS2 (serial E6S...), so readings were assumed to be m³; pass gas_unit=kwh to override"
	}
	return false, "gas readings assumed to be kWh (serial is not SMETS2 style); pass gas_unit=m3 to override"
}

// kwhReadings fetches readings and returns them in kWh, with notes explaining
// any conversion.
func (s *Service) kwhReadings(ctx context.Context, m meter, start, end time.Time, gasUnit string) ([]calc.Reading, []string, error) {
	raw, err := s.api.Consumption(ctx, m.Fuel, m.Point, m.Serial, start, end)
	if err != nil {
		return nil, nil, fmt.Errorf("fetching %s consumption: %w", m.Kind, err)
	}
	var notes []string
	convert := false
	if m.Fuel == octopus.Gas && len(raw) > 0 {
		var n string
		convert, n = gasInM3(m.Serial, gasUnit)
		if convert {
			n = fmt.Sprintf("Converted gas m³ to kWh using volume correction %v × calorific value %v MJ/m³ ÷ 3.6. %s",
				calc.GasVolumeCorrection, calc.GasCalorificValue, n)
		}
		notes = append(notes, n)
	}
	out := make([]calc.Reading, 0, len(raw))
	for _, r := range raw {
		v := r.Consumption
		if convert {
			v = calc.GasM3ToKWh(v)
		}
		out = append(out, calc.Reading{Start: r.IntervalStart, Value: v})
	}
	return out, notes, nil
}

func (s *Service) checkRange(start, end time.Time) error {
	if end.Sub(start) > maxRangeDays*24*time.Hour {
		return fmt.Errorf("range too long: the maximum is %d days", maxRangeDays)
	}
	return nil
}

// ------------------------------------------------------------- get_account

type AgreementOut struct {
	Tariff string `json:"tariff_code"`
	From   string `json:"valid_from"`
	To     string `json:"valid_to,omitempty"`
	Active bool   `json:"active"`
}

type MeterOut struct {
	Kind          string         `json:"kind"`
	MeterPoint    string         `json:"meter_point_masked"`
	Serial        string         `json:"serial_masked"`
	CurrentTariff string         `json:"current_tariff,omitempty"`
	Agreements    []AgreementOut `json:"agreements"`
}

type PropertyOut struct {
	MovedIn  string     `json:"moved_in,omitempty"`
	MovedOut string     `json:"moved_out,omitempty"`
	Meters   []MeterOut `json:"meters"`
}

type AccountOut struct {
	Account    string        `json:"account_masked"`
	Properties []PropertyOut `json:"properties"`
	Note       string        `json:"note"`
}

func (s *Service) GetAccount(ctx context.Context) (*AccountOut, error) {
	a, err := s.getAccount(ctx)
	if err != nil {
		return nil, err
	}
	out := &AccountOut{
		Account:    mask(a.Number),
		Properties: []PropertyOut{},
		Note:       "Account, meter point and serial numbers are masked; use the 'meter' argument (import, export, gas) on other tools.",
	}
	now := s.now()
	for _, p := range a.Properties {
		po := PropertyOut{MovedIn: p.MovedInAt, MovedOut: p.MovedOutAt, Meters: []MeterOut{}}
		for _, m := range buildMeters(&octopus.Account{Properties: []octopus.Property{p}}) {
			mo := MeterOut{Kind: m.Kind, MeterPoint: mask(m.Point), Serial: mask(m.Serial), CurrentTariff: s.currentTariff(m), Agreements: []AgreementOut{}}
			for _, ag := range m.Agreements {
				ao := AgreementOut{Tariff: ag.TariffCode, From: local(ag.ValidFrom)}
				if ag.ValidTo != nil {
					ao.To = local(*ag.ValidTo)
				}
				ao.Active = !ag.ValidFrom.After(now) && (ag.ValidTo == nil || now.Before(*ag.ValidTo))
				mo.Agreements = append(mo.Agreements, ao)
			}
			po.Meters = append(po.Meters, mo)
		}
		out.Properties = append(out.Properties, po)
	}
	return out, nil
}

// --------------------------------------------------------- get_consumption

type BucketOut struct {
	Start     string   `json:"start"`
	KWh       *float64 `json:"kwh"`
	Intervals int      `json:"readings"`
	Expected  int      `json:"expected_readings"`
}

type ConsumptionOut struct {
	Meter    string      `json:"meter"`
	From     string      `json:"from"`
	To       string      `json:"to"`
	GroupBy  string      `json:"group_by"`
	Status   string      `json:"status"` // ok, partial, no_data
	TotalKWh *float64    `json:"total_kwh"`
	Readings int         `json:"readings_found"`
	Expected int         `json:"readings_expected"`
	Buckets  []BucketOut `json:"buckets"`
	Notes    []string    `json:"notes"`
}

func (s *Service) GetConsumption(ctx context.Context, kind, from, to, group, gasUnit string) (*ConsumptionOut, error) {
	if group == "" {
		group = "day"
	}
	start, end, err := calc.ParseRange(from, to)
	if err != nil {
		return nil, err
	}
	if err := s.checkRange(start, end); err != nil {
		return nil, err
	}
	if group == "half_hour" && end.Sub(start) > maxHalfHourDay*24*time.Hour {
		return nil, fmt.Errorf("half_hour grouping is limited to %d days; use day, week or month", maxHalfHourDay)
	}
	m, err := s.findMeter(ctx, kind)
	if err != nil {
		return nil, err
	}
	rs, notes, err := s.kwhReadings(ctx, m, start, end, gasUnit)
	if err != nil {
		return nil, err
	}
	buckets, err := calc.Group(rs, group, start, end)
	if err != nil {
		return nil, err
	}
	out := &ConsumptionOut{
		Meter: kind, From: local(start), To: local(end), GroupBy: group,
		Readings: len(rs), Expected: calc.ExpectedSlots(start, end),
		Buckets: []BucketOut{}, Notes: append([]string{}, notes...),
	}
	if len(rs) == 0 {
		out.Status = "no_data"
		out.Notes = append(out.Notes, fmt.Sprintf("The API returned no readings for the %s meter in this period. This is NOT zero consumption: the meter is not reporting data. Use check_data_gaps for detail.", kind))
		return out, nil
	}
	var total float64
	for _, b := range buckets {
		bo := BucketOut{Start: local(b.Start), Intervals: b.Intervals, Expected: b.Expected}
		if b.Intervals > 0 {
			v := round(b.Value, 3)
			bo.KWh = &v
			total += b.Value
		}
		out.Buckets = append(out.Buckets, bo)
	}
	t := round(total, 3)
	out.TotalKWh = &t
	out.Status = "ok"
	if out.Readings < out.Expected {
		out.Status = "partial"
		out.Notes = append(out.Notes, fmt.Sprintf("Only %d of %d expected half-hour readings are present; totals understate real use. Buckets with kwh null have no readings at all.", out.Readings, out.Expected))
	}
	return out, nil
}

// --------------------------------------------------------------- get_rates

type RateOut struct {
	From   string  `json:"from"`
	To     string  `json:"to,omitempty"`
	IncVAT float64 `json:"pence_inc_vat"`
	ExcVAT float64 `json:"pence_exc_vat"`
	Band   string  `json:"band,omitempty"`
}

type RatesOut struct {
	Tariff          string    `json:"tariff_code"`
	From            string    `json:"from"`
	To              string    `json:"to"`
	UnitRates       []RateOut `json:"unit_rates_pence_per_kwh"`
	StandingCharges []RateOut `json:"standing_charges_pence_per_day"`
	Notes           []string  `json:"notes"`
}

func isFlux(tariff string) bool { return strings.Contains(strings.ToUpper(tariff), "FLUX") }

func usableRates(rs []octopus.Rate) []octopus.Rate {
	out := rs[:0:0]
	for _, r := range rs {
		if r.PaymentMethod != nil && strings.EqualFold(*r.PaymentMethod, "NON_DIRECT_DEBIT") {
			continue
		}
		out = append(out, r)
	}
	return out
}

func toPeriods(rs []octopus.Rate) []calc.RatePeriod {
	rs = usableRates(rs)
	out := make([]calc.RatePeriod, 0, len(rs))
	for _, r := range rs {
		out = append(out, calc.RatePeriod{From: r.ValidFrom, To: r.ValidTo, Pence: r.ValueIncVAT})
	}
	return out
}

func (s *Service) GetRates(ctx context.Context, tariff, kind, from, to string) (*RatesOut, error) {
	if tariff == "" {
		if kind == "" {
			kind = "import"
		}
		m, err := s.findMeter(ctx, kind)
		if err != nil {
			return nil, err
		}
		if tariff = s.currentTariff(m); tariff == "" {
			return nil, fmt.Errorf("no current tariff on the %s meter; pass tariff_code", kind)
		}
	}
	if from == "" {
		from = s.now().In(calc.London).Format("2006-01-02")
	}
	if to == "" {
		to = from
	}
	start, end, err := calc.ParseRange(from, to)
	if err != nil {
		return nil, err
	}
	if end.Sub(start) > 31*24*time.Hour {
		return nil, errors.New("get_rates is limited to 31 days")
	}
	units, err := s.api.UnitRates(ctx, tariff, start, end)
	if err != nil {
		return nil, fmt.Errorf("fetching unit rates: %w", err)
	}
	standing, err := s.api.StandingCharges(ctx, tariff, start, end)
	if err != nil {
		return nil, fmt.Errorf("fetching standing charges: %w", err)
	}
	out := &RatesOut{Tariff: tariff, From: local(start), To: local(end), UnitRates: []RateOut{}, StandingCharges: []RateOut{}, Notes: []string{}}
	conv := func(r octopus.Rate, banded bool) RateOut {
		ro := RateOut{From: local(r.ValidFrom), IncVAT: round(r.ValueIncVAT, 4), ExcVAT: round(r.ValueExcVAT, 4)}
		if r.ValidTo != nil {
			ro.To = local(*r.ValidTo)
		}
		if banded {
			ro.Band = calc.FluxBand(later(r.ValidFrom, start))
		}
		return ro
	}
	for _, r := range sortedRates(usableRates(units)) {
		out.UnitRates = append(out.UnitRates, conv(r, isFlux(tariff)))
	}
	for _, r := range sortedRates(usableRates(standing)) {
		out.StandingCharges = append(out.StandingCharges, conv(r, false))
	}
	if isFlux(tariff) {
		out.Notes = append(out.Notes, "Flux bands are in UK local time: off-peak 02:00-05:00, peak 16:00-19:00, day otherwise. Times shown are UK local; the API itself uses UTC.")
	}
	if len(out.UnitRates) == 0 {
		out.Notes = append(out.Notes, "No unit rates were returned for this tariff and period.")
	}
	return out, nil
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func sortedRates(rs []octopus.Rate) []octopus.Rate {
	out := append([]octopus.Rate(nil), rs...)
	for i := 1; i < len(out); i++ { // insertion sort: inputs are near-sorted and small
		for j := i; j > 0 && out[j].ValidFrom.Before(out[j-1].ValidFrom); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// --------------------------------------------------------------- get_costs

type BandOut struct {
	Band   string  `json:"band"`
	KWh    float64 `json:"kwh"`
	Pounds float64 `json:"pounds"`
	AvgP   float64 `json:"average_pence_per_kwh"`
}

type SideOut struct {
	Meter            string    `json:"meter"`
	Status           string    `json:"status"` // ok, partial, no_data
	Tariffs          []string  `json:"tariffs"`
	KWh              float64   `json:"kwh"`
	Readings         int       `json:"readings_found"`
	Expected         int       `json:"readings_expected"`
	UnpricedReadings int       `json:"readings_without_a_rate"`
	EnergyPounds     float64   `json:"energy_pounds"`
	StandingPounds   float64   `json:"standing_charge_pounds"`
	TotalPounds      float64   `json:"total_pounds"`
	Bands            []BandOut `json:"bands"`
	Notes            []string  `json:"notes"`
}

type CostsOut struct {
	From          string   `json:"from"`
	To            string   `json:"to"`
	Import        *SideOut `json:"import,omitempty"`
	Export        *SideOut `json:"export_earnings,omitempty"`
	Gas           *SideOut `json:"gas,omitempty"`
	NetCostPounds *float64 `json:"net_cost_pounds,omitempty"`
	Notes         []string `json:"notes"`
}

func (s *Service) side(ctx context.Context, m meter, start, end time.Time, gasUnit string) (*SideOut, error) {
	rs, notes, err := s.kwhReadings(ctx, m, start, end, gasUnit)
	if err != nil {
		return nil, err
	}
	so := &SideOut{Meter: m.Kind, Tariffs: []string{}, Bands: []BandOut{}, Notes: append([]string{}, notes...), Readings: len(rs), Expected: calc.ExpectedSlots(start, end)}
	if len(rs) == 0 {
		so.Status = "no_data"
		so.Notes = append(so.Notes, fmt.Sprintf("No readings for the %s meter in this period; costs cannot be calculated (this is not zero).", m.Kind))
		return so, nil
	}
	var energy, standing float64
	bands := map[string]*calc.BandCost{}
	var scMissing int
	for _, ag := range m.Agreements {
		as, ae := later(ag.ValidFrom, start), end
		if ag.ValidTo != nil && ag.ValidTo.Before(ae) {
			ae = *ag.ValidTo
		}
		if !ae.After(as) {
			continue
		}
		var sub []calc.Reading
		for _, r := range rs {
			if !r.Start.Before(as) && r.Start.Before(ae) {
				sub = append(sub, r)
			}
		}
		units, err := s.api.UnitRates(ctx, ag.TariffCode, as, ae)
		if err != nil {
			return nil, fmt.Errorf("fetching unit rates for %s: %w", ag.TariffCode, err)
		}
		scs, err := s.api.StandingCharges(ctx, ag.TariffCode, as, ae)
		if err != nil && !errors.Is(err, octopus.ErrNotFound) {
			return nil, fmt.Errorf("fetching standing charges for %s: %w", ag.TariffCode, err)
		}
		var band func(time.Time) string
		if isFlux(ag.TariffCode) {
			band = calc.FluxBand
		}
		cr := calc.Cost(sub, calc.NewRateSeries(toPeriods(units)), band)
		so.Tariffs = append(so.Tariffs, ag.TariffCode)
		so.KWh += cr.KWh
		so.UnpricedReadings += cr.Unpriced
		energy += cr.Pence
		for name, b := range cr.Bands {
			if bands[name] == nil {
				bands[name] = &calc.BandCost{}
			}
			bands[name].KWh += b.KWh
			bands[name].Pence += b.Pence
		}
		if sp := toPeriods(scs); len(sp) > 0 {
			p, _, missing := calc.StandingCharge(calc.NewRateSeries(sp), as, ae)
			standing += p
			scMissing += missing
		} else if m.Kind != "export" {
			so.Notes = append(so.Notes, fmt.Sprintf("No standing charge returned for %s.", ag.TariffCode))
		}
	}
	if len(so.Tariffs) == 0 {
		so.Notes = append(so.Notes, "No tariff agreement covers this period.")
	}
	for _, name := range []string{"off-peak", "day", "peak", "all"} {
		if b := bands[name]; b != nil {
			so.Bands = append(so.Bands, BandOut{Band: name, KWh: round(b.KWh, 3), Pounds: pounds(b.Pence), AvgP: round(b.Pence/b.KWh, 2)})
		}
	}
	so.KWh = round(so.KWh, 3)
	so.EnergyPounds = pounds(energy)
	so.StandingPounds = pounds(standing)
	so.TotalPounds = pounds(energy + standing)
	so.Status = "ok"
	if so.Readings < so.Expected || so.UnpricedReadings > 0 || scMissing > 0 {
		so.Status = "partial"
		so.Notes = append(so.Notes, fmt.Sprintf("Incomplete: %d of %d readings present, %d readings had no rate, %d days had no standing charge. Totals understate the real cost.", so.Readings, so.Expected, so.UnpricedReadings, scMissing))
	}
	return so, nil
}

func (s *Service) GetCosts(ctx context.Context, kind, from, to, gasUnit string) (*CostsOut, error) {
	start, end, err := calc.ParseRange(from, to)
	if err != nil {
		return nil, err
	}
	if err := s.checkRange(start, end); err != nil {
		return nil, err
	}
	var kinds []string
	switch kind {
	case "", "all":
		kinds = []string{"import", "export"}
	case "import", "export", "gas":
		kinds = []string{kind}
	default:
		return nil, fmt.Errorf("unknown meter %q (use import, export, gas or all)", kind)
	}
	out := &CostsOut{From: local(start), To: local(end), Notes: []string{"Prices are inc VAT. net_cost_pounds = import total (energy + standing charge) minus export earnings; positive means you paid."}}
	for _, k := range kinds {
		m, err := s.findMeter(ctx, k)
		if err != nil {
			return nil, err
		}
		so, err := s.side(ctx, m, start, end, gasUnit)
		if err != nil {
			return nil, err
		}
		switch k {
		case "import":
			out.Import = so
		case "export":
			out.Export = so
		case "gas":
			out.Gas = so
		}
	}
	if out.Import != nil && out.Export != nil && out.Import.Status != "no_data" && out.Export.Status != "no_data" {
		n := round(out.Import.TotalPounds-out.Export.TotalPounds, 2)
		out.NetCostPounds = &n
	}
	return out, nil
}

// --------------------------------------------------------- check_data_gaps

type GapOut struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	Hours    float64 `json:"hours"`
	Missing  int     `json:"missing_readings"`
	Trailing bool    `json:"reaches_end_of_window"`
}

type MeterGapsOut struct {
	Meter      string   `json:"meter"`
	Status     string   `json:"status"` // ok, gaps, no_data
	Readings   int      `json:"readings_found"`
	Expected   int      `json:"readings_expected"`
	FirstInWin string   `json:"first_reading_in_window,omitempty"`
	LastInWin  string   `json:"last_reading_in_window,omitempty"`
	LatestEver string   `json:"latest_reading_ever,omitempty"`
	Gaps       []GapOut `json:"gaps"`
	Notes      []string `json:"notes"`
}

type GapsOut struct {
	From   string         `json:"window_from"`
	To     string         `json:"window_to"`
	Meters []MeterGapsOut `json:"meters"`
}

func (s *Service) CheckDataGaps(ctx context.Context, kind string, days, minGapMinutes int) (*GapsOut, error) {
	if days <= 0 {
		days = 14
	}
	if days > maxGapDays {
		return nil, fmt.Errorf("days is limited to %d", maxGapDays)
	}
	if minGapMinutes <= 0 {
		minGapMinutes = 60
	}
	end := s.now().UTC().Truncate(calc.HalfHour)
	start := end.AddDate(0, 0, -days)
	kinds := []string{"import", "export", "gas"}
	if kind != "" && kind != "all" {
		kinds = []string{kind}
	}
	out := &GapsOut{From: local(start), To: local(end), Meters: []MeterGapsOut{}}
	for _, k := range kinds {
		m, err := s.findMeter(ctx, k)
		if err != nil {
			if kind == "" || kind == "all" {
				continue // meter simply not on this account
			}
			return nil, err
		}
		rs, _, err := s.kwhReadings(ctx, m, start, end, "kwh")
		if err != nil {
			return nil, err
		}
		g := MeterGapsOut{Meter: k, Readings: len(rs), Expected: calc.ExpectedSlots(start, end), Gaps: []GapOut{}, Notes: []string{}}
		if len(rs) == 0 {
			g.Status = "no_data"
			g.Notes = append(g.Notes, fmt.Sprintf("No readings at all in the last %d days. The meter is not reporting; this is missing data, not zero use.", days))
			if latest, err := s.api.LatestReading(ctx, m.Fuel, m.Point, m.Serial); err != nil {
				g.Notes = append(g.Notes, "Could not look up the most recent reading ever recorded.")
			} else if latest == nil {
				g.Notes = append(g.Notes, "The API has no readings for this meter at all.")
			} else {
				g.LatestEver = local(latest.IntervalStart)
			}
			g.Gaps = append(g.Gaps, GapOut{From: local(start), To: local(end), Hours: round(end.Sub(start).Hours(), 1), Missing: g.Expected, Trailing: true})
			out.Meters = append(out.Meters, g)
			continue
		}
		first, last := rs[0].Start, rs[0].Start
		for _, r := range rs {
			if r.Start.Before(first) {
				first = r.Start
			}
			if r.Start.After(last) {
				last = r.Start
			}
		}
		g.FirstInWin, g.LastInWin = local(first), local(last)
		for _, gp := range calc.FindGaps(rs, start, end, time.Duration(minGapMinutes)*time.Minute) {
			g.Gaps = append(g.Gaps, GapOut{From: local(gp.From), To: local(gp.To), Hours: round(gp.To.Sub(gp.From).Hours(), 1), Missing: gp.Slots, Trailing: gp.To.Equal(end.Truncate(calc.HalfHour))})
		}
		g.Status = "ok"
		if len(g.Gaps) > 0 {
			g.Status = "gaps"
		}
		if n := len(g.Gaps); n > 0 && g.Gaps[n-1].Trailing {
			g.Notes = append(g.Notes, "The last gap runs to the end of the window; Octopus data normally lags by up to about a day, so this may just be delay.")
		}
		out.Meters = append(out.Meters, g)
	}
	return out, nil
}

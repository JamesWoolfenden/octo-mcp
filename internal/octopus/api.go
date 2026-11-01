package octopus

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Fuel is the kind of meter point.
type Fuel string

const (
	Electricity Fuel = "electricity"
	Gas         Fuel = "gas"
)

type Account struct {
	Number     string     `json:"number"`
	Properties []Property `json:"properties"`
}

type Property struct {
	ID                     int                     `json:"id"`
	MovedInAt              string                  `json:"moved_in_at"`
	MovedOutAt             string                  `json:"moved_out_at"`
	ElectricityMeterPoints []ElectricityMeterPoint `json:"electricity_meter_points"`
	GasMeterPoints         []GasMeterPoint         `json:"gas_meter_points"`
}

type ElectricityMeterPoint struct {
	MPAN       string      `json:"mpan"`
	IsExport   bool        `json:"is_export"`
	Meters     []Meter     `json:"meters"`
	Agreements []Agreement `json:"agreements"`
}

type GasMeterPoint struct {
	MPRN       string      `json:"mprn"`
	Meters     []Meter     `json:"meters"`
	Agreements []Agreement `json:"agreements"`
}

type Meter struct {
	SerialNumber string `json:"serial_number"`
}

type Agreement struct {
	TariffCode string     `json:"tariff_code"`
	ValidFrom  time.Time  `json:"valid_from"`
	ValidTo    *time.Time `json:"valid_to"`
}

// Reading is one consumption interval. Units are kWh for electricity and
// either kWh or m³ for gas, depending on the meter.
type Reading struct {
	Consumption   float64   `json:"consumption"`
	IntervalStart time.Time `json:"interval_start"`
	IntervalEnd   time.Time `json:"interval_end"`
}

// Rate is a unit rate or standing charge, in pence.
type Rate struct {
	ValueExcVAT   float64    `json:"value_exc_vat"`
	ValueIncVAT   float64    `json:"value_inc_vat"`
	ValidFrom     time.Time  `json:"valid_from"`
	ValidTo       *time.Time `json:"valid_to"`
	PaymentMethod *string    `json:"payment_method"`
}

func (c *Client) Account(ctx context.Context, number string) (*Account, error) {
	var a Account
	if err := c.get(ctx, "accounts/"+url.PathEscape(number)+"/", &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func meterPath(f Fuel, point, serial string) string {
	return fmt.Sprintf("%s-meter-points/%s/meters/%s/consumption/", f, url.PathEscape(point), url.PathEscape(serial))
}

// Consumption returns half-hourly readings in [from, to), oldest first.
func (c *Client) Consumption(ctx context.Context, f Fuel, point, serial string, from, to time.Time) ([]Reading, error) {
	q := url.Values{}
	q.Set("period_from", from.UTC().Format(time.RFC3339))
	q.Set("period_to", to.UTC().Format(time.RFC3339))
	q.Set("order_by", "period")
	q.Set("page_size", "25000")
	return getAll[Reading](ctx, c, meterPath(f, point, serial), q)
}

// LatestReading returns the most recent reading ever recorded, if any.
func (c *Client) LatestReading(ctx context.Context, f Fuel, point, serial string) (*Reading, error) {
	q := url.Values{}
	q.Set("order_by", "-period")
	q.Set("page_size", "1")
	var p page[Reading]
	if err := c.get(ctx, meterPath(f, point, serial)+"?"+q.Encode(), &p); err != nil {
		return nil, err
	}
	if len(p.Results) == 0 {
		return nil, nil
	}
	return &p.Results[0], nil
}

// ProductCode derives the product code from a tariff code,
// e.g. E-1R-FLUX-IMPORT-23-02-14-A -> FLUX-IMPORT-23-02-14.
func ProductCode(tariff string) (string, error) {
	parts := strings.Split(tariff, "-")
	if len(parts) < 4 {
		return "", fmt.Errorf("unrecognised tariff code %q", tariff)
	}
	if parts[1] != "1R" {
		return "", fmt.Errorf("tariff %q is not a single-register tariff, which is all that is supported", tariff)
	}
	return strings.Join(parts[2:len(parts)-1], "-"), nil
}

// FuelOfTariff reports the fuel a tariff code is for.
func FuelOfTariff(tariff string) (Fuel, error) {
	switch {
	case strings.HasPrefix(tariff, "E-"):
		return Electricity, nil
	case strings.HasPrefix(tariff, "G-"):
		return Gas, nil
	}
	return "", fmt.Errorf("unrecognised tariff code %q", tariff)
}

func (c *Client) rates(ctx context.Context, tariff, kind string, from, to time.Time) ([]Rate, error) {
	f, err := FuelOfTariff(tariff)
	if err != nil {
		return nil, err
	}
	product, err := ProductCode(tariff)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("period_from", from.UTC().Format(time.RFC3339))
	q.Set("period_to", to.UTC().Format(time.RFC3339))
	q.Set("page_size", strconv.Itoa(1500))
	path := fmt.Sprintf("products/%s/%s-tariffs/%s/%s/", url.PathEscape(product), f, url.PathEscape(tariff), kind)
	return getAll[Rate](ctx, c, path, q)
}

// UnitRates returns the standard unit rates (pence/kWh) overlapping [from, to).
func (c *Client) UnitRates(ctx context.Context, tariff string, from, to time.Time) ([]Rate, error) {
	return c.rates(ctx, tariff, "standard-unit-rates", from, to)
}

// StandingCharges returns the daily standing charges (pence/day) overlapping [from, to).
func (c *Client) StandingCharges(ctx context.Context, tariff string, from, to time.Time) ([]Rate, error) {
	return c.rates(ctx, tariff, "standing-charges", from, to)
}

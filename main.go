// octo-mcp is a read-only MCP server (stdio) for Octopus Energy account data.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/JamesWoolfenden/octo-mcp/internal/octopus"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

func main() {
	// stdout carries the MCP protocol; log to stderr only, and never the key.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	key, account := os.Getenv("OCTOPUS_API_KEY"), os.Getenv("OCTOPUS_ACCOUNT")
	if key == "" || account == "" {
		log.Error("OCTOPUS_API_KEY and OCTOPUS_ACCOUNT must both be set")
		os.Exit(2)
	}
	api, err := octopus.NewClient(octopus.DefaultBaseURL, key, nil)
	if err != nil {
		log.Error("creating client", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	server := newServer(NewService(api, account))
	log.Info("octo-mcp starting", "version", version, "account", mask(account))
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// jsonResult renders a value as indented JSON text content.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
}

func wrap[In any, Out any](f func(context.Context, In) (Out, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		out, err := f(ctx, in)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("error: %v", err)}}}, nil, nil
		}
		return jsonResult(out)
	}
}

type noInput struct{}

type consumptionIn struct {
	Meter   string `json:"meter" jsonschema:"which meter: import, export or gas"`
	From    string `json:"from" jsonschema:"start, YYYY-MM-DD (UK local midnight) or RFC3339"`
	To      string `json:"to" jsonschema:"end, YYYY-MM-DD (inclusive whole day) or RFC3339 (exclusive)"`
	GroupBy string `json:"group_by,omitempty" jsonschema:"half_hour, day (default), week or month; half_hour is limited to 7 days"`
	GasUnit string `json:"gas_unit,omitempty" jsonschema:"gas meters only: auto (default), m3 or kwh - the unit the meter reports in"`
}

type ratesIn struct {
	TariffCode string `json:"tariff_code,omitempty" jsonschema:"tariff code, e.g. E-1R-FLUX-IMPORT-23-02-14-A; defaults to the meter's current tariff"`
	Meter      string `json:"meter,omitempty" jsonschema:"import, export or gas; used when tariff_code is omitted (default import)"`
	From       string `json:"from,omitempty" jsonschema:"start date YYYY-MM-DD (default today)"`
	To         string `json:"to,omitempty" jsonschema:"end date YYYY-MM-DD inclusive (default same as from); max 31 days"`
}

type costsIn struct {
	Meter   string `json:"meter,omitempty" jsonschema:"import, export, gas or all (default: import and export with a net figure)"`
	From    string `json:"from" jsonschema:"start, YYYY-MM-DD (UK local midnight) or RFC3339"`
	To      string `json:"to" jsonschema:"end, YYYY-MM-DD (inclusive whole day) or RFC3339 (exclusive)"`
	GasUnit string `json:"gas_unit,omitempty" jsonschema:"gas only: auto (default), m3 or kwh"`
}

type gapsIn struct {
	Meter         string `json:"meter,omitempty" jsonschema:"import, export, gas or all (default all)"`
	Days          int    `json:"days,omitempty" jsonschema:"how many days back to check (default 14, max 90)"`
	MinGapMinutes int    `json:"min_gap_minutes,omitempty" jsonschema:"ignore gaps shorter than this (default 60)"`
}

func newServer(svc *Service) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "octo-mcp", Version: version}, nil)
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_account",
		Description: "Octopus account overview: properties, meters (import, export, gas) and current and past tariff agreements. Identifiers are masked.",
		Annotations: ro,
	}, wrap(func(ctx context.Context, _ noInput) (*AccountOut, error) { return svc.GetAccount(ctx) }))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_consumption",
		Description: "Energy used (import, gas) or exported, in kWh, grouped by half hour, day, week or month in UK local time. Gas m³ readings are converted to kWh and the result says so. If the meter returns no readings the status is no_data - that is missing data, never zero.",
		Annotations: ro,
	}, wrap(func(ctx context.Context, in consumptionIn) (*ConsumptionOut, error) {
		return svc.GetConsumption(ctx, in.Meter, in.From, in.To, in.GroupBy, in.GasUnit)
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_rates",
		Description: "Unit rates (p/kWh) and standing charges (p/day), inc and exc VAT, for a tariff code over a date range. Flux rates are labelled off-peak, day or peak.",
		Annotations: ro,
	}, wrap(func(ctx context.Context, in ratesIn) (*RatesOut, error) {
		return svc.GetRates(ctx, in.TariffCode, in.Meter, in.From, in.To)
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_costs",
		Description: "Cost of import (consumption x matching half-hourly rate plus standing charge), earnings for export, and the net figure, inc VAT, with a per-band breakdown. Reports incomplete data as partial rather than hiding it.",
		Annotations: ro,
	}, wrap(func(ctx context.Context, in costsIn) (*CostsOut, error) {
		return svc.GetCosts(ctx, in.Meter, in.From, in.To, in.GasUnit)
	}))

	mcp.AddTool(s, &mcp.Tool{
		Name:        "check_data_gaps",
		Description: "For each meter, list date ranges with no readings over the last N days. The main diagnostic for a meter that has stopped reporting; also reports the latest reading ever recorded when a meter has no recent data.",
		Annotations: ro,
	}, wrap(func(ctx context.Context, in gapsIn) (*GapsOut, error) {
		return svc.CheckDataGaps(ctx, in.Meter, in.Days, in.MinGapMinutes)
	}))

	return s
}

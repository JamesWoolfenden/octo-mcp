# Brief: Octopus Energy MCP server (Go)

## Goal
Build a small, read-only MCP server in Go (stdio transport) that exposes Octopus Energy account data to Claude, so you can ask about usage, export, costs and billing without going through the dashboard.

## Context
- Octopus account: `A-XXXXXXXX` (set via `OCTOPUS_ACCOUNT`)
- Electricity import MPAN, export MPAN and gas MPRN are discovered from the API, never hardcoded
- Gas: the smart meter may not be reporting, so expect empty results. The server must handle this cleanly and say "no data", not return 0 kWh
- Example setup: Octopus Flux (import and export), solar panels and a home battery
- API: Octopus public REST API, `https://api.octopus.energy/v1/`, HTTP basic auth with the API key as the username and an empty password. The key comes from the dashboard → Personal details → API access.

## Requirements
- Go, single binary, `github.com/modelcontextprotocol/go-sdk` (or mark3labs/mcp-go if the official SDK falls short; explain which you chose)
- Config comes from env vars only: `OCTOPUS_API_KEY`, `OCTOPUS_ACCOUNT`. Never log or echo the key. Mask account/meter IDs in logs and output where practical.
- On first call, discover meter serials and MPANs/MPRN from `/accounts/{account}/`. Don't hardcode them.
- Read-only. No endpoints that change anything.
- Test fixtures must use fake IDs (e.g. `A-TEST0001`), never real account or meter numbers.

## Tools to expose
1. `get_account`: properties, meters, current and past agreements (tariff codes and dates)
2. `get_consumption`: meter (import, export or gas), date range, grouping (half-hour, day, week, month). Returns kWh. Converts gas m³ to kWh when the meter reports in m³ (SMETS2 gas often does; volume correction 1.02264 × CV ~39.5 / 3.6), and says when it has.
3. `get_rates`: unit rates and standing charges for a tariff code over a date range (Flux has three time bands)
4. `get_costs`: consumption × matching half-hourly rates, plus standing charge, for import; earnings for export; net figure
5. `check_data_gaps`: for each meter, list date ranges with no readings over the last N days. This is the main diagnostic for a non-reporting meter.

## Quality bar
- Table-driven tests with recorded/fixture API responses (no live calls in tests)
- Handle pagination (`next`), rate limits, and the timezone and DST boundary (API uses UTC; Flux bands are UK local time)
- README: setup, the Claude Desktop / Claude Code MCP config snippet, and example prompts
- Git: commit straight to master

## Acceptance check
Run the server against a real account. Then confirm:
- Electricity import and export costs for a chosen week roughly match the dashboard
- `check_data_gaps` on a non-reporting meter reports the missing data instead of zeros

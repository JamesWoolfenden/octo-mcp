# octo-mcp

A small, **read-only** [MCP](https://modelcontextprotocol.io) server for your Octopus Energy account, written in Go. Ask Claude about usage, solar export, costs and billing without opening the dashboard.

- Single static binary, stdio transport, built on the official [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).
- Only ever issues `GET` requests. Nothing it exposes can change your account.
- Meters (MPAN/MPRN and serials) are discovered from your account; nothing is hardcoded.
- Missing data is reported as missing, never as zero. A gas meter that has stopped reporting shows `no_data`.

## Tools

| Tool | What it does |
| --- | --- |
| `get_account` | Properties, meters and current/past tariff agreements. Identifiers are masked. |
| `get_consumption` | kWh for `import`, `export` or `gas`, grouped by `half_hour`, `day`, `week` or `month` (UK local time). Converts gas m³ to kWh and says so. |
| `get_rates` | Unit rates and standing charges (inc/exc VAT) for a tariff over a date range. Flux rates are labelled off-peak / day / peak. |
| `get_costs` | Consumption × matching half-hourly rate + standing charge for import, earnings for export, and a net figure. Flags partial data. |
| `check_data_gaps` | Date ranges with no readings over the last N days, per meter. The diagnostic for a meter that stopped reporting. |

Dates are `YYYY-MM-DD` (UK local; a date-only end is inclusive of that whole day) or RFC3339 timestamps (exclusive end).

## Setup

You need an API key from the Octopus dashboard: **Personal details → API access**, and your account number (`A-XXXXXXXX`).

```sh
go build -o octo-mcp .
```

Configuration is by environment variable only:

| Variable | Meaning |
| --- | --- |
| `OCTOPUS_API_KEY` | Your API key. Never logged or echoed. |
| `OCTOPUS_ACCOUNT` | Your account number, e.g. `A-XXXXXXXX`. |

### Claude Code

```sh
claude mcp add octopus --env OCTOPUS_API_KEY=sk_live_... --env OCTOPUS_ACCOUNT=A-XXXXXXXX -- /path/to/octo-mcp
```

### Claude Desktop

Add to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "octopus": {
      "command": "C:\\path\\to\\octo-mcp.exe",
      "env": {
        "OCTOPUS_API_KEY": "sk_live_...",
        "OCTOPUS_ACCOUNT": "A-XXXXXXXX"
      }
    }
  }
}
```

## Example prompts

- "What tariffs am I on, and when did they start?"
- "How much electricity did I import and export last week, day by day?"
- "What did last week cost me on Flux, including standing charge, and what did I earn from export? What's the net?"
- "How much of my import was in the off-peak band last month?"
- "What are today's Flux rates?"
- "Check all my meters for missing readings over the last 30 days."
- "Is my gas meter reporting? When was the last reading?"

If you have solar and a battery, pair this with [foxess-mcp](https://github.com/JamesWoolfenden/foxess-mcp) and its [`solar-savings` skill](https://github.com/JamesWoolfenden/foxess-mcp/tree/master/skills/solar-savings). Together they estimate what your system saves, because the grid meters alone can't see the solar you use yourself.

## Behaviour worth knowing

- **Gas units.** SMETS2 gas meters (serial starting `E6S`) report cubic metres. These are converted with `m³ × 1.02264 × 39.5 ÷ 3.6` and the result notes the conversion. Other meters are assumed to report kWh. Override with `gas_unit` (`m3` or `kwh`) if the guess is wrong.
- **Time zones.** The API speaks UTC; Flux bands and billing days are UK local time. Grouping and standing charges use `Europe/London`, so 23h and 25h clock-change days are handled (a day's expected readings are 46 or 50, not 48).
- **Rates.** Each half hour is priced at the rate in force at its start, using the tariff agreement that was active then, so a period spanning a tariff change prices correctly. Prices include VAT.
- **Data lag.** Octopus data usually arrives up to about a day late, so a gap at the very end of `check_data_gaps` may be delay rather than a fault.
- **Rate limits and pagination.** `429` and `5xx` responses are retried with backoff (honouring `Retry-After`), and paginated results are followed. `next` links are only followed on the API's own host.
- **Privacy.** Account, meter point and serial numbers are masked in tool output. Credentials stay in your MCP client's config; the server never writes them anywhere.
- **Limitations.** Single-register tariffs only (no Economy 7 style two-register tariffs).

## Development

```sh
go test ./...
```

Tests never touch the network. The API is faked with an in-process `httptest` server serving fixture responses (`testdata/`), and all identifiers in the fixtures are fake.

### Keeping secrets out of the repo

Your API key and account number come from environment variables and must never be committed. After cloning, install the [pre-commit](https://pre-commit.com) hooks, which run gitleaks, a private-key check, `go vet` and `go test` before every commit:

```sh
pre-commit install
```

The same gitleaks scan runs in CI on every push and pull request (`.github/workflows/gitleaks.yml`), so a commit that skips the local hook is still caught. `.env` files, keys and local MCP config are in `.gitignore`.

## Licence

[MIT](LICENSE)

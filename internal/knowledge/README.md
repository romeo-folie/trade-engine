# internal/knowledge — Knowledge Graph

Maintains a relationship graph of market entities (companies, sectors, commodities, macro events) and enriches it automatically from news. The graph provides structured context injected into analysis prompts to improve reasoning about supply chains, competitive dynamics, and macro exposure.

## Components

| File | Purpose |
|---|---|
| `graph.go` | Thin facade over `store` — CRUD for entities and relationships |
| `context.go` | `BuildContext()` + `GraphContext.Format()` — assembles prompt injection block |
| `enricher.go` | `Enricher` — processes news items to extract relationships via LLM |
| `seed.go` | Seed data loader for bootstrap |

## Architecture

```
News item arrives
      │
      ▼
Enricher.EnrichNewsItem(ctx, id, headline, summary)
      │
      ├── acquireRateLimit()         throttle concurrent LLM calls
      │
      ├── buildExtractionPrompt()    headline + summary → system + user prompt
      │
      ├── ExtractionCallFn(...)      pluggable LLM call (avoids circular import)
      │
      ├── parseExtractionResponse()  tolerant JSON parser (array, single object,
      │                               fenced markdown, prose prefix)
      │
      └── persistExtractedRelationship()
            ├── resolveEntityID()    upsert entity by name/ticker
            └── store.UpsertRelationship()
```

## Prompt Injection

`BuildContext(ctx, ticker, openPositions)` returns a `GraphContext` ready to be formatted into an LLM prompt:

```go
gc, err := knowledge.BuildContext(ctx, "AAPL", openPositionMap)
formatted := gc.Format() // injected into AnalysisRequest.KnowledgeContext
```

The formatted block includes:
1. Primary entity metadata (sector, country, description)
2. Direct 1-hop relationships with direction arrows (`→` outbound, `←` inbound)
3. Portfolio overlap — related tickers with open positions and current value

## CLI

```bash
te graph seed                       # seed the graph with built-in entity data
te graph entities [--type company]  # list entities (optionally filter by type)
te graph relationships <ticker>     # show relationships for a ticker
te graph review                     # review and approve/reject auto-generated relationships
```

## Relationship Types

| Type | Meaning |
|---|---|
| `supplies_to` / `supplied_by` | Supply chain edge |
| `competes_with` | Direct competitors |
| `subsidiary_of` / `parent_of` | Corporate hierarchy |
| `partner_of` | Strategic alliance |
| `sector_member` | Sector classification |
| `manufactures_for` / `manufactured_by` | OEM relationships |
| `regulates` / `regulated_by` | Regulatory exposure |
| `affects_contract` / `affected_by_event` | Event-driven edges |

## Rate Limiting

`Enricher` enforces a configurable `minInterval` between LLM extraction calls and applies automatic exponential backoff on `429` / rate-limit responses to stay within provider quotas.

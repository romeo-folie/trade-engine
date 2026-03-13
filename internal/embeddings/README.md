# internal/embeddings — Vector Embedding Pipeline

Generates vector embeddings for past analyses, news items, and entities. Embeddings are stored in `pgvector` columns and enable semantic similarity search — "find past setups that look like the current one" — which is injected into analysis prompts to improve signal quality.

## Components

| File | Purpose |
|---|---|
| `embedder.go` | `Embedder` interface + `NewEmbedder()` factory |
| `openai.go` | OpenAI `text-embedding-3-small` provider (1536 dims, raw HTTP) |
| `ollama.go` | Ollama `nomic-embed-text` provider (768 dims, raw HTTP) |
| `worker.go` | Background `Worker` — polls for NULL embeddings and fills them |

## Dimension Decision

| Provider | Model | Dimensions |
|---|---|---|
| `openai` | `text-embedding-3-small` | **1536** (default) |
| `ollama` | `nomic-embed-text` | 768 |

> **All `vector()` columns are sized to 1536.** Switching to Ollama requires an `ALTER TABLE ... ALTER COLUMN ... TYPE vector(768)` migration plus a full `te embeddings reindex --force`.

## Architecture

```
Worker (background goroutine)
  │
  ├── every worker_interval_sec: query NULL-embedding rows (analyses, news, entities)
  │
  ├── embed each row via Embedder.EmbedBatch()
  │      │
  │      ├── OpenAIEmbedder  → POST https://api.openai.com/v1/embeddings
  │      └── OllamaEmbedder  → POST http://localhost:11434/api/embeddings
  │
  └── store.UpdateEmbedding(ctx, id, vector)
```

## Similarity Search

Once embeddings are populated, the engine calls `store.FindSimilarAnalyses()` before each live analysis to retrieve the top-N semantically similar past setups. These are injected into the prompt's `SIMILAR PAST ANALYSES` section.

The embedding text for analyses is a compact indicator snapshot:

```
AAPL RSI:62.3 EMA20:183.4 MACD:1.2 ADX:28.7
```

## CLI

```bash
te embeddings status           # show counts (total, embedded, pending) per table
te embeddings reindex          # fill NULL embeddings (skips rows that already have embeddings)
te embeddings reindex --force  # clear all embeddings first, then rebuild from scratch
```

## Configuration

```yaml
embeddings:
  provider: openai                  # openai | ollama
  model: text-embedding-3-small
  api_key_env: OPENAI_API_KEY
  dimension: 1536
  worker_interval_sec: 60           # polling interval for the background worker
  batch_size: 50
  similar_analyses_enabled: true
  similar_analyses_threshold: 0.8   # cosine similarity threshold
  similar_analyses_limit: 5         # top-N results injected into prompt
```

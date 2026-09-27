Every file is an answer from `https://api.venice.ai/api/v1`. A listing keeps
whole entries and drops the rest; no field is changed.

| File | Where it came from |
|---|---|
| `models.json` | `GET /models` on 2026-09-24, four of the 123 entries, all of type `text` |
| `stream_reply.sse` | `POST /chat/completions`, streaming, `reasoning.enabled` |
| `stream_tool_calls.sse` | `POST /chat/completions` with `deepseek-v4-flash-0731`, streaming, `reasoning.enabled`, one tool offered; two calls came back |
| `stream_cache_write.sse` | `POST /chat/completions` with `openai-gpt-6-luna` on 2026-09-25, streaming with `stream_options.include_usage`, a 17 kB card as the system message and `hey` as the only message; the first request of that prompt, so its cache was written and none was read |
| `error_bad_model.json` | `POST /chat/completions` with a model id that does not exist, 404 |
| `error_unauthorized.json` | `POST /chat/completions` with a key that does not exist, 401 |
| `search.json` | `POST /augment/search` on 2026-09-27 with `{"query":"concertos em Lisboa esta semana","limit":5}` |
| `scrape.json` | `POST /augment/scrape` on 2026-09-27 with the address of the first page `search.json` found |
| `error_scrape_blocked.json` | `POST /augment/scrape` on 2026-09-27 with `https://www.reddit.com/r/lisboa/`, 400 |

`GET /api_keys/rate_limits` has no fixture: it answers with the account's own
balance.

On 2026-09-18 `GET /models?type=all`, and on 2026-09-24 `GET /models`, answered
with `cache-control: no-store, no-cache, private, must-revalidate`, which is why
no listing is kept between runs.

Every file is an answer from `https://api.venice.ai/api/v1`. A listing keeps
whole entries and drops the rest; no field is changed.

| File | Where it came from |
|---|---|
| `models.json` | `GET /models` on 2026-09-24, four of the 123 entries, all of type `text` |
| `stream_reply.sse` | `POST /chat/completions`, streaming, `reasoning.enabled` |
| `stream_tool_calls.sse` | `POST /chat/completions` with `deepseek-v4-flash-0731`, streaming, `reasoning.enabled`, one tool offered; two calls came back |
| `error_bad_model.json` | `POST /chat/completions` with a model id that does not exist, 404 |
| `error_unauthorized.json` | `POST /chat/completions` with a key that does not exist, 401 |

`GET /api_keys/rate_limits` has no fixture: it answers with the account's own
balance.

On 2026-09-18 `GET /models?type=all`, and on 2026-09-24 `GET /models`, answered
with `cache-control: no-store, no-cache, private, must-revalidate`, which is why
no listing is kept between runs.

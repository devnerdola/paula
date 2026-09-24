Every file is an answer from `https://openrouter.ai/api/v1`. A listing keeps
whole entries and drops the rest; no field is changed, apart from the account
id noted below.

| File | Where it came from |
|---|---|
| `models.json` | `GET /models`, five entries |
| `endpoints.json` | `GET /models/deepseek/deepseek-v4-pro-0813/endpoints`, three endpoints |
| `stream_reply.sse` | `POST /chat/completions`, streaming, `reasoning.enabled` |
| `stream_tool_calls.sse` | `POST /chat/completions` with `deepseek/deepseek-v4-flash-0731`, streaming, `reasoning.enabled`, one tool offered; two calls came back |
| `error_bad_model.json` | `POST /chat/completions` with a model id that does not exist, 400; the `user_id` the answer carries is dropped |
| `error_unauthorized.json` | `POST /chat/completions` with a key that does not exist, 401 |

`GET /key` has no fixture: it answers with the account's own spending.

On 2026-09-18 `GET /models` answered with `cache-control: private, no-store`
and no entity tag, which is why no listing is kept between runs.

An error written into a stream has none either. It is what a host sends when it
fails after OpenRouter answered 200, and every cause of it is the host's own:
it disconnects, times out, is overloaded, or its filter stops the text it was
already sending. None of that can be asked for, and a transcription of the
documented shape would only say that Paula reads what was transcribed. The
reading itself is held to an error in a stream by
`TestAnErrorWhereAChunkBelongs`, beside the client that does it.

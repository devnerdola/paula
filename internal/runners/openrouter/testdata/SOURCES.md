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
| `images_models.json` | `GET /images/models` on 2026-09-28, five of the 55 entries |
| `models_gemini_image.json` | `GET /models` on 2026-09-28, the entry of `google/gemini-3.1-flash-image`, one of the nine models that `GET /images/models` lists as well |
| `images.json` | `POST /images` on 2026-09-28 with `google/gemini-3.1-flash-image`, `aspect_ratio: 3:4`, `resolution: 512`, the prompt `The same apple, cut in half, on the same table`, and as the one `input_references` entry a picture the same model had painted from `A red apple on a wooden kitchen table, morning light` |
| `error_images_bad_model.json` | `POST /images` on 2026-09-28 with a model id that does not exist, 404 |

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

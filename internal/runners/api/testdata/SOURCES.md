Both files are request bodies Paula sent to
`https://openrouter.ai/api/v1/chat/completions` on 2026-09-19, taken from
`paula turns -dump` of a scratch conversation with the card in
`internal/cmd/testdata/ada.yaml`. Each was answered 200 and streamed a reply.
The bytes are as they were sent; no field is changed.

| File | Where it came from |
|---|---|
| `chat_request.json` | the reply to one line of text, `deepseek/deepseek-v4-pro-0813` |
| `chat_request_image.json` | the caption of `internal/media/testdata/red-blue-8x4.png`, scaled up, `~deepseek/deepseek-flash-latest` |

They are what `ReadPrompt` is read against, so a body Paula once sent stays
readable by `paula turns` however the body it sends today is built.

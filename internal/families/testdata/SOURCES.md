Every file is the ids a host listed on 2026-09-25, one per line, in the order
it listed them. Neither listing needs a key.

| File | Where it came from |
|---|---|
| `openrouter-ids.txt` | `curl -sS https://openrouter.ai/api/v1/models \| jq -r '.data[].id'` |
| `venice-ids.txt` | `curl -sS https://api.venice.ai/api/v1/models \| jq -r '.data[].id'` |

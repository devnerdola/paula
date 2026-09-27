Every file is an answer from `https://api.tavily.com`, captured on 2026-09-27.
No field is changed.

| File | Where it came from |
|---|---|
| `search.json` | `POST /search` with `{"query":"concertos em Lisboa esta semana","max_results":5,"include_published_date":true}` |
| `extract.json` | `POST /extract` with the address of the first page `search.json` found |
| `extract_failed.json` | `POST /extract` with `https://www.reddit.com/r/lisboa/`; Tavily answers 200 and names the page among the failed results |
| `error_unauthorized.json` | `POST /search` with a key that does not exist, 401 |
| `error_invalid.json` | `POST /search` with `"max_results":"five"`, 422 |
| `error_invalid_item.json` | `POST /search` on 2026-09-27 with `"include_domains":["timeout.pt",7]`, 422; the field is named with the position of the item in the list |

`GET /usage` has no fixture: it answers with the account's own credits.

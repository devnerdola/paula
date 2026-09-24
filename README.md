# Paula

Paula is a texting companion. You describe a character in a small YAML file,
point her at a hosted model, and she keeps one long conversation with you in a
terminal. The conversation lives in a SQLite database on your machine. So do
the pictures you send her, and a log of every request behind her replies.

She is a single Go binary with no services behind it. What she costs is what
the model API charges.

## Getting started

You need Go 1.26 or later and an API key for OpenRouter or Venice.

```
go build
export OPENROUTER_API_KEY=...
```

Write `paula.yaml` next to the binary:

```yaml
persona: personas/paula.yaml

runners:
  openrouter:
    type: openrouter

models:
  pro:
    runner: openrouter
    id: deepseek/deepseek-v4-pro-0813

default_models:
  chat: pro

frontends:
  repl:
```

One model is the smallest she runs on: the one that writes her replies.

`paula.example.yaml` is a fuller one, with two runners and a model for each.

Then write the character in `personas/paula.yaml`. The quickest way is to copy
the example card and edit it:

```
cp personas/paula.example.yaml personas/paula.yaml
```

The Paula in it is invented. The character, her life and the way she writes are
made up for the example, and she is nobody real. A card can be as small as
this:

```yaml
name: Paula
user:
  name: Caio
personality:
  - She is warm, and writes the way people text.
```

Check that the model is reachable and that the card reads the way you want:

```
./paula models
./paula persona-check
```

Both exit with 1 and explain the problem if anything is wrong.

## Talking to her

Start the process that holds the conversation, and open a terminal on it:

```
./paula serve
./paula repl
```

Type to her and she answers, word by word as the model writes it. She waits two
seconds before she starts, so several quick messages become one reply.

| Input | Effect |
|---|---|
| `/image PATH [TEXT]` | sends a picture, with an optional line of text |
| `/models` | shows the model behind each role |
| `/model chat NAME` | switches to another model and remembers it |
| `/models reset` | forgets those choices |
| `/summary` | what she was told of the conversation before the messages she still carries |
| `/memory [QUERY]` | the ten newest memories, or the ten a question is about |
| `/forget ID` | takes a memory away, and the ones it replaced |
| `/stop` | stops the reply she is writing |
| `/help` | lists all of this |
| `/quit` | closes the terminal |
| Ctrl-C | stops the reply; press it twice to close the terminal |

Open `paula repl` in as many terminals as you like. They all show the same
conversation, and each of them shows what you typed in the others.

You can also pipe a file to her:

```
./paula repl < questions.txt
```

She answers one line at a time, and the terminal exits when the file ends.

`serve` runs until you stop it. Stop it while she is writing and that reply is
thrown away; the next `serve` answers the message you were waiting on.

## Looking at what happened

`paula turns` lists what she did, newest first, with the tokens and the cost of
each: a reply, a fold of the oldest of the conversation, or a summary written
again.

```
./paula turns
./paula turns -n 50       # the latest 50 rather than the latest 20
./paula turns 42          # everything about one reply
./paula turns -dump 42    # the raw requests and responses of that reply
```

`turns 42` shows which messages the reply answered, the prompt the model was
given, whether it was asked to reason and at which effort, and the reply
itself. A reply that asked for tools lists each call, the request of the round
that asked for it, and why it failed if it did. `-dump` adds the headers and
the exact bytes in both directions, and what each call was asked with and
answered, which is what you want when an API behaves strangely.

`paula memory` is what she remembers. It reads and writes the conversation a
`serve` is holding, so it runs beside one or on its own:

```
./paula memory list                    # the newest, with the number each is forgotten by
./paula memory list -n 50              # as many as you ask for
./paula memory search where does Ana live   # the ones that hold its words
./paula memory search -n 3 Ana         # the few it says most about
./paula memory forget 7                # takes it away, and the ones it replaced
```

None of them asks anything of a model. A search finds memories by their words,
as hers does, which How she works describes. Forgetting is about what
she carries: the messages a memory was read from, and the summary, stay where
they are.

`paula models` prints what each runner says about the models you configured.
`paula models -available` lists everything the runners offer, which is how you
find the id of a model to configure. Both read every runner's listing from its
API. Nothing of a listing is kept between runs: both APIs answer it `no-store`.

Every command takes `-config FILE`, `-log-level` (`debug`, `info`, `warn`,
`error`), `-log-format` (`text` or `json`) and `-v`, which is short for
`-log-level info`. `paula help` lists the commands, and `paula help COMMAND`
its flags.

## Configuration

Paula reads `paula.yaml` from the working directory. Use `-config FILE` or
`$PAULA_CONFIG` for another path. Paths inside the file are relative to the
file itself, and `~/` is your home directory. A frontend's `socket` is the one
exception: it is relative to `data_dir`.

She rejects keys she does not know, and reports every problem in the file at
once, each one under its key path. Anchors and merge keys work, so models can
share settings.

| Key | Default | Meaning |
|---|---|---|
| `persona` | — | the character card; required |
| `data_dir` | `data`, beside the file | the database and the images, created with mode 0700 |
| `runners` | — | the APIs she talks through; at least one |
| `models` | — | the models she may use, in the order you write them |
| `default_models` | — | the model for each role; `chat` is required |
| `engine` | see below | how she replies |
| `frontends` | none | how you reach her |
| `tools` | none | what she can do in the middle of a reply |

### Runners

A runner is one API. Its settings are the defaults for its models, and a model
can override any of them.

| Key | Default | Meaning |
|---|---|---|
| `type` | — | `openrouter` or `venice` |
| `url` | the API's own | base URL |
| `token_env` | `OPENROUTER_API_KEY` or `VENICE_API_KEY` | the environment variable with the key |
| `idle_timeout` | `2m` | how long a request may go without a byte |
| `request_timeout` | `3m` | the whole a request that is not a reply may take |
| `retries` | `2` | how often at most a request is sent again, when the API answers with a status that asks for it |
| `provider` | none | settings that only this API documents |

The key is never written to a log, and never appears in the request log either.

The two timeouts answer different questions, and neither shortens the other.
`idle_timeout` cuts any request that goes quiet. `request_timeout` is the whole
a listing or a key check may take, however steadily it arrives; a reply has no
such bound, since it arrives for as long as she is writing.

`request_timeout` is generous because a catalogue is usually instant and
occasionally stalls: OpenRouter answers its own in under a second most of the
time, and now and then takes minutes. Waiting costs nothing while an API is
healthy, and a run that cannot read a listing has no host to talk to anyway.

A request whose connection drops before any status arrives is reported, not
sent again: nothing says the host did not take it.

**OpenRouter** serves `https://openrouter.ai/api/v1`. Paula reads its catalogue
from `GET /models` and checks the key with `GET /key`. She sends `408`, `429`,
`502` and `503` again after the wait `Retry-After` asks for, at most `retries`
times, and gives up on a wait longer than two minutes.

| `provider` setting | Request field |
|---|---|
| `routing.order`, `.only`, `.ignore`, `.allow_fallbacks`, `.require_parameters`, `.data_collection`, `.zdr`, `.enforce_distillable_text`, `.quantizations`, `.sort`, `.preferred_min_throughput`, `.preferred_max_latency`, `.max_price` | the `provider` object |
| `reasoning.max_tokens` | `reasoning.max_tokens` |
| `sampling.top_a`, `sampling.logit_bias` | `top_a`, `logit_bias` |
| `cache.control` | `cache_control` |
| `service_tier` | `service_tier` |

`routing.only` pins a model to certain hosts. At startup Paula checks that one
of them accepts every parameter she sends and serves the context you asked for.
A base name covers its variants, so `novita` matches `novita/fp8`.

Most models cache a prompt without being asked. Claude, and OpenAI's models
from GPT-5.6 on, write a cache only where a request marks it, and
`cache.control` is what marks it. Set on such a model, every request carries
it three times. A top-level `cache_control` marks the end of the prompt, where
the rounds of a reply find what the round before them wrote. What she
remembers and what time it is now change from one reply to the next, so the
last message before them is marked too, which is usually her reply, and so is
the last message she was sent before it, since OpenAI takes a marker only on a
message the model was given. The next reply finds everything up to the later
of the two on Claude, and up to the earlier on OpenAI:

```yaml
models:
  opus:
    runner: openrouter
    id: anthropic/claude-opus-5.5
    provider:
      cache:
        control:
          type: ephemeral
          ttl: 1h
```

`type` is `ephemeral`, and `ttl` is `5m` or `1h`. Reading the cache costs a
tenth of the input price; writing it costs 1.25 times the input price with
`5m` and twice with `1h`, and each reply writes only what is new since the one
before. Texts are often more than five minutes apart, which a `5m` cache does
not outlast, so `1h` is the one for a conversation. On OpenAI the `ttl` is
dropped: a cache lasts at least 30 minutes, reading it costs a tenth of the
input price and writing it 1.25 times. Neither caches anything shorter than
the minimum of its model, so a conversation that has just begun may show none.

**Venice** serves `https://api.venice.ai/api/v1`. Paula reads its catalogue
from `GET /models?type=all` and checks the key with `GET
/api_keys/rate_limits`. She sends `429` again at the time in
`x-ratelimit-reset-requests`, at most `retries` times. She keeps Venice's own
system prompt off unless `provider.system_prompt` turns it on, and wants
`sampling.seed` above zero.

Venice labels each model with a privacy. A `private` model runs where nothing
of a request is kept. An `anonymized` one, such as Claude, is passed to the
company that makes it with nothing that names you, and that company reads the
prompt. The catalogue page of each model says which it is.

| `provider` setting | Request field |
|---|---|
| `system_prompt`, `character`, `e2ee` | `venice_parameters` |
| `sampling.min_temperature`, `sampling.max_temperature` | `min_temp`, `max_temp` |
| `output.stop_token_ids`, `output.verbosity` | `stop_token_ids`, `verbosity` |
| `cache.retention` | `prompt_cache_retention` |
| `fallbacks` | `fallbacks`, up to 10 models the catalogue serves |
| `search.provider` | `search_provider` of the web search |

Venice publishes no list of accepted parameters, so a sampling setting is held
against nothing there. Only what a model can do is checked.

### Models

A model needs a `runner` and the `id` that runner knows it by. It may also set
`context`, and any of the settings below.

Without a `context`, Paula uses the largest the catalogue reports. The number
is never sent to the API. It is what `paula models` shows, what a host pinned
with `routing.only` is held against, and what a prompt is held to. Where
neither the file nor the catalogue gives one, a prompt is held to nothing.

`paula models` holds each model against the catalogue: the runner serves it,
its reasoning settings fit what it does, and it accepts every parameter she
would send.

### Settings

A setting can be written on a runner, where it becomes the default for every
model of that runner, or on a model, which overrides the runner key by key.

A sampling or output setting you leave out is not sent at all, so the model
uses its own default. Reasoning is the exception: Paula always sends it, and
takes it from the catalogue when you say nothing.

| Setting | Sent as | Accepted |
|---|---|---|
| `sampling.temperature` | `temperature` | 0 to 2 |
| `sampling.top_p` | `top_p` | 0 to 1 |
| `sampling.top_k` | `top_k` | 0 or more |
| `sampling.min_p` | `min_p` | 0 to 1 |
| `sampling.repetition_penalty` | `repetition_penalty` | 0 or more |
| `sampling.presence_penalty` | `presence_penalty` | -2 to 2 |
| `sampling.frequency_penalty` | `frequency_penalty` | -2 to 2 |
| `sampling.seed` | `seed` | any whole number; Venice wants it above zero |
| `output.max_tokens` | `max_tokens` | above zero |
| `output.stop` | `stop` | at most 4 sequences |
| `reasoning.mode` | the reasoning object of the API | `on` or `off` |
| `reasoning.effort` | the same | `minimal`, `low`, `medium`, `high`, `xhigh`, `max` |
| `reasoning.summary` | the same | `auto`, `concise`, `detailed` |

A model that can reason is asked to, at the middle of the efforts its catalogue
lists. One that cannot is asked not to. Write `mode: off` yourself to turn
reasoning off on a model that could use it.

Each API documents settings the other does not. Those live in the `provider`
block of its runner, and are listed with each runner above.

### Roles

| Role | Needs a model that | Required |
|---|---|---|
| `chat` | chats and accepts tools | yes |
| `vision` | sees images | no |

Without a `vision` model, pictures reach the chat model only if that model can
see them itself.

A reply is offered the tools the file names, so the chat role asks for a model
that takes them.

### Engine

| Key | Default | Meaning |
|---|---|---|
| `debounce` | `2s` | how long a message waits before she starts writing |
| `prefill_cancel` | `true` | a new message restarts a reply that has written nothing yet |
| `system_ratio` | `0.5` | share of the context the system message may take; above 0 and below 1 |
| `memory_ratio` | `0.5` | share of what is left of the system message, once the card is written, that memories may take; the summary takes the rest; above 0 and at most 1 |
| `history_keep` | `0.5` | share of the messages' half of the context a fold leaves behind; above 0 and below 1 |
| `image_messages` | `2` | how many of the latest messages that carry a picture send it as a picture |
| `image_max_px` | `1024` | longest side of a stored image; `0` keeps it as it is |
| `log_keep` | `500` | how many entries keep the bodies of their requests; a fold is an entry of its own |
| `tool_rounds` | `3` | how many rounds of tool calls a reply may take, at least 1; the round after them is asked for an answer with no call in it |

### Frontends

A frontend is a way of reaching her. List as many as you like: they all show
the same conversation, and each shows what you said on the others.

**`repl`** listens on a Unix socket: `paula.sock` inside `data_dir`, or the
`socket` you name. That path is relative to `data_dir`, not to the
configuration file.

**`telegram`** is a bot you text. Talk to `@BotFather` to make one, and tell
Paula the token and who she is talking to:

| Key | Default | Meaning |
|---|---|---|
| `token_env` | `TELEGRAM_TOKEN` | the environment variable with the bot token |
| `user_id` | — | required: the number of the one person served |
| `stream_edits` | `false` | show a reply as it is written, by writing it over the message it started as |

```yaml
frontends:
  repl:
    socket: /tmp/paula.sock
  telegram:
    user_id: 123456789
```

A bot is reachable by anyone who finds it, and this conversation is with one
person: a message from anyone else is logged and left alone. Send her text,
photos, stickers or an image as a file; a sticker is read as its emoji, with
the picture.

What she writes as separate paragraphs arrives as separate texts, each sent as
she finishes writing it, with the typing status up in between — she texts the
way a person does. A text longer than Telegram takes carries on in another.
`/models` and the rest are offered by the client as you type them, and the
models menu is buttons to tap.

`stream_edits` lets you watch her write: a text appears as soon as she has
started it and is written over as it fills, about once a second, instead of
arriving finished. It changes nothing about what the texts are — the same
paragraphs land as the same messages either way.

Your `user_id` is the number Telegram knows you by, which `@userinfobot` will
tell you. The run keeps the last update it answered in `telegram.offset` beside
the database, so a run that follows does not answer it again.

**`web`** is a page you open in a browser. Point it at the address, sign in
with the token, and the conversation is there: her texts as she writes them,
pictures either way, the models menu as buttons, and the rest of the commands
as they are typed. The page is served out of the binary, so there is nothing
to install and nothing is fetched from anywhere.

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:8484` | the address to listen on |
| `token_env` | `PAULA_WEB_TOKEN` | the environment variable with the token |

```yaml
frontends:
  web:
    listen: 127.0.0.1:8484
```

The token is asked for as an ordinary sign-in and goes in either box, so a
browser remembers it. Make it long: at least eight characters, and without a
colon or a space. Anyone who reaches the address and has the token reads the
whole conversation, so listen on `127.0.0.1` unless something in front of it
is doing the letting in.

Every browser that opens the page gets a session of its own, and they show each
other what is typed. A message she writes arrives text by text, as it does
everywhere else, and the one she is in the middle of fills as she writes it.
Scrolling up reads further back.

On a page the browser calls secure — `https`, or `localhost` — a bell asks
whether to notify you. While the page is open but not being looked at, the
first text of a reply to something you sent from it comes as a notification,
and clicking it brings the page back.

### Tools

A tool is something she can do in the middle of a reply: she asks for it, it
runs, and she writes on with what it answered. Only the kinds listed under
`tools` are offered. While one runs, the frontend says what she is doing, such
as `searching memories for Ana`.

**`memory`** reaches what she remembers.

| Tool | What she does with it |
|---|---|
| `search_memories` | looks for memories by the words they hold, in the card's language, past the newest ones her prompt carries; each comes with its number |
| `remember` | keeps a lasting fact about you or about her as soon as it is said, rather than when a fold reaches it, in place of the memories she found it updates |
| `forget_memory` | takes a memory away by its number, and the ones it replaced, when you ask her to |

| Key | Default | Meaning |
|---|---|---|
| `results` | `10` | the most memories a search answers with; at least 1 |

```yaml
tools:
  memory:
    results: 10
```

A memory she keeps is dated by the message she was answering, and a search
finds it at once. The memories it replaces must still stand: a number that
names none keeps nothing, and she is told so.

## The character card

The card is the whole character. Every field is optional except `name` and
`user.name`.

| Key | What it does |
|---|---|
| `name` | what she is called |
| `user` | `name`, and optionally `nickname` and `facts` about you |
| `language` | the language she writes in; English by default |
| `background`, `appearance`, `personality`, `speech`, `scenario`, `rules` | lists of statements, one per line |
| `examples` | pairs of `user` and `reply` that show her voice |
| `id` | the name of her prompt cache on the API; the file name by default |
| `prompt` | your own template, instead of the built-in one |

Write `{{char}}` and `{{user}}` anywhere in the card. The two names are filled
in wherever they turn up in the finished message.

`prompt` is a Go template over the card, so `{{.User.Facts}}` and the rest of
it are yours to use, and a stray `{{` is reported as the parse error it is.
`paula persona-check` prints the finished system message.

The lists are descriptive, so each item is a separate line:

```yaml
speech:
  - She writes in lower case, in short bursts.
  - She never explains herself twice.
```

## How she works

**One reply at a time.** A message starts a two-second timer; anything else you
type restarts it, so a burst becomes one reply. If a message arrives while she
is writing, it restarts that reply, as long as nothing of it has been sent yet.
Stopping keeps what she had written and marks it interrupted.

**Failures wait for you.** If a request fails, she says so and the message
stays unanswered. Your next message, or the next `serve`, picks it up. Nothing
is answered twice: every stored reply records which messages it answered.

A reply that has already run a tool is the exception. What the tool did stands,
and asking again would do it again, so a failure after it ends the reply the
way a stop does: what she had written is kept, the error is said below it, and
the message counts as answered.

**The prompt is the conversation.** It opens with a system message: the card,
then the summary of what came before. After it come the messages the summary
does not cover, in order, each reply after what it answers. What she remembers
is a system message of its own, just before the time of the message she is
answering: it changes whenever she keeps or forgets something, and a host that
cached the prompt still has everything before it when it does. It takes its
share of the system message's room, as the summary does.

Each reply goes back with what she thought on the way to it, as the API sent
it: the reasoning text, and on OpenRouter the details it sent beside it, which
may be signed and so go back exactly as they came. A reply that ran tools goes
back as one message, the round that wrote it, so the details are that round's;
what the rounds before it thought went back with their calls. A model offered
tools reads the thinking of every earlier reply, and one shown replies with no
thinking learns to skip its own, or to leave it open and write the reply inside
it. That thinking is part of the prompt, so it is counted against the context
and folded away with the messages it belongs to.

**The oldest of it is folded away.** The model's `context`, or the largest its
catalogue reports, is split between that system message and the messages —
`system_ratio` says how. When the messages outgrow their half, a fold runs in
the background: it takes the oldest exchanges, asks the chat model for the
lasting facts in them and for the summary written again with them added, and
stores both together. It steps until what is left is `history_keep` of that
half. Replies carry on while it works, and a fold that fails waits 30 seconds
before the next try, doubling up to ten minutes.

A memory is one sentence about you or about her, dated by the day it was said.
Memories take `memory_ratio` of what is left of the system message once the
card is written, newest first; the summary takes the rest. Nothing is thrown
away: the conversation itself keeps every message, and `paula turns` shows
every fold and what it asked.

**A summary past its room is written again.** Folding adds to it, so it grows.
When it no longer fits what is left of the system message, she writes it again
from itself, with nothing added, and it goes on covering the same messages.
That follows a fold, in the same background work, and runs on its own when the
messages are not due one. One that comes back no
shorter is dropped, so the summary it was written from stands, and it waits
like a failure rather than asking the same thing again.

A card big enough to fill `system_ratio` of the context on its own leaves
nothing for either: no memory is ever shown and the summary is never written
again, however long it grows. `serve` says so at startup, at `warn`, with the
card's size and the share it filled. It still runs — the prompt holds itself
to the context by leaving the oldest exchanges out — but raise `system_ratio`,
give the model a larger `context`, or write a shorter card.

**Memories are found by their words.** SQLite keeps an index of the words each
memory holds, and a search returns the ones that hold any word of the question,
those the words say most about first: a word few memories hold says more than
one most of them do. A word finds its other forms, so "sisters" finds a memory
about a sister, but not another word for the same thing: "bike" does not find
"bicycle". A question about nothing she remembers finds nothing.

Every memory names who it is about, you or her, so a name finds all of them. A
search looks past the two names, and past the single letters an apostrophe
leaves of a word, as the s of "Caio's". A question of nothing but names is
answered with why it finds nothing.

**What still does not fit is left out.** Past the context, the oldest exchanges
of the prompt are dropped — the messages you sent since her previous reply, and
that reply, go together, so she is never shown an answer without the messages
it answered. What she is answering now is sent whatever it takes.

Measuring means counting tokens, which only the host can do exactly. Paula
starts at a token every 3.5 characters, which counts a little high, and
corrects it per model from the count an answer comes back with.

A picture is counted the same way. A host bills one by how big it is, and each
host by its own reckoning — tiles for one, pixels for another — so there is no
number that is right for all of them and none to set in the file. Paula starts
at 1000 tokens a picture, which is high for the size she stores them at, and
reads what one really costs off the same count: a prompt that carried pictures
says nothing about characters, since the pictures are in the count and not in
them, but what is left of that count once the characters are paid for is what
the pictures came to.

Both numbers are the model's own, and both are what a fold weighs an exchange
by, since what a fold decides is what a prompt can carry.

**The time is told, not written into a message.** Before each message of yours
stands a system message of its own: `The next message was sent at Saturday, 19
September 2026, 09:06 UTC+02:00.`, and `It is now …` before the last one. A
model writes like the messages it reads, and a small one that reads a time
inside the message it is answering starts stamping its own replies with one. As
a message of its own it is read rather than copied, it needs no notation to
explain, and since only the one before your latest message ever changes, what a
host has cached of everything earlier still stands.

A host keeps a prompt on the machine that read it, so the requests of one
conversation name it: `prompt_cache_key` on both APIs, which Venice routes by,
and on OpenRouter also `session_id`, which is what keeps them on one host. The
name is the card's `id` and what the request is for. `paula turns` shows how
much of each prompt a host had cached.

**Pictures are described once.** She accepts JPEG, PNG, GIF and WebP up to 64
megapixels. Each one is scaled to `image_max_px`, turned upright by its EXIF
orientation and kept as JPEG in `media/`. The `vision` model describes a
picture the first time it is used, and that description stays in the
conversation for good. Recent pictures also go to the chat model itself, when
it can see them.

**Slow APIs are visible.** Any request still waiting after five seconds is
logged at `warn`, which the default level shows, with its runner and its URL.
Before `serve` checks the models it says so, at `info`, so `-v` shows it. A
listing that takes longer than `request_timeout` is given up on, and the error
names the runner.

**Terminals follow an event log.** Every terminal reads the same numbered
stream of events, which keeps the latest 4096. One that falls further behind,
or that opens later, reads the conversation from the database instead, and
shows only what it has not shown.

**Every turn is recorded.** Each reply is an entry, and an entry holds the
requests it made: the reply itself, and a look at any picture she was sent.
Each request keeps its headers, bodies, timings, tokens and cost, and shows a
dash for a cost the host did not report. Bodies older than the latest
`log_keep` entries are dropped, and the rest stays. `paula turns` is the window
into it.

## Your data

```
data/
  paula.db         the conversation, what she remembers of it, the settings,
                   the request log
  paula.db-wal     the write-ahead log SQLite keeps beside it
  paula.db-shm     the shared memory it keeps with it
  media/xx/        every picture, as JPEG named by its sha256, under the
                   first two characters of that name
  paula.sock       the socket a terminal connects to
  paula.lock       held by the running serve
```

The database is SQLite in WAL mode, with `STRICT` tables and foreign keys on,
stamped with `PRAGMA application_id = 0x5041554C`. Paula refuses a database she
did not create, and one written by a newer schema than the build knows, without
touching either. An older one is brought up to date by `serve`. A command that
only reads says to run `serve` first, rather than read a schema it does not
know. Only one `serve` may use a directory at a time.

Nothing leaves the machine except the requests to the model API. Those carry
the prompt, the pictures and the settings. A search of her memories runs in the
database and sends nothing.

## Development

```
go fix ./... && go build ./... && go vet ./... && gofmt -l . && go mod tidy -diff && go test -race ./...
```

`go fix` is the one that writes rather than reports: it says what the standard
library now has a shorter way of saying, and applies it, so what it changes
belongs to the commit it ran in.

The tests run without a network: every runner test answers from captured API
responses in `testdata`, and `SOURCES.md` in each of those directories says
which request each file came from.

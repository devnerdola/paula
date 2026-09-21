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
  vectors:
    runner: openrouter
    id: openai/text-embedding-3-small

default_models:
  chat: pro
  embed: vectors

frontends:
  repl:
```

Two models is the smallest she runs on: one writes her replies, and one turns
what she remembers into vectors, so she can find a memory by what it means. A
model that embeds writes nothing, so it is always a second model.

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
each: a reply, a fold of the oldest of the conversation, a summary written
again, or memories turned into vectors.

```
./paula turns
./paula turns -n 50       # the latest 50 rather than the latest 20
./paula turns 42          # everything about one reply
./paula turns -dump 42    # the raw requests and responses of that reply
```

`turns 42` shows which messages the reply answered, the prompt the model was
given, whether it was asked to reason and at which effort, and the reply
itself. `-dump` adds the headers and the exact bytes in both directions, which
is what you want when an API behaves strangely.

`paula memory` is what she remembers. It reads and writes the conversation a
`serve` is holding, so it runs beside one or on its own:

```
./paula memory list                    # the newest, with the number each is forgotten by
./paula memory search where does Ana live   # the ones a question is about
./paula memory forget 7                # takes it away, and the ones it replaced
```

Searching asks the `embed` model what the words mean, so it needs one in the
file; listing and forgetting ask nothing of a model. Forgetting is about what
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
| `default_models` | — | the model for each role; `chat` and `embed` are required |
| `engine` | see below | how she replies |
| `frontends` | none | how you reach her |

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
from `GET /models` and `GET /embeddings/models`, which are listed apart, and
checks the key with `GET /key`. She sends `408`, `429`,
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

**Venice** serves `https://api.venice.ai/api/v1`. Paula reads its catalogue
from `GET /models?type=all` and checks the key with `GET
/api_keys/rate_limits`. She sends `429` again at the time in
`x-ratelimit-reset-requests`, at most `retries` times. She refuses a model
whose `privacy` is not `private`, keeps Venice's own system prompt off unless
`provider.system_prompt` turns it on, and wants `sampling.seed` above zero.

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
| `embed` | turns text into a vector | to serve, and to search |

Without a `vision` model, pictures reach the chat model only if that model can
see them itself.

An `embed` model is what memories are searched by. A model that embeds writes
nothing, so it is a model of its own: OpenRouter lists them apart from the rest
and Paula reads both listings, and Venice marks them in the one it serves.
`paula serve` asks for one before it starts, since the conversation embeds what
each fold writes; so does `paula memory search`, which asks it what the words
you are looking for mean. Every other command reads the memories without
searching them, and runs on a file that names none.

The chat role asks for tools although no request carries any yet. Tools are
part of the work ahead, and a model that cannot take them is not worth
configuring for it.

### Engine

| Key | Default | Meaning |
|---|---|---|
| `debounce` | `2s` | how long a message waits before she starts writing |
| `prefill_cancel` | `true` | a new message restarts a reply that has written nothing yet |
| `system_ratio` | `0.5` | share of the context the system message may take; above 0 and below 1 |
| `memory_ratio` | `0.5` | share of what is left of the system message, once the card is written, that memories may take; the summary takes the rest; above 0 and at most 1 |
| `history_keep` | `0.5` | share of the messages' half of the context a fold leaves behind; above 0 and below 1 |
| `image_turns` | `2` | how many recent messages send their picture as a picture |
| `image_max_px` | `1024` | longest side of a stored image; `0` keeps it as it is |
| `image_tokens` | `1000` | tokens counted for each picture sent as a picture; set it to what the host bills for one |
| `log_keep` | `500` | how many replies keep the bodies of their requests |

### Frontends

`repl` is the only one. It listens on a Unix socket: `paula.sock` inside
`data_dir`, or the `socket` you name. That path is relative to `data_dir`, not
to the configuration file.

```yaml
frontends:
  repl:
    socket: /tmp/paula.sock
```

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

**The prompt is the conversation.** It opens with one system message: the card,
then what she remembers, then the summary of what came before. After it come
the messages the summary does not cover, in order, each reply after what it
answers.

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

**Memories are found by what they mean.** The `embed` model turns each of them
into a vector, a hundred at a time, in a piece of background work of its own —
so a question finds the memory it is about without sharing a word with it. A
vector is kept under the model that made it: change the model and they are made
again, rather than measured against a space they were never in.

The two run beside each other, each waiting its own wait after a failure of its
own. A host that is away for the model that embeds, or merely slow with a long
backlog, does not hold up the fold, which is what keeps the prompt inside the
context; and a fold that fails does not stop the memories from being embedded.

**What still does not fit is left out.** Past the context, the oldest exchanges
of the prompt are dropped — the messages you sent since her previous reply, and
that reply, go together, so she is never shown an answer without the messages
it answered. What she is answering now is sent whatever it takes.

Measuring means counting tokens, which only the host can do exactly. Paula
starts at a token every 3.5 characters, which counts a little high, and
corrects it per model from the count an answer comes back with. A prompt that
carried a picture corrects nothing, since the picture is in the count and not
in the characters. Every picture sent as a picture counts `image_tokens`.

**The time is told, not written into a message.** Before each message of yours
stands a system message of its own: `The next message was sent at Saturday, 19
September 2026, 09:06 UTC+02:00.`, and `It is now …` before the last one. A
model writes like the messages it reads, and a small one that reads a time
inside the message it is answering starts stamping its own replies with one. As
a message of its own it is read rather than copied, it needs no notation to
explain, and since only the one before your latest message ever changes, what a
host has cached of everything earlier still stands.

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
dash for a cost the host did not report. What a request turned into vectors is
kept, and the vectors themselves are not: they are megabytes of numbers, and
the conversation holds them already. Bodies older than the latest `log_keep`
entries are dropped, and the rest stays. `paula turns` is the window into it.

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
the prompt, the pictures and the settings, and — to the model that embeds — the
memories a fold wrote, which are the conversation in the words it left them in.
On OpenRouter the `routing` settings of the runner go with them, so where a
reply may be routed and what a host may keep of it holds for the memories too.
Venice takes no such settings on that endpoint.

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

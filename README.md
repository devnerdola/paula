# Paula

Paula is a texting companion. You describe a character in a small YAML file,
point her at a hosted model, and she keeps one long conversation with you in a
terminal. The conversation lives in a SQLite database on your machine. So do
the pictures you send her, the photos she sends you, and a log of every request
behind her replies.

She is a single Go binary with no services behind it. What she costs is what
the model API charges.

## Getting started

You need Go 1.26 or later on macOS or Linux, and an API key for OpenRouter or
Venice. The terminal talks to `serve` over a Unix socket, and `serve` holds
its data directory with a Unix lock, so nothing here runs on Windows. A build
for Linux on amd64 is on the `rolling` release of the repository on GitHub,
made from the latest commit: it is not a release, and changes with every
commit.

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
./paula models list
./paula persona check
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
| `/stop` | stops the reply she is writing, or answers with nothing the messages a failed reply left |
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

Every command past `serve`, `repl`, `version` and `help` is a noun with verbs,
and the verbs are the same words everywhere: `list` for a table, `show ID` for
one thing in full, and a plain word for what changes something.

`paula turns` is what she did, newest first, with the tokens and the cost of
each: a reply, or a compaction of the history into the summary.

```
./paula turns list          # the latest 20
./paula turns list -n 50    # the latest 50
./paula turns show 42       # everything about one reply
./paula turns dump 42       # the raw requests and responses of that reply
```

`turns show 42` shows which messages the reply answered, the prompt the model
was given, whether it was asked to reason and at which effort, and the reply
itself. A reply that asked for tools lists each call, the request of the round
that asked for it, and why it failed if it did. A request a tool made of a
runner, such as a web search, is one of the reply's, as `tool`, and the call
that made it names it under `SENT`. `turns dump 42` adds the headers and the
exact bytes in both directions, and what each call was asked with and
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

`paula callbacks` is the times she scheduled to write to you on her own, the
ones that have not come yet. It runs beside a `serve` or on its own too:

```
./paula callbacks list        # soonest first: the number, when it is due, and why
./paula callbacks cancel 3    # takes one away before it comes
```

A `serve` reads what is pending again before it fires a call back, so one
cancelled here does not come, even when it is due within the minute. Times
are in the machine's time zone.

`paula images` is the pictures of the conversation, the ones you sent and the
photos she sent, newest first, by the numbers her tools list them by. It runs
beside a `serve` or on its own:

```
./paula images list                  # the newest 20: when each was sent, by whom, and what it showed
./paula images list -n 50 -from 20   # the 50 after the newest 20
./paula images show 7                # one picture, with where its file is
```

`paula models` is the models the configuration file names, and which serves
each role:

```
./paula models list             # what each runner says about them, and the model of each role
./paula models available        # everything the runners offer, which is how you find a model's id
./paula models set chat fast    # has a role served by a model the file names, and remembers it
./paula models reset            # forgets those choices, so the file decides again
```

`list`, `available` and `set` read every runner's listing from its API, and
nothing of a listing is kept between runs: both APIs answer it `no-store`.
`set` writes the choice beside a `serve`, as `/model` does from a session: the
next reply, look at a picture or photo is made with it. Two things are settled
when `serve` starts and stay so until the next: whether the `photos` tools are
offered at all, which needs an `image` model then, and the model that paints
her avatar.

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
can override any of them. Tavily serves no models: it searches the web for the
`web` tools, so its section takes every key below but `provider`, and a model
written on it is refused.

| Key | Default | Meaning |
|---|---|---|
| `type` | — | `openrouter`, `venice` or `tavily` |
| `url` | the API's own | base URL |
| `token_env` | `OPENROUTER_API_KEY`, `VENICE_API_KEY` or `TAVILY_API_KEY` | the environment variable with the key |
| `idle_timeout` | `2m` | how long a request may go without a byte |
| `request_timeout` | `3m` | the whole a request that is not a reply may take |
| `retries` | `2` | how often at most a request is sent again, when the API answers with a status that asks for it, or when the connection of a listing or a check went before any status |
| `provider` | none | settings that only this API documents |

The key is never written to a log, and never appears in the request log either.

The two timeouts answer different questions, and neither shortens the other.
`idle_timeout` cuts any request that goes quiet. `request_timeout` is the whole
a listing, a key check, a web search, a page read or a picture may take,
however steadily it arrives; a reply has no such bound, since it arrives for as
long as she is writing. A picture arrives only once it is made, so the wait for
it is held to `idle_timeout` as well.

`request_timeout` is generous because a catalogue is usually instant and
occasionally stalls: OpenRouter answers its own in under a second most of the
time, and now and then takes minutes. Waiting costs nothing while an API is
healthy, and a run that cannot read a listing has no host to talk to anyway.

A request whose connection drops before any status arrives is sent again when
it asks the host to do nothing, as a listing and a key check do. One that asks
for something, a reply or a picture, is reported: nothing says the host did
not take it.

**OpenRouter** serves `https://openrouter.ai/api/v1`. Paula reads its catalogue
from `GET /models`, and from `GET /images/models`, which alone says which
models make pictures, and checks the key with `GET /key`. A model on both
listings is one model. She sends `408`, `429`, `502` and `503` again after the
wait `Retry-After` asks for, at most `retries` times, and gives up on a wait
longer than two minutes.

A model of the image listing paints from a prompt alone unless it must be
given a reference, and makes a picture from another when it takes one. Both go
to `POST /images`, the second with her avatar as its one `input_references`
entry, in the listed aspect ratio of the photo's shape; what the answer says a
picture cost is kept with the request. Nothing of the `provider` block reaches
a picture.

| `provider` setting | Request field |
|---|---|
| `routing.order`, `.only`, `.ignore`, `.allow_fallbacks`, `.require_parameters`, `.data_collection`, `.zdr`, `.enforce_distillable_text`, `.quantizations`, `.sort`, `.preferred_min_throughput`, `.preferred_max_latency`, `.max_price` | the `provider` object |
| `reasoning.max_tokens` | `reasoning.max_tokens` |
| `sampling.top_a`, `sampling.logit_bias` | `top_a`, `logit_bias` |
| `service_tier` | `service_tier` |

`routing.only` pins a model to certain hosts. At startup Paula checks that one
of them accepts every parameter she sends and serves the context you asked for.
A base name covers its variants, so `novita` matches `novita/fp8`.

**Venice** serves `https://api.venice.ai/api/v1`. Paula reads its catalogue
from `GET /models`, which lists the models that write text, and from
`GET /models?type=image` and `GET /models?type=inpaint`, which list the models
that paint a picture from a prompt and the ones that make a picture from
another, and checks the key with `GET /api_keys/rate_limits`. She sends `429`
again at the time in `x-ratelimit-reset-requests`, at most `retries` times. She
keeps Venice's own system prompt off unless `provider.system_prompt` turns it
on, and wants `sampling.seed` above zero. For the `web` tools she searches with
`POST /augment/search` and reads a page with `POST /augment/scrape`. She paints
her avatar with `POST /image/generate`, and makes a photo from it with
`POST /image/edit`, each asked for as a JPEG in the listed aspect ratio of its
shape. Neither answer says what a picture cost, so `paula turns` shows none.

No model on Venice is shown a picture in the answer of a call. Venice
documents that answer as a string, and turns a picture put there into the text
of its bytes: Claude, GPT and DeepSeek were each counted over ten thousand
tokens for one of 256 pixels, and none could say what it showed. So a model on
Venice is told what such a picture showed. On OpenRouter a model that sees is
sent the picture itself there, which Claude and GPT were counted about a
hundred tokens for.

Venice labels each model with a privacy. A `private` model runs where nothing
of a request is kept. An `anonymized` one, such as Claude, is passed to the
company that makes it with nothing that names you, and that company reads the
prompt. The catalogue page of each model says which it is.

| `provider` setting | Request field |
|---|---|
| `system_prompt`, `character`, `e2ee` | `venice_parameters` |
| `sampling.min_temperature`, `sampling.max_temperature` | `min_temp`, `max_temp` |
| `output.stop_token_ids`, `output.verbosity` | `stop_token_ids`, `verbosity` |
| `fallbacks` | `fallbacks`, up to 10 models the catalogue serves |
| `search.provider` | `search_provider` of a web search, `brave` or `google`; the runner's own block sets it, and a model's that names another is reported |
| `image.hide_watermark` | `hide_watermark` of a painted picture; `true` when the block says nothing |
| `image.safe_mode` | `safe_mode` of a painted picture and of a photo, which blurs what Venice reads as adult; `false` when the block says nothing |

Venice publishes no list of accepted parameters, so a sampling setting is held
against nothing there. Only what a model can do is checked.

**Tavily** serves `https://api.tavily.com`. For the `web` tools she searches
with `POST /search`, asking for the day each page was published, and reads a
page with `POST /extract`. She checks the key with `GET /usage`, and sends
`429` again after the seconds in `Retry-After`, at most `retries` times.

### Models

A model needs a `runner` and the `id` that runner knows it by. It may also set
`context`, `cache: false` where its family allows it (see
[Model families](#model-families)), and any of the settings below.

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

A model that only makes pictures is sent none of these, so none of them is held
against it, including the ones its runner writes for every model: a runner's
`reasoning.effort` or `routing.only` is for the models that chat. On Venice,
the `provider.image` block is what reaches a picture.

### Model families

How a model reads a prompt, and what it needs to cache one, depends on its
family and on the API that serves it. So each pair has an extension of its
own, chosen by the runner's `type` and the model's `id`. No two extensions
serve the same model, so changing one never changes what another model is
sent. A model none of them serves is sent nothing about caching, and reads her
notes as system messages.

An extension decides two things. The first is the role of her notes: the time
told before each message you send. A system message is the
role a model reads for what it is told rather than for what it answers. Some
hosts move every system message ahead of the conversation, next to the card,
where the times change the prompt on every reply and nothing of the
conversation is read from the cache. For those, her notes are user messages,
and the card is followed by a sentence saying that they come from the app
rather than from you, and are not to be answered. A host that caches where it
chooses, rather than where a request marks, reads an earlier prompt only as far
as the next one starts with it. A model on such a host is also told the time
before the message she answers as when it was sent, the way the next prompt
tells it, rather than as what time it is now. The second is what a request
carries for caching.

Every model an extension serves caches, the way its host caches best: where
the host takes explicit caching, for the longest the host keeps it, and
otherwise as the host caches by itself. `cache: false` on a model turns caching
off where its host allows that, and is reported where it does not. A `cache`
key on a model no extension serves is reported too.

```yaml
models:
  opus:
    runner: openrouter
    id: anthropic/claude-opus-5.5
    cache: false
```

| Extension | Serves the ids | Notes | Sends | Caches | `cache: false` |
|---|---|---|---|---|---|
| `openrouter/claude` | `anthropic/*`, `~anthropic/*` | system | `session_id` | two markers on a reply, for an hour | marks nothing |
| `openrouter/gpt` | `openai/gpt-5.6*`, `openai/gpt-6*`, `~openai/gpt-astra-latest`, `~openai/gpt-sol-latest`, `~openai/gpt-terra-latest`, `~openai/gpt-luna-latest` | system | `session_id`, `prompt_cache_key` | explicit mode, two breakpoints, for 30 minutes | explicit mode with no breakpoint |
| `openrouter/deepseek` | `deepseek/*`, `~deepseek/*` | user, the last time as sent | `session_id` | by itself | reported |
| `venice/claude` | `claude-*` | user | `prompt_cache_key` | Venice's markers and one more | reported |
| `venice/gpt` | `openai-gpt-56-*`, `openai-gpt-6-*` | user, the last time as sent | `prompt_cache_key` | by itself, for at least 30 minutes | reported |
| `venice/deepseek` | `deepseek-*` | user, the last time as sent | `prompt_cache_key` | by itself | reported |

GPT models before 5.6 cache by other rules, and no extension serves them.

A host keeps a cache on the machine that wrote it. OpenRouter sends every
request that names one `session_id` to the host that served the first of them.
Venice routes by `prompt_cache_key`, and OpenAI groups its cache by it. Both
are the card's `id` and what the request is for.

**Claude on OpenRouter** writes a cache only where a request marks it, and a
request of a reply carries two markers. A top-level `cache_control` marks the
end of the prompt, where the rounds of a reply find what the round before them
wrote. The other marks the last message the next reply sends again as it is,
which is where the next reply finds what this one wrote: the time before the
message she answers reads differently in the next prompt. Nothing else is
marked. A request that also marked the message she was given before that one
read nothing new once its cache had expired. A caption or a compaction is
marked nowhere, since no later request sends it again.

A marker lasts an hour, the longest Anthropic keeps one. Texts are often more
than five minutes apart, which the five-minute marker does not outlast.
Reading the cache costs a tenth of the input price, and writing it for an hour
costs twice the input price. Each reply writes only what is new since the one
before.

**GPT on OpenRouter**, from GPT-5.6 on, takes a breakpoint only on the text of
a message the model was given, and a request carries two. One is on the last
such message the next reply sends again, which is where the next reply finds
what this one wrote. The other is where the reply before it put its own, since
in explicit mode a request reads only at the breakpoints it carries. Explicit
mode writes nothing but at a breakpoint, so the time before the message she
answers, which the next prompt tells otherwise, is never written. A cache
lasts 30 minutes, the only lifetime OpenAI gives, reading it costs a tenth of
the input price and writing it 1.25 times.

**GPT on Venice** takes no `prompt_cache_options`, and read no more with
breakpoints than without, so OpenAI writes its cache at the end of the prompt.
A request read the whole of the one before when that prompt started it as it
was sent, and only the card when the time before the message she answered was
told as what time it is now. So that time is told as when it was sent, which
is the same minute whenever a reply starts as the message lands. Nothing sets
how long the cache lasts: from GPT-5.6 on, only `prompt_cache_options` does.

**Claude on Venice** is marked by Venice itself, on the system prompt and on
the second-to-last user message. With her notes as user messages, that one is
the time before the message she answers, which the next reply tells
otherwise, so the last message the next reply sends again is marked as well.
The marker lasts as long as Venice's own: a longer one takes a header Venice
does not document.

**DeepSeek** caches a prompt without being asked, on both hosts, and reads it
in steps of 256 tokens, for five minutes. With the time before the message she
answered told as what time it is now, the third reply of a conversation read
3840 tokens; told as when it was sent, it read 4096, the whole of the prompt
before it.

No model caches a prompt shorter than its minimum, so a conversation that has
just begun may show none.

### Roles

| Role | Needs a model that | Required |
|---|---|---|
| `chat` | chats and accepts tools | yes |
| `vision` | sees images | no |
| `image` | makes a picture from another and a prompt | no |
| `avatar` | makes a picture from a prompt | no |

Without a `vision` model, pictures reach the chat model only if that model can
see them itself.

The `image` model makes the photos she takes with the `photos` tools, each from
her avatar: the picture of her in `data_dir`, as `avatar.jpg`, `avatar.jpeg`,
`avatar.png` or `avatar.webp`. Put one there yourself, or name an `avatar`
model, and `serve` paints one from the card's `appearance` when it finds none:
a head-and-shoulders portrait against a plain background, kept as
`avatar.jpg`. A portrait says how she looks and nothing more, which is all a
photo made from it should keep. It is painted once, before anything is
served, and a card that says nothing of her appearance is refused. Every
frontend that has somewhere to show a face shows it.

A reply is offered the tools the file names, so the chat role asks for a model
that takes them.

### Engine

| Key | Default | Meaning |
|---|---|---|
| `debounce` | `2s` | how long a message waits before she starts writing |
| `prefill_cancel` | `true` | a new message restarts a reply that has written nothing yet |
| `summary_ratio` | `0.3` | part of what the context leaves once the card and the tools are written that the summary is written to fill; above 0, and below `history_ratio` |
| `history_ratio` | `0.6` | part of the same that the history may take before it is compacted into the summary; above 0, and together with `summary_ratio` below 1 |
| `image_max_px` | `1024` | longest side of a stored image; `0` keeps it as it is |
| `log_keep` | `500` | how many entries keep the bodies of their requests; a compaction is an entry of its own |
| `tool_rounds` | `3` | how many rounds of tool calls a reply may take, at least 1; in the round after them, a call that only looks something up is not run |

### Frontends

A frontend is a way of reaching her. List as many as you like: they all show
the same conversation, and each shows what you said on the others.

**`repl`** listens on a Unix socket: `paula.sock` inside `data_dir`, or the
`socket` you name. That path is relative to `data_dir`, not to the
configuration file. A terminal shows no picture, so a photo she sends is a line
of its own saying where its file is.

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
person, in your private chat with the bot: a message from anyone else, and one
you write in a group the bot was added to, is logged and left alone. Send her text,
photos, stickers or an image as a file; a sticker is read as its emoji, with
the picture.

What she writes as separate paragraphs arrives as separate texts, each sent as
she finishes writing it, with the typing status up in between — she texts the
way a person does. A text longer than Telegram takes carries on in another.
`/models` and the rest are offered by the client as you type them, and the
models menu is buttons to tap. A photo she sends comes after her texts, as a
message of its own.

Her avatar is the bot's profile photo, set as `serve` starts. Nothing Telegram
answers says which picture a bot has, so the run keeps the digest of the one it
set in `telegram.avatar` beside the database, and sets it again only when the
avatar is another. A bot that refuses it is logged at `warn` and talks all the
same.

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
| `listen` | `127.0.0.1:8484` | the address to listen on |
| `token_env` | `PAULA_WEB_TOKEN` | the environment variable with the token |

```yaml
frontends:
  web:
    listen: 127.0.0.1:8484
```

The token is asked for as an ordinary sign-in and goes in either box, so a
browser remembers it. Make it long: at least eight characters, and without a
colon or a space. Anyone who reaches the address and has the token reads the
whole conversation, and the page is served over plain HTTP, so it listens on
this machine alone unless something in front of it does the letting in, over
TLS.

Every browser that opens the page gets a session of its own, and they show each
other what is typed. A message she writes arrives text by text, as it does
everywhere else, and the one she is in the middle of fills as she writes it. A
photo she sends comes after her texts, in a bubble of its own. Scrolling up
reads further back. A message shows as it was written, as it does on every
frontend, and an address in it is a link.

Her avatar is her face beside her name, the page's icon, and the icon of a
notification.

On a page the browser calls secure — `https`, or `localhost` — a bell asks
whether to notify you. While the page is open but not being looked at, the
first text of a reply to something you sent from it comes as a notification,
and clicking it brings the page back.

### Tools

A tool is something she can do in the middle of a reply: she asks for it, it
runs, and she writes on with what it answered. Only the kinds listed under
`tools` are offered. While one runs, the frontend says what she is doing, such
as `searching memories for Ana`.

A reply takes at most `tool_rounds` rounds of calls, and the round after them
is the last. It is sent as the round before it was, with what came of that
round, and the last answer in it ends with a note saying the calls are over
and to answer now. Nothing earlier in the request changes, and no message of
the user's turn comes after the answers, so the host reads from its cache all
it kept of the round before, thinking included.

In the last round, a call that only looks something up is not run, since
nothing would read what it answered, and neither is one whose answer only a
later round could act on, such as taking a photo. One that changes something
still runs, so putting the answer off, or keeping a memory, works there as it
does in any round. When the last round left a call to look something up, or wrote nothing
without putting the answer off, she is asked once more for an answer, this
time with calls turned off, which a host reads from its cache no further than
the prompt's start.

Her memories are never in her prompt, and neither are the pictures of messages
the history no longer carries: the tools are how she reaches them. So each
tool's description tells her what her prompt holds, what it does not, and
which tool reaches what, at whatever length that takes.

**`memory`** reaches what she remembers.

| Tool | What she does with it |
|---|---|
| `list_memories` | lists the memories she kept, newest first and 50 at a time, each with its number and the day it was said; a list with older ones after it says so, and `from` lists them |
| `search_memories` | looks for memories by the words they hold, in the card's language; each comes with its number |
| `remember` | keeps a lasting fact about you or about her, in place of the memories it updates, given by their numbers |
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
names none keeps nothing, and she is told so. Nothing but these tools makes or
changes a memory.

**`images`** reaches the pictures you sent and the photos she sent. It takes no
settings.

| Tool | What she does with it |
|---|---|
| `list_images` | lists the pictures, newest first and 50 at a time: its number, when it was sent and by whom, and what it showed; a list with older ones after it says so, and `from` lists them |
| `get_image` | looks at one picture again by its number: a model shown a picture in the answer of a call is sent the picture there, and any other what it showed |

```yaml
tools:
  memory:
  images:
```

**`photos`** lets her send you photos of herself. It takes no settings, and
needs her avatar and an `image` model (see [Roles](#roles)): `serve` refuses
it without either, saying which.

| Tool | What she does with it |
|---|---|
| `take_photo` | takes a photo of herself, as its camera sees it: where she is, what she is doing and wearing, the light, and where the camera is, in the shape she asks for, `portrait`, `landscape` or `square`; she is told its number, and is sent the photo in the call's answer, or told what it shows, as `get_image` sends a picture |
| `send_photo` | puts a photo she took in her reply, by its number, after what she writes |

```yaml
tools:
  photos:
```

A photo is the `image` model's, made from her avatar and her prompt, which
comes after a line telling the model that the avatar says how she looks, that
the photo is a new one, and that it is what its camera sees, so no camera or
phone is in it but in a mirror. Told only of a place, a model made the avatar
again over another background, and told of a selfie, it drew her holding a
phone. Her avatar goes to the model as every picture Paula keeps does, turned
upright and scaled to `image_max_px` as a JPEG, whatever file it is. The photo
is kept as a picture she was sent is, under the call that took it, and the
`vision` model describes it at once. She looks at it, or reads what it shows,
before she sends it. In the last round of a reply a photo
is not taken, as a search is not: nothing after it could send it.

Every photo is a request the image model's API charges for, apart from the
chat model: a few cents each on Venice. Her system prompt tells her so, and to
take one only to send it. In the prompts after a reply that carried a photo,
the photo is told after her message the way a time is: `Your message above
came with [photo: …].`

**`callbacks`** lets her write to you on her own. It takes no settings.

| Tool | What she does with it |
|---|---|
| `schedule_callback` | picks a time to write to you, and why: when she said she would get back to you, or wants to check how something went; a time that has passed is refused |
| `list_callbacks` | lists the call backs she scheduled that have not come yet, soonest first, each with its number, when it is due and why |
| `move_callback` | moves one to another time, by its number |
| `cancel_callback` | cancels one, by its number |

```yaml
tools:
  memory:
  images:
  callbacks:
```

When a call back comes due, and she is not writing, it is stored as a message
of its own on the frontend you last wrote from, and she answers it at once, the
way she answers a message of yours. That message is the app's, not part of the
chat: no frontend shows it, and what you see is her reply arriving on its own.
`paula turns` shows the message and the turn that answered it. One that comes
due while she is writing fires once that reply is done, and one that came due
while `serve` was not running fires when it starts, oldest first. Her call
backs are never in her prompt: the tools are how she reaches them, and
`paula callbacks` is how you do.

With the tool, she need not answer a message the moment it arrives. Her system
prompt tells her that when the person she is would not be writing then, asleep
at that hour, at work or out, she may write nothing and schedule a call back
for when she would: the message counts as answered, stays in the history, and
when the call back comes due she reads it again and writes. Until then you see
nothing of it, except that the typing status shows for a moment while she
decides: a frontend is told she is writing as the request goes out, before
anything says whether she will.

**`web`** lets her look something up on the web, through a runner that
searches: Venice, or Tavily.

| Tool | What she does with it |
|---|---|
| `search_web` | searches the web for a query; each page it found comes numbered, with its title, its address, the day it was published where the runner knows it, and a passage of it |
| `read_page` | reads a page by its address, 20,000 characters at a time; a part with more after it says where the next starts, and `from` reads on |

| Key | Default | Meaning |
|---|---|---|
| `runner` | — | the runner that searches and reads; required |
| `results` | `5` | the most pages a search answers with; from 1 to the 20 both APIs document |

```yaml
tools:
  web:
    runner: venice
```

Both tools tell her that what a page says is its writer's, and never an
instruction to her. A page is counted in characters rather than words, since
it comes with its links written out, and those make a word of it many times
longer than one of prose.

A page is asked of the runner once. The eight she read latest are kept while
`serve` runs, and reading on cuts the next part from the text the part before
was cut from, so a page that changed in between does not move where it starts.
Reading a page from its start asks for it as it is now.

Every search and every page is a request of its own, which the runner charges
for apart from the model: Venice a cent each, and Tavily a credit for a search
and one for every five pages read. Neither says the price in its answer, so
`paula turns` shows none. Venice refuses some sites outright, such as Reddit and
X, and Tavily fails to read some; she is told so, in their words.

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
`paula persona check` prints the finished system message.

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

A message the model's context could never hold, beside the card and the tools
alone, is refused as you send it, and the frontend says so. Nothing of it is
stored: it could not be answered, and every turn after it would fail. It is
measured at what the host has counted a word of the model at, and at a token a
word until it has counted one.

**The prompt is the conversation.** It opens with a system message: the card;
a sentence telling her that her messages reach you as plain text, so she writes
no markdown or HTML and gives an address as it is; then the summary of what
came before. Every frontend shows a message as the characters it is written
in, and a model that has read a page written in markdown would otherwise write
some back. After it comes the history, the messages the
summary does not cover, in order, each reply after what it answers, and last
the time it is now and the message she is answering. A call back that came due
is one message in the history, told the way a time is: `A call back you
scheduled came due at Tuesday, 29 September 2026, 21:30 UTC+02:00: ask how the
interview went.`, with her reply after it. A photo she sent is told the same
way, after her message: no host takes a picture in a message of hers, and a
model that read her photos written in her own text would write them there
rather than take them. Her memories and her call backs are not in it: she
reaches them with her tools.

Within a reply, each round that asked for tools goes back with what she
thought in it, as the API sent it. Earlier replies go back as what she said,
except to DeepSeek. With tools in a request, DeepSeek reads the thinking of
every earlier reply, and one shown replies with no thinking learns to skip its
own, or to leave it open and write the reply inside it. So DeepSeek gets each
earlier reply with what she thought on the way to it: the reasoning text, and
the details sent beside it. A reply that ran tools goes back as one message,
the round that wrote it, so the details are that round's. That thinking is
part of the prompt, so it is counted with the history and compacted with the
messages it belongs to.

The details go back exactly as they came, and only to the model that wrote
them, on the runner it wrote them on: signed or encrypted thinking belongs to
that model and host, and another's refuses it. Switch to DeepSeek and the
replies another model wrote go to it with their thinking as text alone. Each
reply looks the same in every prompt of one model, so each model keeps its own
cache, and switching back finds the first model's prompt as it left it.

**Every part of the prompt has its reservation.** The card and the tools are
written into every reply. What the model's `context`, or the largest its
catalogue reports, leaves once they are written is shared by ratio: the summary
is reserved `summary_ratio` of it, the history `history_ratio`, and the rest is
the user input's, which is the message she is answering, its pictures, the
rounds of tool calls, and her answer. With a context of 200,000 tokens, a card
and tools of 10,000, and the default ratios, that is 57,000 for the summary,
114,000 for the history and 19,000 for the user input. A model that can be the
chat model and whose context the card and the tools fill on their own is
refused when the conversation opens. Nothing has been counted then, so they are
measured at a token a word, the least a word comes to.

**The history is compacted when it overflows its reservation.** When a turn
ends, the messages it answered and her reply join the history. If that takes the
history past its reservation — its own, not the whole context — the turn itself
still went out whole, since what took it over was in the user input's
reservation. Right after it, the whole history, with the summary so far, is
compressed into a new summary that fills the summary's whole reservation, and
the next turn starts with an empty history. A reply she was writing when your
next message arrived is newer than that message, so it is not compacted: it
stays in the history with it. A compaction writes no memories: nothing but her
tools does.

A compaction compresses the summary so far together with the history. At the
default ratios that is three times as much text as the summary may take, so it
has to shrink to a third. The compaction works that fraction out from the sizes
and asks for everything at it: a part of 3,000 words is asked back in 1,000.
Text that is already shorter than the summary may take is asked back at its own
length, never longer: a history of pictures fills its reservation with what a
host bills for them, and is written by what they showed.

One request cannot always carry all of that and have the new summary written
back within the context, so the text is cut into parts, in order. There are as
few parts as fit a request together with what the model writes back, and no
part asks for more than the model writes in one answer: what its catalogue
says, or `output.max_tokens` when that is less. A part the model stops writing
at that limit has lost the end of what it summarises, so the compaction fails.
The parts are sent one after another, so a key held to a rate is never asked
for all of them at once, and what they wrote, in order, is the new summary. A model does not keep to the
words it is asked for, and a summary it wrote past its reservation is written
again on its own. The summary so far is compressed again with each compaction,
so what is older ends compressed more than what is recent.

A compaction goes on beside the conversation, so once her reply is done you can
write again while it runs. A message that arrives while the history is being
compacted waits for it, since its prompt needs the compacted history, and
`/stop` ends that wait while the compaction goes on. A compaction that fails is
said the way a failed reply is, with the host's error, and the history stays as
it was; your next message has it compacted again before its turn, and when
that fails too the turn goes out as it is, with the history past its
reservation, so a summary model that is down does not silence her. Only a turn
whose prompt is past the context needs the room, and that one is dropped and
said. Nothing is thrown away: the conversation itself keeps every message, and
`paula turns` shows every compaction and what it asked.

**A prompt past the context is not sent.** A message longer than the user
input's reservation can take a prompt past the model's context, and the host
refuses that. A turn measured past it waits for the history to be compacted to
make room, and a turn still past it fails, saying so. A message no reply could
answer would fail every turn after it, since every prompt carries it: `/stop`
answers it with nothing, and it goes into the history behind her, where the
next compaction folds it into the summary from as much of it as the model
reads at once, saying the rest was left out. Each round of tool calls adds the
calls and what they answered. A round past the context before its
answers are in runs none of its calls, since what they did would never reach
her, and the reply fails, saying so. An answer that would take the round past
the context is not sent: she is told the call ran and its answer did not fit.
All of this holds once a prompt of the model has been counted: until then a
word counts high on purpose, and a prompt held back on that count would never
be counted.

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

Measuring means counting tokens, which only the host can do exactly, and it
counts every prompt a reply sends. Paula counts words and turns them into tokens
at a rate read off those counts, per model.

A word does not cost the same in every prompt. A history of short messages, each
with a time before it, came to about 2.1 tokens a word on one model, and the
card with a summary to about 1.3. So the rate is the most the prompts of the
run have come to, and a prompt that comes to less leaves it where it was: a
mistake makes the history compacted a little early rather than a prompt too
long. A prompt that comes to more is taken only once the next one comes to
more as well, and then at the lower of the two, so one prompt that is an
exception does not set the rate for the rest of the run. The rate rises a reply
late, which the user input's reservation leaves room for.

Only the first round of a reply is read: the rounds after it carry the calls it
made and what they answered, which the history never does. A run starts at
three tokens a word, more than any prompt has come to, until its first reply is
counted; a run that starts on a history near its reservation has it compacted
before its first turn. A compaction's count is not used: it carries the
conversation as a transcript, not as a reply does.

A picture is counted the same way. A host bills one by how big it is, and each
host by its own reckoning — tiles for one, pixels for another — so there is no
number that is right for all of them and none to set in the file. Paula starts
at 1000 tokens a picture, which is high for the size she stores them at, and
reads what one really costs off the counts of two prompts in a row: when the
second carries a picture more and at least the words of the first, the two
counts give what a word and a picture cost. After that, what a word costs is
read off every prompt less its pictures. Until then, a model that sees counts
its pictures as part of the words, which errs high: it carries the pictures of
the recent messages in every prompt, so none of its prompts is words alone.

**The time is told, not written into a message.** Before each message of yours
stands a message of its own: `The next message was sent at Saturday, 19
September 2026, 09:06 UTC+02:00.`, and `It is now …` before the last one. A
model writes like the messages it reads, and a small one that reads a time
inside the message it is answering starts stamping its own replies with one. As
a message of its own it is read rather than copied, it needs no notation to
explain, and since only the one before your latest message ever changes, what a
host has cached of everything earlier still stands. It is a system message, or
a user message for a model whose host moves system messages ahead of the
conversation, and so is the message saying a call back came due. A model whose
host caches where it chooses, rather than where a request marks, is told when
your latest message was sent rather than what time it is now (see [Model
families](#model-families)).

A host keeps a prompt on the machine that read it, so the requests of one
conversation name it, in the field the model's family extension sends. The
name is the card's `id` and what the request is for. `paula turns` shows how
much of each prompt a host had cached and how much it wrote to the cache, as
each API reports it.

**Pictures are kept twice.** She accepts JPEG, PNG, GIF and WebP up to 64
megapixels. Each one is scaled to `image_max_px`, turned upright by its EXIF
orientation and kept as JPEG in `media/`, and the `vision` model describes it
the first time it is used; both stay for good, whatever the chat model can do,
so any model you switch to can be served it. A chat model that sees images is
sent every picture its prompt carries as the picture, never its description in
its place; any other is sent the description. A photo she takes is kept and
described the same way, as it arrives.

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
requests it made: the reply itself, a look at any picture she was sent, and
any photo she took. Each request keeps its headers, bodies, timings, tokens and
cost, and shows a dash for a cost the host did not report. A picture a chat
request carries is recorded by the sha256 it is kept under in `media/`, in
place of its bytes, so a prompt of pictures costs the record a line each and
the picture is read from `media/` when it is wanted. A picture a host answered
with is kept whole. `paula turns dump` prints one that came as a file, as
Venice's does, as its size and type, and one that came in JSON, as
OpenRouter's does, as the JSON it came in. A photo's request carries her
avatar as it was sent, so each keeps a copy of it for as long as its body is
kept. Bodies older than the latest `log_keep` entries are dropped, and the
rest stays. `paula turns` is the window into it. The avatar is painted before
any turn, so its request is logged and not recorded.

## Your data

```
data/
  paula.db         the conversation, what she remembers of it, the settings,
                   the request log
  paula.db-wal     the write-ahead log SQLite keeps beside it
  paula.db-shm     the shared memory it keeps with it
  media/xx/        every picture, as JPEG named by its sha256, under the
                   first two characters of that name
  avatar.jpg       her avatar, painted or put there by you
  paula.sock       the socket a terminal connects to
  paula.lock       held by the running serve
```

The database is SQLite in WAL mode, with `STRICT` tables and foreign keys on,
stamped with `PRAGMA application_id = 0x5041554C`. Paula refuses a database she
did not create, and one written by a newer schema than the build knows, without
touching either. An older one is brought up to date by `serve`. A command that
only reads says to run `serve` first, rather than read a schema it does not
know. Only one `serve` may use a directory at a time.

Nothing leaves the machine except the requests to the model API, and those of
the `web` tools. The first carry the prompt, the pictures and the settings; a
photo's carries her avatar and what she asked of the photo, and the avatar's
what the card says of her appearance. A web search sends its query, and
reading a page its address, to the runner the `web` tools name. A search of
her memories runs in the database and sends nothing.

## Keeping it running

`serve` runs until it is stopped. It ends with an error when a frontend stops
serving, and at start when a listing cannot be read even after its retries, so
run it under something that starts it again: a systemd unit on Linux, or a
launchd job on macOS, with the keys in its environment and its working
directory where `paula.yaml` is. `paula version` says what the running binary
was built from, as Go stamps it: a tag when it was built at one, and otherwise
the time and the commit, as `v0.0.0-20260928090838-f3c89d3f3ffa`, with
`+dirty` after it when the checkout had changes not committed.

A copy of `data_dir` is a copy of everything: the database is `paula.db` with
its `-wal` and `-shm` files, and the pictures are `media/`. Copy them while
`serve` is stopped, or take the database with `sqlite3 paula.db ".backup
paula-copy.db"` while it runs, which is the one way to copy a database in WAL
mode that is being written to, and copy `media/` beside it.

## Development

```
go fix ./... && go build ./... && go vet ./... && gofmt -l . && go mod tidy -diff && go test -race ./...
```

`go fix` is the one that writes rather than reports: it says what the standard
library now has a shorter way of saying, and applies it, so what it changes
belongs to the commit it ran in. What it changes depends on the Go it comes
with, so the checks run with the toolchain the `toolchain` line of `go.mod`
names, which is raised with `go get toolchain@goX.Y.Z` when the Go you build
with is. The same six run on every push to `main` and every pull request, in
`.github/workflows/build.yml`, with that toolchain, where a change `go fix`
would make fails the run. A push to `main` that passes them builds the Linux
binary and puts it on the `rolling` release.

The tests run without a network: every runner test answers from captured API
responses in `testdata`, and `SOURCES.md` in each of those directories says
which request each file came from.

Six tests run against real models, and only when `PAULA_LIVE` names a
configuration file. Each reads the keys the environment holds, as `serve` does.

```
PAULA_LIVE=live.yaml go test ./internal/conversation -run '^TestLive$' -v -timeout 3h
```

The first holds the conversation to the file's default chat and vision
models, at the context the file gives the chat model. It goes through months
of ordinary texting in `internal/conversation/testdata`, which covers contexts
up to about 200,000 tokens; a larger model's `context` holds it there.

The test runs Paula three times. The first run, after the first day of that
past, asks her to remember something and sends a picture. Before each of the
other two runs, as much more of the past is written as the history's
reservation holds, measured at the most a word has cost in the runs before.
Each of those runs starts counting a word higher than that, finds the history
past its reservation, and compacts it before its first turn, whose messages
wait for it; the second of them compresses the summary so far with it. The
last one then asks for her memories and for the picture again.

Everything goes to a data directory of its own, which is kept, with a
`report.md` of every check, what it expected and what it found, the requests
that show it, and what each request sent, was cached and cost.

The second asks a model whether its cache works. It keeps the conversation in
a directory of its own, never in the file's `data_dir`. It sends
`PAULA_LIVE_TURNS` messages (4 by default) to the chat model, or to the model
`PAULA_LIVE_MODEL` names. Some reply after the first has to read the cache, and
from that one on each has to read at least as much as the one before, ending
with more. With `PAULA_LIVE_CACHE_WAIT`, it waits that long and sends as many
again. After the wait, reading has to come back and pass what it read before.
Set the wait past how long the model's cache lasts:

```
OPENROUTER_API_KEY=… PAULA_LIVE=scratch/paula.yaml PAULA_LIVE_CACHE_WAIT=6m \
  go test ./internal/conversation -run TestLiveTheCache -v -timeout 30m
```

`-v` prints how much each round of every reply read from the cache and wrote to
it, and the reply itself, which shows whether a model read her notes as notes.

The third holds what a word costs to what the host counts. It sends ten texts
to the chat model, one asking her to keep something and one asking for it, so
some replies run a tool. A model that sees gets a picture with the second
text, and the first round of that reply has to give what a picture costs.
Every reply's rate has to be its first round's count, less its pictures, over
that round's words, and the rate kept has to be the most two replies in a row
came to. It keeps its data directory, with a `report.md` of what every round
was counted at:

```
OPENROUTER_API_KEY=… PAULA_LIVE=scratch/paula.yaml \
  go test ./internal/conversation -run TestLiveTheRate -v -timeout 20m
```

The fourth holds her call backs to a real model and the machine's clock. It
asks her to ping in two minutes, to make it four, to cancel, and to ping in a
minute, which has to fire as a message she answers with the note that it came
due; then it asks for one more, closes the run, waits past its time, and opens
a new run, which has to fire it as it starts. It takes about three minutes:

```
VENICE_API_KEY=… PAULA_LIVE=scratch/paula.yaml \
  go test ./internal/conversation -run TestLiveCallbacks -v -timeout 20m
```

The fifth holds the `web` tools to a real model and to the runner the file's
`web` section names. It asks her what concerts are on in Lisbon this week,
which she has to search the web for, and then to open the first page she
found; each has to run its tool, whose call has to have sent a request the
runner answered `200`, and end in a reply. Run it once for each runner that
searches:

```
VENICE_API_KEY=… TAVILY_API_KEY=… PAULA_LIVE=scratch/paula.yaml \
  go test ./internal/conversation -run TestLiveWeb -v -timeout 20m
```

The sixth holds the `photos` tools to the file's `image` and `avatar` models.
It paints her avatar from the card into its own data directory, as `serve`
does, and asks her for a selfie, which she has to take and send: the reply has
to carry a photo she took, described, whose file is a JPEG. The report names
the files, to look at:

```
VENICE_API_KEY=… PAULA_LIVE=scratch/paula.yaml \
  go test ./internal/conversation -run TestLivePhotos -v -timeout 20m
```

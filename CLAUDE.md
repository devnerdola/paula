# Paula

Paula is a texting companion: a character described in a YAML card, a hosted
model behind her, one long conversation in SQLite, and a terminal to reach it
through. One Go binary, no services behind it. The module is
`nerdola.dev/x/paula`.

This file is the working agreement and the design it protects. Read it before
changing anything here.

## How work goes here

- **Design first, then write.** Decide the design, write down the reasons, and
  say what you are about to do. Do not open files on an assumption, and do not
  hand over a list of options to be picked from. Ask only what only the owner
  can decide.
- **Stop at the end of each agreed piece** and wait. Hand off what was built,
  the exact build and run commands, the manual scenarios worth trying with
  their expected results, the known limitations, and the `go test ./...` output
  as it came.
- **Scope is what was agreed.** A new idea goes in the handoff, not in the
  commit.
- **Write code for what was asked or reported, never for a case nobody
  raised.** Structure for a case that cannot happen is raised, not built.
- **Fix at the design level.** A fix that patches a symptom, tunes the persona
  card, or works around a library is a workaround: say so plainly and say what
  the clean fix would be.
- **Before adding a mechanism, look at what already handles that case.**
- **When a model or a host misbehaves, suspect the request first.** Find what
  Paula sends that differs from how the model is normally called.
- **Run no model probes** unless asked, or unless a reported problem cannot be
  understood without one. A live run uses the model's real context.

## Package layout

Every kind of plugin has the same three-way shape:

- `internal/X` — the interfaces and the kind table. The only place that names
  an implementation.
- `internal/X/api` — the vocabulary the implementations and their callers
  share. It imports neither its parent nor anything beside it; what it does
  take is what the whole tree takes, `config`, `store` and `logs`.
- `internal/X/<kind>` — one implementation, importing `internal/X/api` and
  never `internal/X`.

Go interfaces are structural, so an implementation satisfies `internal/X`
without importing it. A caller imports `internal/X/api` beside `internal/X`
rather than reading the vocabulary through the parent: the parent aliases only
the types of its own signatures (`Host`, `Names`). The parent knows nothing
about any one implementation: outside the kind table, no code in
`internal/frontend` names the repl and none in `internal/runners` names a
provider.

Built for `internal/runners` (`api`, `openai`, `openrouter`, `venice`) and
`internal/frontend` (`api`, `repl`). The same shape applies to tools and to
whatever comes next.

## What the design holds to

- **One conversation.** No conversation id, no persona id, nothing keyed by
  either. The `kv` table holds `model.<role>` and nothing else.
- **One writer.** The conversation loop is the only thing that writes the
  conversation. Every field of the loop is touched from its one goroutine.
- **A request carries what records it.** The recorder rides on the chat
  request, never in a context. A request that belongs to no turn is not
  recorded.
- **Listings are read from the API every run.** Both APIs answer them
  `no-store`; nothing of a listing is kept between runs.
- **Validate, don't adjust.** A configured value a model cannot take is
  reported under its key, never rewritten to something the model takes. A
  default of Paula's own is chosen among the values the model lists.
- **One setting never clamps another.** Two bounds over the same request each
  measure their own thing: `idle_timeout` a gap between bytes,
  `request_timeout` the whole of a request that is not a reply.
- **Setup-agnostic.** Nothing in the code names a model, a family or a host.
  Roles are `chat` and `vision`, and what a role needs is a capability.
- **The prompt is the card, then the conversation.** A time is told in a
  message of its own before each message she was sent — when an older one was
  sent, and what time it is now before the one she is answering. Nothing is
  written inside a message: a model writes like the messages it reads, and one
  that reads a time in them starts writing times of its own.
- **What changes goes last.** A host caches the longest matching prefix of a
  prompt, so anything that changes every turn belongs at the end, after
  everything that does not.

## Schema and migrations

`STRICT` tables, WAL, foreign keys on, stamped with `PRAGMA application_id`.
Migrations are numbered files applied in order, each in a transaction.

**An existing migration is never edited**, `0001_initial.sql` included. A
schema change is a new `NNNN_name.sql`, and data that has to move comes with
it. Paula is published, so a database she wrote is out there and the only way
to reach it is a migration after the ones it has already run.

The schema says each thing once: a column nothing reads is deleted, and what
can be read from the entries is not kept beside them.

## Tests

- **Real data only.** Nothing a test checks against may come from the code
  under test. Captured API answers and request bodies live in `testdata` with a
  `SOURCES.md` saying where each came from; they are never hand-edited — a
  fixture that must change is captured again.
- **The adversarial case first**: the empty file, the stop that lands as the
  reply finishes, the answer with no events.
- **Prove a fix by breaking it.** Revert the fix, watch the new test fail, put
  it back.
- **Each behaviour is covered once**, and a test is named for the behaviour it
  holds rather than for the function it calls.
- The suite runs without a network.

## Writing

- Plain statements. No riddle with a dropped subject, no sentence that stacks
  three clauses where two sentences do.
- A comment says why, or says what is not visible in the code. A comment that
  restates the line under it, or the name of the test above it, is deleted.
- **No process anywhere** — not in the code, the docs or a commit message. No
  phases, no plans, no "rewritten", no "now". Say what the code does and why.
- **The README never drifts.** A change that touches something it describes
  updates it in the same commit.
- The person Paula texts is whoever the card names. No gendered pronoun for
  them in code, comments or docs.

## Git

- **A feature is developed on a branch of its own.** Commit on it as the work
  goes; those commits are working notes, not the record.
- **`main` takes one commit per feature.** When the owner approves the branch,
  it is squashed and that single commit is added to `main`.
- A commit message names the packages and the behaviour, and says why.
- **History on `main` is never rewritten.** Squashing a branch that has been
  approved is the flow; amending, rebasing or resetting anything else is not,
  unless asked for it explicitly.
- No push and no other git operation without asking.

## Before every commit

```
go build ./... && go vet ./... && gofmt -l . && go mod tidy -diff && go test -race ./...
```

All five, every time. A commit with a wrong `go.mod` is redone, not patched on
top.

## Running and debugging

- **Build into a scratch directory.** `./paula`, `./paula.yaml` and `./data/`
  belong to the owner: never write or delete them, and ask before reading the
  conversation.
- A live run uses a scratch `data_dir` and a socket path short enough for a
  Unix socket (about a hundred bytes).
- **An API key is read from the environment**, under the name the runner's
  `token_env` gives. A key never goes into a file in the tree, a command line,
  a test or a log.
- `paula turns` and `paula turns -dump` are the window into what was really
  sent and received, including the tokens a host reported as cached.

## Editing

- Change a file with the Edit tool; write a whole new one with Write. Never
  rewrite a file through a shell script.
- Read a file before changing it, and read the result back.

## Not part of the tree

`paula.yaml`, `personas/paula.yaml` and `data/` are a working copy's own and
stay out of git. What ships is the example of each, `paula.example.yaml` and
`personas/paula.example.yaml`, and a setup begins by copying them.

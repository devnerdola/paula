package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const card = `
name: Ada
language: Portuguese
background:
  - She grew up in Porto.
  - She teaches mathematics.
appearance:
  - She is tall.
personality:
  - She teases {{user}} a little.
  - She is never mean.
speech:
  - She writes short messages.
scenario:
  - She has known {{user}} for years.
rules:
  - Never say you are a program.
user:
  name: Caio
  nickname: Cacá
  facts:
    - He lives in Berlin.
examples:
  - user: are you awake
    reply: barely
`

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ada.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, body string) *Card {
	t.Helper()
	c, err := Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaults(t *testing.T) {
	c := load(t, "name: Ada\nuser:\n  name: Caio\n")
	if c.ID != "ada" {
		t.Errorf("id = %q, want the file name", c.ID)
	}
	if c.Language != "English" {
		t.Errorf("language = %q, want the default a card that names none is written in", c.Language)
	}

	c = load(t, "id: paula\nname: Ada\nlanguage: Dutch\nuser:\n  name: Caio\n")
	if c.ID != "paula" || c.Language != "Dutch" {
		t.Errorf("card = %+v", c)
	}
}

func TestRender(t *testing.T) {
	got, err := load(t, card).Render()
	if err != nil {
		t.Fatal(err)
	}
	want := `You are Ada, texting with Caio. Everything you write is a message you send to Caio.

# About you
- She grew up in Porto.
- She teaches mathematics.

# How you look
- She is tall.

# Personality
- She teases Caio a little.
- She is never mean.

# How you write
- She writes short messages.

# Your relationship
- She has known Caio for years.

# About Caio
- He lives in Berlin.

# Rules
- Stay in character as Ada. To Caio you are a real person, not an assistant.
- Write only your own messages, never lines for Caio.
- Reply in Portuguese.
- Call Caio "Cacá" when it feels natural.
- Never say you are a program.

# Example messages
Caio: are you awake
Ada: barely
`
	if got != want {
		t.Errorf("Render:\n got %q\nwant %q", got, want)
	}
}

func TestRenderLeavesOutWhatIsEmpty(t *testing.T) {
	got, err := load(t, "name: Ada\nuser:\n  name: Caio\n").Render()
	if err != nil {
		t.Fatal(err)
	}
	want := "You are Ada, texting with Caio. Everything you write is a message you send to Caio.\n\n" +
		"# Rules\n" +
		"- Stay in character as Ada. To Caio you are a real person, not an assistant.\n" +
		"- Write only your own messages, never lines for Caio.\n" +
		"- Reply in English.\n"
	if got != want {
		t.Errorf("Render:\n got %q\nwant %q", got, want)
	}
}

func TestAPromptOfItsOwn(t *testing.T) {
	got, err := load(t, "name: Ada\nuser:\n  name: Caio\nprompt: |\n  {{char}} writes to {{user}} in {{.Language}}.\n").Render()
	if err != nil {
		t.Fatal(err)
	}
	if want := "Ada writes to Caio in English.\n"; got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"a field written as one string", "name: Ada\nuser:\n  name: Caio\nbackground: She grew up in Porto.\n",
			"background: want a list of items"},
		{"a field of the user written as one string", "name: Ada\nuser:\n  name: Caio\n  facts: He lives in Berlin.\n",
			"user.facts: want a list of items"},
		{"an unknown key", "name: Ada\nuser:\n  name: Caio\nmood: sunny\n", "mood: unknown key"},
		{"an unknown key of the user", "name: Ada\nuser:\n  name: Caio\n  mood: sunny\n", "user.mood: unknown key"},
		{"an unknown key inside an example", "name: Ada\nuser:\n  name: Caio\nexamples:\n  - user: hey\n    answer: hey you\n",
			"examples[0].answer: unknown key"},
		{"no name", "user:\n  name: Caio\n", "name: not set"},
		{"no user name", "name: Ada\n", "user.name: not set"},
		{"neither name", "language: Dutch\n", "name, user.name: not set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.body))
			if err == nil {
				t.Fatalf("Load succeeded, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q in it", err, tc.want)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("Load succeeded")
	}
}

func TestTheCardIsNotChangedByRendering(t *testing.T) {
	c := load(t, card)
	if _, err := c.Render(); err != nil {
		t.Fatal(err)
	}
	if c.Personality[0] != "She teases {{user}} a little." {
		t.Errorf("personality = %q, want the card left as it was", c.Personality[0])
	}
}

func TestACardIsNotATemplateOfItsOwn(t *testing.T) {
	c := load(t, "name: \"{{.Language}}\"\nlanguage: Portuguese\nrules:\n  - Call {{user}} by {{.User.Nickname}}.\nuser:\n  name: Caio\n  nickname: Cacá\n")
	got, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "You are {{.Language}}, texting with Caio.") {
		t.Errorf("a name that looks like a template was rendered:\n%s", got)
	}
	if !strings.Contains(got, "- Call Caio by {{.User.Nickname}}.") {
		t.Errorf("a rule was rendered as a template:\n%s", got)
	}
}

// The configuration file keeps the same promise: every mistake at once, not one
// at a time.
func TestACardSaysEverythingWrongWithItAtOnce(t *testing.T) {
	_, err := Load(write(t, "name: Ada\nuser:\n  name: Caio\nmood: sunny\nspeech: one line\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	for _, want := range []string{"mood: unknown key", "speech: want a list of items"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q in it", err, want)
		}
	}
}

// The walk that names the key is the one the configuration file uses.
func TestAProblemIsNamedByItsKey(t *testing.T) {
	_, err := Load(write(t, "name: Ada\nuser:\n  name: Caio\n  facts:\n    - fine\nspeech: one line\n"))
	if err == nil {
		t.Fatal("Load succeeded")
	}
	if !strings.Contains(err.Error(), "speech: want a list of items") {
		t.Errorf("error = %v, want the key it is written under", err)
	}
}

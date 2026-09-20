// Package persona reads a character card and renders the system message that
// tells a model who it is.
package persona

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"

	"nerdola.dev/x/paula/internal/config"
)

// defaultLanguage is what a card that names none is written in.
const defaultLanguage = "English"

//go:embed persona.tmpl
var defaultTemplate string

// List is a descriptive field, which holds one item per thing it says.
type List []string

func (l *List) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: want a list of items, one per line", n.Line)
	}
	var out []string
	if err := n.Decode(&out); err != nil {
		return err
	}
	*l = out
	return nil
}

type Card struct {
	ID          string    `yaml:"id"`
	Name        string    `yaml:"name"`
	Language    string    `yaml:"language"`
	Background  List      `yaml:"background"`
	Appearance  List      `yaml:"appearance"`
	Personality List      `yaml:"personality"`
	Speech      List      `yaml:"speech"`
	Scenario    List      `yaml:"scenario"`
	Rules       List      `yaml:"rules"`
	User        User      `yaml:"user"`
	Examples    []Example `yaml:"examples"`
	Prompt      string    `yaml:"prompt"`
}

type User struct {
	Name     string `yaml:"name"`
	Nickname string `yaml:"nickname"`
	Facts    List   `yaml:"facts"`
}

type Example struct {
	User  string `yaml:"user"`
	Reply string `yaml:"reply"`
}

// Load reads a character card, rejecting keys it does not have. A card is read
// the way the configuration file is, so every problem with one is reported at
// once and each is named by the key it is written under.
func Load(path string) (*Card, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root, err := config.Root(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var c Card
	if err := root.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	if c.ID == "" {
		c.ID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if c.Language == "" {
		c.Language = defaultLanguage
	}

	var missing []string
	if c.Name == "" {
		missing = append(missing, "name")
	}
	if c.User.Name == "" {
		missing = append(missing, "user.name")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: %s: not set", path, strings.Join(missing, ", "))
	}
	return &c, nil
}

// Render is the system message the card describes.
func (c *Card) Render() (string, error) {
	text := defaultTemplate
	if c.Prompt != "" {
		text = c.Prompt
	}
	// The names are template actions rather than text laid into the template, so
	// a name holding one is never rendered as one.
	t, err := template.New("persona").Funcs(template.FuncMap{
		"char": func() string { return c.Name },
		"user": func() string { return c.User.Name },
	}).Parse(text)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, c); err != nil {
		return "", err
	}
	// The card is written with {{char}} and {{user}} in its own lines, which are
	// filled in once everything is written out: the fields go to the template as
	// they are, so replacing them there and here comes to the same text.
	replace := strings.NewReplacer("{{char}}", c.Name, "{{user}}", c.User.Name)
	return strings.TrimRight(replace.Replace(out.String()), "\n") + "\n", nil
}

// Package config reads the files Paula is configured by, rejecting keys Paula
// does not know. It reads paula.yaml itself, and holds the rules a file of
// Paula's is read under -- every problem at once, each named by the key it is
// written at -- which the character card is read by too.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Role is what a model is chosen for.
type Role string

const (
	RoleChat   Role = "chat"
	RoleVision Role = "vision"
	RoleEmbed  Role = "embed"
)

// Roles are every role, in the order they are shown.
var Roles = []Role{RoleChat, RoleVision, RoleEmbed}

// Config is a loaded paula.yaml.
type Config struct {
	Path          string
	Dir           string
	Persona       string
	DataDir       string
	Runners       []Runner
	Models        []Model
	DefaultModels DefaultModels
	Engine        Engine
	Frontends     []Frontend
}

// Runner is one entry of the runners mapping. Its section holds every key,
// including the type, for the runner package to decode.
type Runner struct {
	Name    string
	Type    string
	Section Section
}

// Model is one entry of the models mapping, in the order it was written.
type Model struct {
	Name    string
	Runner  string
	ID      string
	Context int
	Section Section
}

// Frontend is one entry of the frontends mapping.
type Frontend struct {
	Name    string
	Section Section
}

// FrontendSection is the section a frontend of that name was written under, and
// an unwritten section when the file names none: a command that reaches one
// frontend asks for it rather than looking through them.
func (c *Config) FrontendSection(name string) Section {
	for _, f := range c.Frontends {
		if f.Name == name {
			return f.Section
		}
	}
	return Section{}
}

type DefaultModels struct {
	Chat   string `yaml:"chat"`
	Vision string `yaml:"vision"`
	Embed  string `yaml:"embed"`
}

func (d DefaultModels) Get(r Role) string {
	switch r {
	case RoleChat:
		return d.Chat
	case RoleVision:
		return d.Vision
	case RoleEmbed:
		return d.Embed
	}
	return ""
}

type Engine struct {
	Debounce      Duration `yaml:"debounce"`
	PrefillCancel bool     `yaml:"prefill_cancel"`
	// SystemRatio is the share of the context the system message may take, and
	// MemoryRatio the share of what is left of it, once the card is written,
	// that memories may take; the summary takes the rest. HistoryKeep is the
	// share of the messages' part of the context a fold leaves behind.
	SystemRatio float64 `yaml:"system_ratio"`
	MemoryRatio float64 `yaml:"memory_ratio"`
	HistoryKeep float64 `yaml:"history_keep"`
	ImageTurns  int     `yaml:"image_turns"`
	ImageMaxPx  int     `yaml:"image_max_px"`
	LogKeep     int     `yaml:"log_keep"`
}

// DefaultEngine is the engine section a file that writes none gets.
func DefaultEngine() Engine {
	return Engine{
		Debounce:      Duration(2 * time.Second),
		PrefillCancel: true,
		SystemRatio:   0.5,
		MemoryRatio:   0.5,
		HistoryKeep:   0.5,
		ImageTurns:    2,
		ImageMaxPx:    1024,
		LogKeep:       500,
	}
}

// Duration is a time.Duration written the way time.ParseDuration reads it.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }
func (d Duration) String() string          { return time.Duration(d).String() }

// UnmarshalYAML reports the line the duration was written on, so the error can
// name the key it belongs to.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: want a duration such as 2s or 1m30s", n.Line)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration such as 2s or 1m30s", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type file struct {
	Persona       string        `yaml:"persona"`
	DataDir       string        `yaml:"data_dir"`
	Runners       yaml.Node     `yaml:"runners"`
	Models        yaml.Node     `yaml:"models"`
	DefaultModels DefaultModels `yaml:"default_models"`
	Engine        Engine        `yaml:"engine"`
	Frontends     yaml.Node     `yaml:"frontends"`
}

// Find returns the configuration file to read: the given path, then
// $PAULA_CONFIG, then paula.yaml in the working directory.
func Find(path string) string {
	if path != "" {
		return path
	}
	if p := os.Getenv("PAULA_CONFIG"); p != "" {
		return p
	}
	return "paula.yaml"
}

// Load reads the configuration file and reports every problem it holds.
func Load(path string) (*Config, error) {
	path = Find(path)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)

	root, err := Root(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f := file{Engine: DefaultEngine()}
	if err := root.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	c := &Config{
		Path:          path,
		Dir:           dir,
		Persona:       Resolve(dir, f.Persona),
		DataDir:       dataDir(dir, f.DataDir),
		DefaultModels: f.DefaultModels,
		Engine:        f.Engine,
	}
	p := &Problems{Path: path}

	if f.Persona == "" {
		p.Addf("persona: no character card is set")
	}
	c.Runners = readRunners(p, &f.Runners)
	c.Models = readModels(p, &f.Models, c.Runners)
	c.Frontends = readFrontends(p, &f.Frontends)
	checkDefaultModels(p, c)
	checkEngine(p, c.Engine)

	if err := p.Err(); err != nil {
		return nil, err
	}
	return c, nil
}

// dataDir is where the conversation is kept: what the file says, or a data
// directory beside the file.
func dataDir(dir, set string) string {
	if set == "" {
		return filepath.Join(dir, "data")
	}
	return Resolve(dir, set)
}

// readRunners reads the runners mapping, keeping every key of a runner for its
// own package to decode.
func readRunners(p *Problems, n *yaml.Node) []Runner {
	entries, err := entries(n, "runners")
	if err != nil {
		p.Add(err)
	}
	var out []Runner
	for _, e := range entries {
		var head struct {
			Type string `yaml:"type"`
		}
		if err := peek(e.section, &head); err != nil {
			p.Add(err)
		}
		if head.Type == "" {
			p.Addf("runners.%s.type: no type is set", e.name)
		}
		out = append(out, Runner{Name: e.name, Type: head.Type, Section: e.section})
	}
	if len(out) == 0 {
		p.Addf("runners: at least one runner is needed")
	}
	return out
}

// readModels reads the models mapping, in the order it was written, and holds
// every model to a name, a runner that is there, and an id.
func readModels(p *Problems, n *yaml.Node, runners []Runner) []Model {
	entries, err := entries(n, "models")
	if err != nil {
		p.Add(err)
	}
	var out []Model
	for _, e := range entries {
		if !modelName.MatchString(e.name) {
			p.Addf("models.%s: a name holds letters, digits, dots, underscores and dashes, and starts with a letter or a digit", e.name)
		}
		var head struct {
			Runner  string `yaml:"runner"`
			ID      string `yaml:"id"`
			Context int    `yaml:"context"`
		}
		if err := peek(e.section, &head); err != nil {
			p.Add(err)
		}
		if head.Runner == "" {
			p.Addf("models.%s.runner: no runner is set", e.name)
		} else if !hasRunner(runners, head.Runner) {
			p.Addf("models.%s.runner: no runner is called %q", e.name, head.Runner)
		}
		if head.ID == "" {
			p.Addf("models.%s.id: no id is set", e.name)
		}
		if head.Context < 0 {
			p.Addf("models.%s.context: %d is below zero", e.name, head.Context)
		}
		out = append(out, Model{
			Name:    e.name,
			Runner:  head.Runner,
			ID:      head.ID,
			Context: head.Context,
			Section: e.section,
		})
	}
	return out
}

func readFrontends(p *Problems, n *yaml.Node) []Frontend {
	entries, err := entries(n, "frontends")
	if err != nil {
		p.Add(err)
		return nil
	}
	var out []Frontend
	for _, e := range entries {
		out = append(out, Frontend{Name: e.name, Section: e.section})
	}
	return out
}

// checkDefaultModels holds every role to a model the file names. Only chat is
// needed to read a file at all: what serving takes of it is serve's to say,
// since the commands that only read a conversation run without it.
func checkDefaultModels(p *Problems, c *Config) {
	if c.DefaultModels.Chat == "" {
		p.Addf("default_models.chat: no model is set")
	}
	for _, role := range Roles {
		name := c.DefaultModels.Get(role)
		if name != "" && c.Model(name) == nil {
			p.Addf("default_models.%s: no model is called %q", role, name)
		}
	}
}

// Model returns the configured model of that name.
func (c *Config) Model(name string) *Model {
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i]
		}
	}
	return nil
}

func checkEngine(p *Problems, e Engine) {
	if e.Debounce < 0 {
		p.Addf("engine.debounce: %s is below zero", e.Debounce)
	}
	// A share of nothing leaves a prompt no room, and a share of everything
	// leaves the other side of the split none.
	if e.SystemRatio <= 0 || e.SystemRatio >= 1 {
		p.Addf("engine.system_ratio: %v is not above 0 and below 1", e.SystemRatio)
	}
	if e.MemoryRatio <= 0 || e.MemoryRatio > 1 {
		p.Addf("engine.memory_ratio: %v is not above 0 and at most 1", e.MemoryRatio)
	}
	if e.HistoryKeep <= 0 || e.HistoryKeep >= 1 {
		p.Addf("engine.history_keep: %v is not above 0 and below 1", e.HistoryKeep)
	}
	if e.ImageTurns < 0 {
		p.Addf("engine.image_turns: %d is below zero", e.ImageTurns)
	}
	if e.ImageMaxPx < 0 {
		p.Addf("engine.image_max_px: %d is below zero", e.ImageMaxPx)
	}
	if e.LogKeep < 0 {
		p.Addf("engine.log_keep: %d is below zero", e.LogKeep)
	}
}

// peek reads the few keys a section is looked at for before its own package
// decodes it. The keys it has no field for are left to that package.
func peek(s Section, v any) error {
	if s.node == nil {
		return nil
	}
	if err := s.node.Decode(v); err != nil {
		return s.problem(s.named(err))
	}
	return nil
}

func hasRunner(rs []Runner, name string) bool {
	for _, r := range rs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// Resolve reads a path the way the configuration file means it: relative to
// the file's directory, and ~/ from the home directory.
func Resolve(dir, p string) string {
	switch {
	case p == "":
		return ""
	case strings.HasPrefix(p, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[2:])
	case filepath.IsAbs(p):
		return p
	}
	return filepath.Join(dir, p)
}

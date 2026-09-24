package config

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Section is the part of a configuration file whose keys only one package
// knows, kept as the node it was written as so that package decodes it itself.
type Section struct {
	path string
	node *yaml.Node
}

// Root reads a whole YAML document as a section, under the same rules as the
// configuration file. A document with nothing in it is a section that was never
// written.
func Root(b []byte) (Section, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return Section{}, errors.New(clean(err.Error()))
	}
	if len(doc.Content) == 0 {
		return Section{}, nil
	}
	return Section{node: doc.Content[0]}, nil
}

// Path is the key path the section was read from, such as runners.openrouter.
func (s Section) Path() string { return s.path }

// under is the key path of a key written inside this section.
func (s Section) under(key string) string {
	if s.path == "" {
		return key
	}
	return s.path + "." + key
}

// Set reports whether the key was written at all.
func (s Section) Set() bool { return s.node != nil }

// Decode decodes the section into v, rejecting keys v has no field for. The
// node carries the lines of the file it was read from, so every problem is
// reported under the key it is about.
func (s Section) Decode(v any) error {
	if s.node == nil {
		return nil
	}
	var problems []string
	if err := s.node.Decode(v); err != nil {
		problems = append(problems, s.named(err)...)
	}
	problems = append(problems, unknown(s.node, reflect.TypeOf(v), "")...)
	s.label(reflect.ValueOf(v))
	return s.problem(problems)
}

// label gives every section decoded out of this one the key path it was written
// under, so whoever decodes it next reports problems where the file has them
// without being told where that is.
func (s Section) label(v reflect.Value) {
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return
	}
	for name, f := range fieldsOf(v.Type()) {
		if f.Type != sectionType {
			continue
		}
		// A section inside an inlined struct is reached through it, and a nil
		// pointer on the way is a struct nothing was decoded into.
		field, err := v.FieldByIndexErr(f.Index)
		if err != nil || !field.CanSet() {
			continue
		}
		inner := field.Interface().(Section)
		inner.path = s.under(name)
		field.Set(reflect.ValueOf(inner))
	}
}

func (s *Section) UnmarshalYAML(n *yaml.Node) error {
	s.node = n
	return nil
}

// problem is everything wrong with a section, under its key path.
func (s Section) problem(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	msg := strings.Join(problems, "; ")
	if s.path == "" {
		return errors.New(msg)
	}
	return fmt.Errorf("%s: %s", s.path, msg)
}

var (
	yamlNoise = regexp.MustCompile(`(?: in type [^\s]+|^line \d+: )`)
	yamlLead  = regexp.MustCompile(`^yaml: (?:unmarshal errors:\n\s*)?`)
)

// named says what yaml reported, one problem to a line, under the key each one
// is about.
func (s Section) named(err error) []string {
	var out []string
	for line := range strings.SplitSeq(yamlLead.ReplaceAllString(err.Error(), ""), "\n") {
		msg := strings.TrimSpace(line)
		if msg == "" {
			continue
		}
		out = append(out, s.about(msg))
	}
	return out
}

// about names the key a problem is written under, read from the line yaml
// reports it on.
func (s Section) about(msg string) string {
	var line int
	if _, err := fmt.Sscanf(msg, "line %d:", &line); err == nil && s.node != nil {
		if key := keyAt(s.node, line, ""); key != "" {
			_, rest, _ := strings.Cut(msg, ": ")
			return key + ": " + clean(rest)
		}
	}
	return clean(msg)
}

func clean(msg string) string {
	msg = yamlNoise.ReplaceAllString(msg, "")
	return strings.Join(strings.Fields(msg), " ")
}

// keyAt is the dotted path of the key written on a line of a node.
func keyAt(n *yaml.Node, line int, path string) string {
	if n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		here := key.Value
		if path != "" {
			here = path + "." + key.Value
		}
		// The deepest key on the line is the one it is written under.
		if deeper := keyAt(value, line, here); deeper != "" {
			return deeper
		}
		if key.Line == line || value.Line == line {
			return here
		}
	}
	return ""
}

// unknown reports the keys of a node that a value of type t has no field for,
// as the dotted paths they are written under. The node is read as plain values
// first, since that is where yaml follows an alias and lays in the keys of a
// merge, leaving the keys the file means and nothing else.
func unknown(n *yaml.Node, t reflect.Type, path string) []string {
	var v any
	if err := n.Decode(&v); err != nil {
		// A file that cannot be read as plain values is one yaml itself has
		// something to say about, which the decode into t reports.
		return nil
	}
	return unknownIn(v, t, path)
}

// unknownIn holds what was read against the type it is decoded into.
func unknownIn(v any, t reflect.Type, path string) []string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return nil
	}
	switch read := v.(type) {
	case []any:
		// A list of mappings is held to what one of them holds, so a key written
		// inside an item of it is named under the item it is in.
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
			return nil
		}
		var out []string
		for i, item := range read {
			out = append(out, unknownIn(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i))...)
		}
		return out

	case map[string]any:
		if t.Kind() != reflect.Struct {
			return nil
		}
		fields := fieldsOf(t)
		var out []string
		// In the order the keys read, since a mapping read this way no longer
		// holds the order they were written in.
		for _, key := range slices.Sorted(maps.Keys(read)) {
			here := key
			if path != "" {
				here = path + "." + key
			}
			f, ok := fields[key]
			if !ok {
				out = append(out, here+": unknown key")
				continue
			}
			// A value that decodes itself holds its own keys to its own rules.
			if decodesItself(f.Type) {
				continue
			}
			out = append(out, unknownIn(read[key], f.Type, here)...)
		}
		return out
	}
	return nil
}

// fieldsOf are the keys a struct has a field for, by the name yaml reads them
// under. The fields of an inlined struct are keys of the struct that inlines
// it, and each one carries the index it has in the struct asked about, so a
// value of it can be reached from there.
func fieldsOf(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		switch {
		case name == "-":
			continue
		case tagged(opts, "inline"):
			inlined := f.Type
			for inlined.Kind() == reflect.Pointer {
				inlined = inlined.Elem()
			}
			if inlined.Kind() == reflect.Struct {
				for key, field := range fieldsOf(inlined) {
					field.Index = append([]int{i}, field.Index...)
					out[key] = field
				}
			}
		case name == "":
			out[strings.ToLower(f.Name)] = f
		default:
			out[name] = f
		}
	}
	return out
}

// tagged reports whether the options of a yaml tag hold one.
func tagged(opts, want string) bool {
	for opt := range strings.SplitSeq(opts, ",") {
		if opt == want {
			return true
		}
	}
	return false
}

var (
	unmarshaler = reflect.TypeFor[yaml.Unmarshaler]()
	nodeType    = reflect.TypeFor[yaml.Node]()
	sectionType = reflect.TypeFor[Section]()
)

// decodesItself reports whether a type reads its own keys, which is a yaml
// node kept as it was written, or a value with an UnmarshalYAML of its own.
func decodesItself(t reflect.Type) bool {
	return t == nodeType || t.Implements(unmarshaler) || reflect.PointerTo(t).Implements(unmarshaler)
}

// MergeSections lays over onto base key by key, and returns base unchanged
// where over says nothing.
func MergeSections(base, over Section) Section {
	switch {
	case !over.Set():
		return base
	case !base.Set():
		return over
	}
	return Section{path: over.path, node: mergeNodes(base.node, over.node)}
}

func mergeNodes(base, over *yaml.Node) *yaml.Node {
	// An alias is the mapping it names, so a block shared by an anchor merges
	// the way it would written out.
	base, over = resolved(base), resolved(over)
	if base.Kind != yaml.MappingNode || over.Kind != yaml.MappingNode {
		return over
	}
	out := *base
	out.Content = append([]*yaml.Node(nil), base.Content...)
	for i := 0; i+1 < len(over.Content); i += 2 {
		k, v := over.Content[i], over.Content[i+1]
		replaced := false
		for j := 0; j+1 < len(out.Content); j += 2 {
			if out.Content[j].Value == k.Value {
				out.Content[j+1] = mergeNodes(out.Content[j+1], v)
				replaced = true
				break
			}
		}
		if !replaced {
			out.Content = append(out.Content, k, v)
		}
	}
	return &out
}

// resolved is the node an alias names, and any other node itself.
func resolved(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

type entry struct {
	name    string
	section Section
}

func entries(n *yaml.Node, prefix string) ([]entry, error) {
	if n.Kind == 0 || n.Tag == "!!null" {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: want a mapping of names to settings", prefix)
	}
	out := make([]entry, 0, len(n.Content)/2)
	seen := map[string]bool{}
	// A name written twice is reported, and the rest of the mapping is read all
	// the same, so one duplicate does not read as a file with nothing in it.
	var problems []error
	for i := 0; i+1 < len(n.Content); i += 2 {
		name := n.Content[i].Value
		if seen[name] {
			problems = append(problems, fmt.Errorf("%s.%s: written twice", prefix, name))
			continue
		}
		seen[name] = true
		out = append(out, entry{name: name, section: Section{
			path: prefix + "." + name,
			node: n.Content[i+1],
		}})
	}
	return out, errors.Join(problems...)
}

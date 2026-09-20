package api

import (
	"errors"
	"fmt"
	"strings"
)

// Problems collects what is wrong with a configuration file, so all of it is
// reported at once instead of only the first of it. A Path, when it is set, is
// printed above the list.
type Problems struct {
	Path string
	list []error
}

func (p *Problems) Add(errs ...error) {
	for _, err := range errs {
		if err != nil {
			p.list = append(p.list, err)
		}
	}
}

func (p *Problems) Addf(format string, a ...any) { p.Add(fmt.Errorf(format, a...)) }

// All is everything that was added, for a caller that heads the list with a
// path of its own.
func (p *Problems) All() []error { return p.list }

// Err returns everything that was added, or nil when nothing was.
func (p *Problems) Err() error {
	switch {
	case len(p.list) == 0:
		return nil
	case len(p.list) == 1 && p.Path == "":
		return p.list[0]
	case len(p.list) == 1:
		return fmt.Errorf("%s: %w", p.Path, p.list[0])
	}
	var b strings.Builder
	if p.Path != "" {
		fmt.Fprintf(&b, "%s:", p.Path)
		for _, err := range p.list {
			fmt.Fprintf(&b, "\n  %s", err)
		}
		return errors.New(b.String())
	}
	for i, err := range p.list {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(err.Error())
	}
	return errors.New(b.String())
}

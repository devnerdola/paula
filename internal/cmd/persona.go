package cmd

import (
	"flag"
	"fmt"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/persona"
)

func personaCheckCommand() *command {
	return &command{
		name:  "persona-check",
		short: "print the persona as a model is given it",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("no arguments are taken")
				}
				cfg, err := config.Load(g.config)
				if err != nil {
					return err
				}
				card, err := persona.Load(cfg.Persona)
				if err != nil {
					return err
				}
				text, err := card.Render()
				if err != nil {
					return fmt.Errorf("%s: %w", cfg.Persona, err)
				}
				fmt.Fprint(g.stdout, text)
				return nil
			}
		},
	}
}

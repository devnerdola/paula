package cmd

import (
	"flag"
	"fmt"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/persona"
)

func personaCommand() *command {
	return &command{
		name:  "persona",
		args:  "check",
		short: "the character card",
		subs: []*command{
			personaCheckCommand(),
		},
		flags: oneOfItsCommands,
	}
}

func personaCheckCommand() *command {
	return &command{
		name:  "check",
		short: "print the card as its template renders it, or what is wrong with it",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("check takes no arguments")
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

package cmd

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/conversation"
	"nerdola.dev/x/paula/internal/frontend"
	frontendapi "nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/store"
	"nerdola.dev/x/paula/internal/tools"
)

// lockFile is the file a run holds in the data directory, so a second one does
// not open the same conversation.
const lockFile = "paula.lock"

func serveCommand() *command {
	return &command{
		name:  "serve",
		short: "keep the conversation and the frontends running",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("no arguments are taken")
				}
				return serve(g)
			}
		},
	}
}

func serve(g *globals) error {
	cfg, err := config.Load(g.config)
	if err != nil {
		return err
	}
	card, err := persona.Load(cfg.Persona)
	if err != nil {
		return err
	}
	log := g.logger()
	set, err := runners.Configure(cfg, runners.Host{Log: log, Secrets: g.secrets})
	if err != nil {
		return err
	}
	var offered []tools.Tool
	for _, t := range cfg.Tools {
		opened, err := tools.Open(t.Name, t.Section, tools.Host{
			Names:    tools.Names{Character: card.Name, User: card.User.Name},
			Language: card.Language,
		})
		if err != nil {
			return err
		}
		offered = append(offered, opened...)
	}
	unlock, err := lock(cfg.DataDir)
	if err != nil {
		return err
	}
	defer unlock()

	s, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer s.Close()

	// Memories are searched by what they mean, which takes a model that turns
	// text into a vector. The conversation is what searches them, so it is what
	// asks for one: every other command reads a conversation without searching
	// it, and runs on a file that names none.
	//
	// It is asked of the conversation rather than of the file, since a model it
	// was given serves the role wherever the file's default stands.
	embeds, err := conversation.RoleModel(g.ctx, s, set, config.RoleEmbed)
	if err != nil {
		return err
	}
	if embeds == nil {
		return fmt.Errorf("%s: default_models.embed: no model is set", cfg.Path)
	}

	// One context covers the conversation and every frontend, so they end
	// together.
	ctx, stop := signal.NotifyContext(g.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Every listing is read over the network, so a slow API is said out loud
	// before it is waited for.
	log.Info("checking the models")
	started := time.Now()
	if err := set.Check(ctx); err != nil {
		return err
	}
	log.Info("models checked", "duration", time.Since(started))

	conv, err := conversation.Open(ctx, conversation.Options{
		Store:   s,
		Runners: set,
		Persona: card,
		Engine:  cfg.Engine,
		Log:     log,
		Tools:   offered,
	})
	if err != nil {
		return err
	}
	defer conv.Close()

	// Every frontend is opened before any of them runs, so one that cannot
	// open is a startup error rather than a run with half of them.
	host := frontend.Host{
		DataDir: cfg.DataDir,
		Log:     log,
		Names:   frontend.Names{Character: card.Name, User: card.User.Name},
		Secrets: g.secrets,
	}
	opened := make([]frontendapi.Frontend, 0, len(cfg.Frontends))
	for _, f := range cfg.Frontends {
		open, err := frontend.Open(f.Name, f.Section, host)
		if err != nil {
			return err
		}
		opened = append(opened, open)
	}

	var running sync.WaitGroup
	var failed atomic.Bool
	for _, open := range opened {
		running.Go(func() {
			if err := frontend.Run(ctx, open, frontend.Options{Conv: conv, Log: log}); err != nil {
				// A frontend that stopped serving does not come back, so the
				// run ends rather than looking alive without it.
				log.Error("frontend", "kind", open.Kind(), "error", err)
				failed.Store(true)
				stop()
			}
		})
	}

	log.Info("serving", "data_dir", cfg.DataDir, "frontends", len(cfg.Frontends))
	<-ctx.Done()
	log.Info("stopping")
	running.Wait()
	if failed.Load() {
		return errors.New("a frontend stopped serving")
	}
	return nil
}

// lock holds the data directory for this run, so two of them never keep the
// same conversation.
func lock(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, lockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("serve is already running on %s", dataDir)
		}
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

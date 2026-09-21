// Package repl is the terminal Paula is talked to from: a server on a socket
// in the data directory, and the client that dials it.
package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/api"
)

// Kind is the type written in the configuration file.
const Kind = "repl"

// defaultSocket is the socket inside the data directory.
const defaultSocket = "paula.sock"

// socketMax is the longest a socket path may be, which macOS is strictest
// about.
const socketMax = 103

type settings struct {
	Socket string `yaml:"socket"`
}

type Frontend struct {
	socket string
	log    *slog.Logger
	names  api.Names
}

// Socket is where the repl of a configuration listens, and what the client
// dials. A section that was never written gives the default.
func Socket(s config.Section, dataDir string) (string, error) {
	cfg := settings{Socket: filepath.Join(dataDir, defaultSocket)}
	if err := s.Decode(&cfg); err != nil {
		return "", err
	}
	socket := config.Resolve(dataDir, cfg.Socket)
	if len(socket) > socketMax {
		return "", fmt.Errorf(
			"%s.socket: %s is %d bytes, longer than the %d a socket path may have; set %s.socket to a shorter path",
			s.Path(), socket, len(socket), socketMax, s.Path())
	}
	return socket, nil
}

// Open reads the repl section and builds the server it describes.
func Open(s config.Section, h api.Host) (*Frontend, error) {
	socket, err := Socket(s, h.DataDir)
	if err != nil {
		return nil, err
	}
	log := h.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Frontend{socket: socket, log: log, names: h.Names}, nil
}

func (f *Frontend) Kind() string { return Kind }

// Socket is where it listens.
func (f *Frontend) Socket() string { return f.socket }

// Run listens on the socket and keeps a session for every connection, until
// the context ends.
func (f *Frontend) Run(ctx context.Context, session func(context.Context, api.Adapter) error) error {
	// A socket a run that was killed left behind refuses connections, and is
	// cleared away. One that answers belongs to something still listening.
	if c, err := net.Dial("unix", f.socket); err == nil {
		c.Close()
		return fmt.Errorf("something is already listening on %s", f.socket)
	}
	if fi, err := os.Stat(f.socket); err == nil && fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a socket", f.socket)
	}
	if err := os.Remove(f.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", f.socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(f.socket, 0o600); err != nil {
		l.Close()
		return err
	}
	f.log.Info("repl listening", "socket", f.socket)

	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !passing(err) {
				return err
			}
			// The listener is still good: a connection that went away before it
			// was taken, or a machine with no room for another file right now.
			f.log.Warn("repl accept", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(acceptWait):
			}
			continue
		}
		wg.Go(func() {
			defer conn.Close()
			// A terminal that stopped reading blocks whoever writes to it, so
			// when the context ends the connection is given the time it takes
			// to say goodbye and no more.
			ended := make(chan struct{})
			defer close(ended)
			go func() {
				select {
				case <-ctx.Done():
					_ = conn.SetDeadline(time.Now().Add(goodbye))
				case <-ended:
				}
			}()
			if err := serve(ctx, conn, f.names, session); err != nil && !gone(err) {
				f.log.Error("repl session", "error", err)
			}
		})
	}
}

// acceptWait is how long the listener waits before taking connections again,
// after one it could not take.
const acceptWait = 100 * time.Millisecond

// goodbye is how long a session has to tell its terminal that the run is
// stopping, once the run is.
const goodbye = 500 * time.Millisecond

// passing reports whether the listener goes on after an error taking a
// connection, rather than ending.
func passing(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EINTR)
}

// gone reports whether an error means the terminal is no longer there, which
// is how a terminal ends rather than something that went wrong.
func gone(err error) bool {
	return errors.Is(err, api.ErrGone) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

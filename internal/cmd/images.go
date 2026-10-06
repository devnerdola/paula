package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"

	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/store"
)

// defaultImages is how many pictures are listed when no number is asked for.
const defaultImages = 20

func imagesCommand() *command {
	return &command{
		name:  "images",
		args:  "list|show",
		short: "the pictures of the conversation, the ones you sent and the photos she sent",
		subs: []*command{
			imagesListCommand(),
			imagesShowCommand(),
		},
		flags: oneOfItsCommands,
	}
}

func imagesListCommand() *command {
	return &command{
		name:  "list",
		args:  "[-n N] [-from N]",
		short: "the newest pictures: when each was sent, by whom, and what it showed",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			n := fs.Int("n", defaultImages, "how many pictures to list")
			from := fs.Int("from", 0, "how many of the newest to pass over")
			return func(g *globals, args []string) error {
				if len(args) > 0 {
					return usagef("list takes no arguments")
				}
				if *n < 1 {
					return usagef("-n takes a count above zero")
				}
				if *from < 0 {
					return usagef("-from takes a count of zero or more")
				}
				cfg, s, err := reading(g)
				if err != nil {
					return err
				}
				defer s.Close()
				// The card names the two of them, which is who a picture is
				// listed as sent by.
				card, err := persona.Load(cfg.Persona)
				if err != nil {
					return err
				}

				images, total, err := s.ImagesFrom(context.Background(), *from, *n)
				if err != nil {
					return err
				}
				switch {
				case total == 0:
					fmt.Fprintln(g.stdout, "no pictures")
					return nil
				case len(images) == 0:
					fmt.Fprintf(g.stdout, "no pictures past the newest %d: there are %d in all\n", *from, total)
					return nil
				}
				if err := listImages(g.stdout, card, images); err != nil {
					return err
				}
				fmt.Fprintf(g.stdout, "\n%d to %d of %s\n", *from+1, *from+len(images), counted(total, "picture", "pictures"))
				return nil
			}
		},
	}
}

func imagesShowCommand() *command {
	return &command{
		name:  "show",
		args:  "ID",
		short: "one picture: when it was sent and by whom, its file, and what it showed",
		flags: func(fs *flag.FlagSet) func(*globals, []string) error {
			return func(g *globals, args []string) error {
				id, err := oneID(args, "picture")
				if err != nil {
					return err
				}
				cfg, s, err := reading(g)
				if err != nil {
					return err
				}
				defer s.Close()
				card, err := persona.Load(cfg.Persona)
				if err != nil {
					return err
				}

				img, err := s.Image(context.Background(), id)
				if err != nil {
					return err
				}
				w := g.stdout
				fmt.Fprintf(w, "picture %d\n", img.ID)
				fmt.Fprintf(w, "sent    %s by %s, in message %d\n", stamp(img.SentAt), sender(card, *img), img.MessageID)
				fmt.Fprintf(w, "file    %s\n", media.New(cfg.DataDir, 0).Path(img.SHA256))
				fmt.Fprintf(w, "shows   %s\n", showed(*img))
				if img.CaptionError != "" {
					fmt.Fprintf(w, "error   %s\n", img.CaptionError)
				}
				return nil
			}
		},
	}
}

// listImages writes the pictures out, newest first, the number each is looked
// at by first.
func listImages(w io.Writer, card *persona.Card, images []store.Image) error {
	table(w, []string{"ID", "SENT", "BY", "MESSAGE", "SHOWED"}, func(row func(...string)) {
		for _, img := range images {
			row(strconv.FormatInt(img.ID, 10), minute(img.SentAt), sender(card, img),
				strconv.FormatInt(int64(img.MessageID), 10), oneLine(showed(img)))
		}
	})
	return nil
}

// sender is who sent a picture, by the name the card gives them.
func sender(card *persona.Card, img store.Image) string {
	if img.Role == store.RoleAssistant {
		return card.Name
	}
	return card.User.Name
}

// showed is what a picture showed, as far as anything has said.
func showed(img store.Image) string {
	if img.Caption == "" {
		return "nothing was said of what it shows"
	}
	return img.Caption
}

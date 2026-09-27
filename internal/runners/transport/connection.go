package transport

import (
	"os"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/runners/api"
)

// Connection is what every runner is configured with: where its API is, the
// key, and how long and how often a request is tried. A runner decodes it with
// the defaults of its own API.
type Connection struct {
	// Type is what the kind table read to know which runner to open. It is
	// named here so that a section is decoded whole, since a section reports
	// any key it does not know.
	Type           string          `yaml:"type"`
	URL            string          `yaml:"url"`
	TokenEnv       string          `yaml:"token_env"`
	IdleTimeout    config.Duration `yaml:"idle_timeout"`
	RequestTimeout config.Duration `yaml:"request_timeout"`
	// Retries is a pointer so that nothing tells zero retries from a file that
	// asks for none. The defaults a runner passes set it.
	Retries *int `yaml:"retries"`

	token string
}

// DecodeConnection reads the section of a runner that serves no models, which
// takes the keys of its connection and nothing else.
func DecodeConnection(s config.Section, defaults Connection) (Connection, *api.Problems) {
	c := defaults
	p := &api.Problems{Path: s.Path()}
	if err := s.Decode(&c); err != nil {
		p.Add(err)
		return c, p
	}
	c.Check(p)
	return c, p
}

// Check reads the key from the environment, and reports what is wrong with
// the keys every runner takes.
func (c *Connection) Check(p *api.Problems) {
	c.token = os.Getenv(c.TokenEnv)
	switch {
	case c.token == "":
		p.Addf("token_env: environment variable %s is not set", c.TokenEnv)
	case len(c.token) < logs.MinSecret:
		// A value this short is no key, and it is too short to be redacted, so
		// it would be written to the log as it is.
		p.Addf("token_env: the value of %s is %d bytes, too short to be a key",
			c.TokenEnv, len(c.token))
	}
	switch {
	case c.Retries == nil:
		// The defaults set it, so nothing here is a key written with nothing
		// after it: a null takes the pointer away rather than leaving it be.
		p.Addf("retries: no number is written")
	case *c.Retries < 0:
		p.Addf("retries: %d is below zero", *c.Retries)
	}
	if c.IdleTimeout <= 0 {
		p.Addf("idle_timeout: %s is not above zero", c.IdleTimeout)
	}
	if c.RequestTimeout <= 0 {
		p.Addf("request_timeout: %s is not above zero", c.RequestTimeout)
	}
}

// Client is the client the configuration describes, answered for by the
// runner's own answers. The token is registered as a secret of the run, since
// from here on it is sent with every request.
func (c Connection) Client(name string, h api.Host, answers Answers) *Client {
	h.Secrets.Add(c.token)
	return &Client{
		Runner:         name,
		BaseURL:        c.URL,
		Token:          c.token,
		IdleTimeout:    c.IdleTimeout.Duration(),
		RequestTimeout: c.RequestTimeout.Duration(),
		Retries:        *c.Retries,
		Answers:        answers,
		Log:            h.Log,
		Secrets:        h.Secrets,
	}
}

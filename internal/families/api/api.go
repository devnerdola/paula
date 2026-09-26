// Package api is the vocabulary the family extensions and the table that opens
// them share. An extension imports it and nothing of the table, and nothing of
// another extension, so each one stands alone.
package api

import chat "nerdola.dev/x/paula/internal/runners/api"

// Extension is what one family of models takes on one provider.
type Extension = chat.Extension

// Open returns the extension for a model, caching the way the family's host
// caches best, or not at all when cache is false. A family whose host caches
// every prompt, with nothing that turns it off, says so for false.
type Open func(cache bool) (Extension, error)

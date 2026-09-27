package api

import "context"

// Searcher is a runner that searches the web and reads pages.
type Searcher interface {
	Name() string
	// MostResults is the most results a search may ask for, as the API
	// documents it.
	MostResults() int
	// Search is what a query finds, and Read the text of a page.
	Search(ctx context.Context, req SearchRequest) ([]SearchResult, error)
	Read(ctx context.Context, req PageRequest) (string, error)
}

// SearchRequest asks what a query finds on the web, at most Limit results of
// it. Like a chat request, it carries what records it.
type SearchRequest struct {
	Query    string
	Limit    int
	Recorder Recorder
}

// SearchResult is a page a search found: its title and address, a passage of
// it, and when it was published, as the API writes that, or empty when the
// API does not say.
type SearchResult struct {
	Title   string
	URL     string
	Content string
	Date    string
}

// PageRequest asks for the text of a page.
type PageRequest struct {
	URL      string
	Recorder Recorder
}

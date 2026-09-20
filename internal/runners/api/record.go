package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// APIError holds what the API itself said went wrong.
type APIError struct {
	Status  int
	Code    string
	Type    string
	Message string
}

func (e *APIError) Error() string {
	var b strings.Builder
	if e.Status != 0 {
		fmt.Fprintf(&b, "%d", e.Status)
	}
	if e.Type != "" {
		fmt.Fprintf(&b, " %s", e.Type)
	}
	if e.Code != "" && e.Code != e.Type {
		fmt.Fprintf(&b, " %s", e.Code)
	}
	if e.Message != "" {
		if b.Len() > 0 {
			b.WriteString(": ")
		}
		b.WriteString(e.Message)
	}
	return strings.TrimPrefix(b.String(), " ")
}

// Attempt is one try of a request.
type Attempt struct {
	StartedAt   time.Time
	FirstByteAt time.Time
	EndedAt     time.Time
	Status      int
	Error       string
	RetryAfter  time.Duration
}

// Record is everything sent and received for one request.
type Record struct {
	Runner          string
	Model           string
	Method          string
	URL             string
	RequestHeaders  http.Header
	RequestBody     []byte
	Attempts        []Attempt
	Status          int
	ResponseHeaders http.Header
	ResponseBody    []byte
	StartedAt       time.Time
	FirstByteAt     time.Time
	EndedAt         time.Time
	Error           string
	Provider        string
	FinishReason    string
	Usage           *Usage

	// ID is what the recorder gave the record when it started it.
	ID int64
}

// Recorder keeps every request under the entry that made it. A runner never
// stores anything itself.
type Recorder interface {
	StartRequest(ctx context.Context, r *Record) error
	EndRequest(ctx context.Context, r *Record) error
}

// Recording is the recorder a request is kept by, and one that keeps nothing
// when it was given none.
func Recording(r Recorder) Recorder {
	if r == nil {
		return noRecorder{}
	}
	return r
}

type noRecorder struct{}

func (noRecorder) StartRequest(context.Context, *Record) error { return nil }
func (noRecorder) EndRequest(context.Context, *Record) error   { return nil }

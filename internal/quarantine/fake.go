package quarantine

import (
	"context"
	"strings"
	"sync"
)

// Script is what a Fake does on its next calls: answer with Text (the
// placeholder {{canary}} becomes the marker found in the user message, to play
// a model that repeats its input), fail with Err, or, when Hang is set, wait
// until the context ends.
type Script struct {
	Text             string
	PromptTokens     *int
	CompletionTokens *int
	Err              error
	Hang             bool
}

// Fake is a Completer for tests. The zero value answers with an empty text.
type Fake struct {
	mu     sync.Mutex
	script Script
	last   Request
	calls  int
}

// Set replaces the script; it is safe to call while requests are served.
func (f *Fake) Set(s Script) {
	f.mu.Lock()
	f.script = s
	f.mu.Unlock()
}

// Complete implements Completer.
func (f *Fake) Complete(ctx context.Context, req Request) (Completion, error) {
	f.mu.Lock()
	f.last = req
	f.calls++
	s := f.script
	f.mu.Unlock()

	if s.Hang {
		<-ctx.Done()
		return Completion{}, ctx.Err()
	}
	if s.Err != nil {
		return Completion{}, s.Err
	}
	marker := ""
	if _, rest, ok := strings.Cut(req.User, "Note marker: "); ok {
		marker, _, _ = strings.Cut(rest, "\n")
	}
	return Completion{
		Text:         strings.ReplaceAll(s.Text, "{{canary}}", marker),
		PromptTokens: s.PromptTokens, CompletionTokens: s.CompletionTokens,
	}, nil
}

// Last returns the last request the fake received and how many it got.
func (f *Fake) Last() (Request, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.calls
}

package gmaps

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
)

// ErrSessionBlocked is returned by Scraper.Run when consecutive place-scrape
// failures indicate the Playwright session has been rate-limited by Google.
// The checkpoint is saved before returning; use --resume to continue.
var ErrSessionBlocked = errors.New("scraper: session blocked by Google — use --resume to continue")

// blockThreshold is the number of consecutive failures across all workers
// that triggers an ErrSessionBlocked return.
const blockThreshold = int64(10)

// Checkpoint persists scraping progress to disk so a blocked or interrupted
// run can be resumed without re-running the feed phase.
type Checkpoint struct {
	Queries []string `json:"queries"`
	URLs    []string `json:"urls"`
	Done    []string `json:"done"`

	path    string
	doneSet map[string]struct{}
	mu      sync.Mutex
}

// NewCheckpoint creates an in-memory checkpoint and writes the initial state
// (all URLs pending) to path.
func NewCheckpoint(path string, queries, urls []string) *Checkpoint {
	return &Checkpoint{
		Queries: queries,
		URLs:    urls,
		Done:    []string{},
		path:    path,
		doneSet: make(map[string]struct{}),
	}
}

// LoadCheckpoint reads and parses a checkpoint file from disk.
func LoadCheckpoint(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, err
	}
	cp.path = path
	cp.doneSet = make(map[string]struct{}, len(cp.Done))
	for _, u := range cp.Done {
		cp.doneSet[u] = struct{}{}
	}
	return &cp, nil
}

// IsDone reports whether url was successfully scraped in a previous run.
func (c *Checkpoint) IsDone(url string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.doneSet[url]
	return ok
}

// MarkDone records url as completed and flushes to disk. Safe for concurrent use.
func (c *Checkpoint) MarkDone(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.doneSet[url]; ok {
		return
	}
	c.doneSet[url] = struct{}{}
	c.Done = append(c.Done, url)
	_ = c.save() // best-effort; a missed flush is recoverable on resume
}

// Save flushes the current state to disk. Safe for concurrent use.
func (c *Checkpoint) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.save()
}

func (c *Checkpoint) save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o644)
}

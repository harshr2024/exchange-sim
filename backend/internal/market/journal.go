package market

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Record is one line of a session log: a command and the sequence number the
// market gave it.
//
// The sequence is recorded even though replay could re-derive it. Replay checks
// the two against each other, which turns a truncated or reordered log into an
// immediate error instead of a silently different market.
type Record struct {
	Sequence uint64  `json:"seq"`
	Command  Command `json:"cmd"`
}

// Journal is an append-only sink for sequenced commands.
type Journal interface {
	Append(Record) error
	Close() error
}

// FileJournal writes newline-delimited JSON records to one file per market.
//
// This is a replay log, not a write-ahead log. Records are flushed to the OS on
// every append so a concurrent reader sees them, but there is no fsync, no
// checkpointing and no torn-write detection, so it makes no crash-safety
// guarantee. Recovering a market after a kill -9 would need those things added
// and verified first.
type FileJournal struct {
	mu   sync.Mutex
	path string
	file *os.File
	buf  *bufio.Writer
}

// SessionFileName is the log file name for a symbol, e.g. "session-NOVA.ndjson".
func SessionFileName(symbol string) string {
	return fmt.Sprintf("session-%s.ndjson", symbol)
}

// OpenFileJournal creates or truncates the session log for symbol inside dir.
func OpenFileJournal(dir, symbol string) (*FileJournal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("market: creating session log directory: %w", err)
	}
	path := filepath.Join(dir, SessionFileName(symbol))
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("market: creating session log: %w", err)
	}
	return &FileJournal{path: path, file: f, buf: bufio.NewWriter(f)}, nil
}

// Path returns the file being written.
func (j *FileJournal) Path() string { return j.path }

// Append writes one record and flushes it to the operating system.
func (j *FileJournal) Append(r Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return errors.New("market: session log is closed")
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("market: encoding record %d: %w", r.Sequence, err)
	}
	if _, err := j.buf.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("market: writing record %d: %w", r.Sequence, err)
	}
	return j.buf.Flush()
}

// Close flushes and closes the file.
func (j *FileJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.buf.Flush()
	if cerr := j.file.Close(); err == nil {
		err = cerr
	}
	j.file = nil
	return err
}

// MemJournal keeps records in memory. Tests use it to capture the exact command
// stream a live market accepted and feed it straight back into a fresh one.
type MemJournal struct {
	mu      sync.Mutex
	records []Record
}

// NewMemJournal returns an empty in-memory journal.
func NewMemJournal() *MemJournal { return &MemJournal{} }

// Append stores the record.
func (j *MemJournal) Append(r Record) error {
	j.mu.Lock()
	j.records = append(j.records, r)
	j.mu.Unlock()
	return nil
}

// Close is a no-op.
func (j *MemJournal) Close() error { return nil }

// Records returns a copy of everything appended so far.
func (j *MemJournal) Records() []Record {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Record(nil), j.records...)
}

// ReadRecords parses a newline-delimited session log.
func ReadRecords(r io.Reader) ([]Record, error) {
	var out []Record
	sc := bufio.NewScanner(r)
	// Records are small, but a generous buffer avoids a confusing failure if one
	// ever is not.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("market: session log line %d: %w", line, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("market: reading session log: %w", err)
	}
	return out, nil
}

// ReadRecordsFile parses a session log from disk.
func ReadRecordsFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("market: opening session log: %w", err)
	}
	defer f.Close()
	return ReadRecords(f)
}

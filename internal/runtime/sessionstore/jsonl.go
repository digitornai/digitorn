package sessionstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

type JSONLReadMode int

const (
	JSONLStrict JSONLReadMode = iota
	JSONLBestEffort
)

type JSONLReadResult struct {
	Events      []Event
	BytesRead   int64
	LinesRead   int
	BadLines    int
	BadOffsets  []int64
	Partial     bool
	LastGoodSeq uint64
	LastGoodOff int64
}

func ReadJSONL(path string, mode JSONLReadMode, quarantinePath string) (*JSONLReadResult, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &JSONLReadResult{}, nil
		}
		return nil, fmt.Errorf("jsonl open: %w", err)
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<20)
	res := &JSONLReadResult{}
	var quarantine *os.File
	defer func() {
		if quarantine != nil {
			_ = quarantine.Close()
		}
	}()

	var offset int64
	for {
		line, err := br.ReadBytes('\n')
		startOff := offset
		offset += int64(len(line))
		trimmed := dropNewline(line)

		if len(trimmed) == 0 {
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return res, fmt.Errorf("jsonl read at %d: %w", startOff, err)
			}
			continue
		}

		res.LinesRead++
		var ev Event
		if decErr := json.Unmarshal(trimmed, &ev); decErr != nil {
			res.BadLines++
			res.BadOffsets = append(res.BadOffsets, startOff)
			res.Partial = true
			if mode == JSONLStrict {
				return res, fmt.Errorf("jsonl line %d (offset %d) malformed: %w", res.LinesRead, startOff, decErr)
			}
			if quarantinePath != "" {
				if quarantine == nil {
					quarantine, err = os.OpenFile(quarantinePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
					if err != nil {
						return res, fmt.Errorf("jsonl quarantine open: %w", err)
					}
				}
				if _, qerr := quarantine.Write(line); qerr != nil {
					return res, fmt.Errorf("jsonl quarantine write: %w", qerr)
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return res, fmt.Errorf("jsonl read at %d: %w", startOff, err)
			}
			continue
		}

		res.Events = append(res.Events, ev)
		if ev.Seq > res.LastGoodSeq {
			res.LastGoodSeq = ev.Seq
		}
		res.LastGoodOff = offset

		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return res, fmt.Errorf("jsonl read at %d: %w", startOff, err)
		}
	}

	res.BytesRead = offset
	return res, nil
}

func dropNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

type JSONLWriter struct {
	f      *os.File
	bw     *bufio.Writer
	fsync  bool
	closed bool
}

func OpenJSONLAppend(path string, fsync bool) (*JSONLWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("jsonl append open: %w", err)
	}
	return &JSONLWriter{f: f, bw: bufio.NewWriterSize(f, 64<<10), fsync: fsync}, nil
}

func (w *JSONLWriter) Write(events []Event) (int, error) {
	if w.closed {
		return 0, errors.New("jsonl writer closed")
	}
	var written int
	for i := range events {
		data, err := json.Marshal(events[i])
		if err != nil {
			return written, fmt.Errorf("jsonl encode seq=%d: %w", events[i].Seq, err)
		}
		if _, err := w.bw.Write(data); err != nil {
			return written, fmt.Errorf("jsonl write: %w", err)
		}
		if err := w.bw.WriteByte('\n'); err != nil {
			return written, fmt.Errorf("jsonl write nl: %w", err)
		}
		written++
	}
	return written, nil
}

func (w *JSONLWriter) Flush() error {
	if w.closed {
		return errors.New("jsonl writer closed")
	}
	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("jsonl flush: %w", err)
	}
	if w.fsync {
		if err := fdatasyncFile(w.f); err != nil {
			return fmt.Errorf("jsonl sync: %w", err)
		}
	}
	return nil
}

func (w *JSONLWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close()
		return fmt.Errorf("jsonl close flush: %w", err)
	}
	if w.fsync {
		_ = fdatasyncFile(w.f)
	}
	return w.f.Close()
}

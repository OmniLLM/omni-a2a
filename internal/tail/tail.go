// Package tail provides simple file tailing: read the last N lines and
// optionally follow appended content.
package tail

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

const pollInterval = 250 * time.Millisecond

// LastLines reads the last n lines from the file at path.
// If the file has fewer than n lines, all lines are returned.
func LastLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Ring buffer to keep only the last n lines.
	ring := make([]string, 0, n)
	for scanner.Scan() {
		if len(ring) < n {
			ring = append(ring, scanner.Text())
		} else {
			copy(ring, ring[1:])
			ring[n-1] = scanner.Text()
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return ring, nil
}

// Follow watches the file at path and copies newly appended content to w.
// It polls every 250ms and handles file truncation (e.g. log rotation) by
// reseeking to the beginning. It blocks until ctx is cancelled.
func Follow(ctx context.Context, path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Seek to end — we only want new content.
	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}

	reader := bufio.NewReader(f)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Check for truncation (log rotation).
			info, err := f.Stat()
			if err != nil {
				return fmt.Errorf("stat %s: %w", path, err)
			}
			if info.Size() < offset {
				// File was truncated — reopen from the start.
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					return fmt.Errorf("seek after truncation: %w", err)
				}
				offset = 0
				reader.Reset(f)
			}

			// Read any new content.
			for {
				line, err := reader.ReadString('\n')
				if len(line) > 0 {
					fmt.Fprint(w, line)
					offset += int64(len(line))
				}
				if err != nil {
					break // no more data right now
				}
			}
		}
	}
}

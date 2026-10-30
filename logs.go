package monolith

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"time"
)

// TailOptions controls how StreamLogFile reads a log file.
type TailOptions struct {
	// Lines is the number of trailing lines to print before streaming.
	// Zero means "print everything from the start of the file".
	// A negative value means "skip the existing backlog entirely".
	Lines int
	// Follow keeps reading new content appended to the file until ctx
	// is cancelled or the writer to out returns an error.
	Follow bool
}

// StreamLogFile prints the contents of the log file at path to out, with
// optional tail/follow semantics analogous to `tail -F`. When Follow is true,
// it keeps reading the file as the daemon appends to it. The function exits
// when out returns an error (e.g. the user closes stdout) or, if Follow is
// false, after the existing content has been written.
func StreamLogFile(path string, out io.Writer, opts TailOptions) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Seek to the start position based on Lines.
	switch {
	case opts.Lines < 0:
		// Skip existing content entirely; jump to end.
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
	case opts.Lines == 0:
		// Print the whole file from the beginning. No seek needed.
	default:
		if err := seekToLastNLines(f, opts.Lines); err != nil {
			return err
		}
	}

	reader := bufio.NewReader(f)
	if err := copyAvailable(reader, out); err != nil {
		return err
	}

	if !opts.Follow {
		return nil
	}

	// Follow: poll for new content. Re-stat to detect truncation.
	const pollInterval = 200 * time.Millisecond
	for {
		time.Sleep(pollInterval)

		// Detect truncation: if file shrank, re-open from the start.
		fi, err := os.Stat(path)
		if err == nil {
			pos, _ := f.Seek(0, io.SeekCurrent)
			if fi.Size() < pos {
				f.Close()
				nf, err := os.Open(path)
				if err != nil {
					return err
				}
				f = nf
				reader = bufio.NewReader(f)
			}
		}

		if err := copyAvailable(reader, out); err != nil {
			return err
		}
	}
}

// copyAvailable drains everything currently buffered/readable into out.
// Returns nil at EOF (so the caller can poll for more in follow mode).
func copyAvailable(r *bufio.Reader, out io.Writer) error {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// seekToLastNLines positions f at the start of the line that is n lines
// before EOF (or at the start of the file if it has fewer than n lines).
func seekToLastNLines(f *os.File, n int) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	if size == 0 {
		return nil
	}

	const chunk int64 = 8192
	var (
		offset   = size
		newlines = 0
		buf      = make([]byte, chunk)
		// We need n+1 newlines so that we land at the start of the
		// nth-from-last line (a trailing newline at EOF counts as one).
		needed = n + 1
	)

	for offset > 0 {
		readSize := chunk
		if offset < chunk {
			readSize = offset
		}
		offset -= readSize
		if _, err := f.ReadAt(buf[:readSize], offset); err != nil && err != io.EOF {
			return err
		}
		for i := readSize - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				newlines++
				if newlines == needed {
					_, err := f.Seek(offset+i+1, io.SeekStart)
					return err
				}
			}
		}
	}

	// Fewer than n lines in the file; start at the beginning.
	_, err = f.Seek(0, io.SeekStart)
	return err
}

// EnsureLogReadable returns a friendly error if the log file is missing.
func EnsureLogReadable(path, what, name string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no logs for %s %q (file %s does not exist)", what, name, path)
		}
		return err
	}
	return nil
}

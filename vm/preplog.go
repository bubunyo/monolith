package vm

import (
	"fmt"
	"io"
)

// progressReader logs how much of a download has been read to log: at
// every tenth of total, or every 10 MB when the size isn't known.
type progressReader struct {
	r      io.Reader
	log    io.Writer
	what   string
	total  int64
	n      int64
	logged int64 // n at the last line
}

func newProgressReader(r io.Reader, log io.Writer, what string, total int64) *progressReader {
	fmt.Fprintf(log, "Downloading %s (%s)\n", what, sizeMB(total))
	return &progressReader{r: r, log: log, what: what, total: total}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	step := int64(10 << 20)
	if p.total > 0 {
		step = p.total / 10
	}
	if p.n-p.logged >= step && (p.total <= 0 || p.n < p.total) {
		p.logged = p.n
		if p.total > 0 {
			fmt.Fprintf(p.log, "  %s: %s of %s (%d%%)\n", p.what, sizeMB(p.n), sizeMB(p.total), p.n*100/p.total)
		} else {
			fmt.Fprintf(p.log, "  %s: %s\n", p.what, sizeMB(p.n))
		}
	}
	if err == io.EOF {
		fmt.Fprintf(p.log, "Downloaded %s (%s)\n", p.what, sizeMB(p.n))
	}
	return n, err
}

// sizeMB formats a byte count for the prepare log, "size unknown" when
// it's not positive.
func sizeMB(n int64) string {
	if n <= 0 {
		return "size unknown"
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}

//go:build linux

package metrics

// A GPU sample used to fork and exec nvidia-smi every time (issue #53):
// about 24.5 ms of CPU and one execve per sample. The sampler now owns one
// long-lived nvidia-smi child that reports the same rows on a loop, so a
// sample costs a read of the child's output instead of a process.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// nvidiaQuery is the row nvidia-smi prints for every GPU.
	nvidiaQuery = "--query-gpu=pci.bus_id,utilization.gpu,temperature.gpu,memory.used,memory.total,name"
	// nvidiaFormat asks for the rows without a header and without units,
	// the shape the one-shot run read.
	nvidiaFormat = "--format=csv,noheader,nounits"

	// nvidiaDefaultPeriod is the loop period the child starts with before
	// the consumer's cadence is known.
	nvidiaDefaultPeriod = time.Second
	// nvidiaMinPeriod keeps the child from polling faster than the
	// consumer asks for; nvidiaMaxPeriod keeps the child's rows from
	// going stale between slow samples.
	nvidiaMinPeriod = 500 * time.Millisecond
	nvidiaMaxPeriod = 5 * time.Second
	// nvidiaStaleFactor is how many periods without a new row make the
	// child's output stale.
	nvidiaStaleFactor = 2
	// nvidiaMaxBackoff caps the delay before a failed child is started
	// again.
	nvidiaMaxBackoff = 30 * time.Second
	// nvidiaMaxFailures caps the backoff growth, so the shift that
	// computes it cannot overflow.
	nvidiaMaxFailures = 6
)

// nvidiaProcess is the long-lived nvidia-smi child. read returns the bytes
// the child has written since the last call, and io.EOF once the child is
// gone.
type nvidiaProcess interface {
	read() ([]byte, error)
	stop()
}

// nvidiaChild runs nvidia-smi in a loop and hands out its rows. The child
// writes into a pipe the sampler drains without blocking, so the sampler
// keeps owning no goroutine.
type nvidiaChild struct {
	cmd    *exec.Cmd
	readFD int
	waited bool
	buf    []byte
}

// startNvidiaChild starts nvidia-smi with the loop period and returns the
// child that reads its rows.
func startNvidiaChild(period time.Duration) (nvidiaProcess, error) {
	fds := make([]int, 2)
	if err := syscall.Pipe2(fds, syscall.O_CLOEXEC); err != nil {
		return nil, fmt.Errorf("nvidia-smi: pipe: %w", err)
	}
	stdout := os.NewFile(uintptr(fds[1]), "nvidia-smi")
	if stdout == nil {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
		return nil, errors.New("nvidia-smi: stdout pipe")
	}
	cmd := exec.Command("nvidia-smi", nvidiaQuery, nvidiaFormat, "-lms", strconv.Itoa(int(period.Milliseconds())))
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = syscall.Close(fds[0])
		return nil, err
	}
	// The child holds the write end now. The parent reads the other end
	// without waiting, so a sample never blocks on the child.
	_ = stdout.Close()
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = syscall.Close(fds[0])
		return nil, fmt.Errorf("nvidia-smi: nonblocking pipe: %w", err)
	}
	return &nvidiaChild{cmd: cmd, readFD: fds[0]}, nil
}

// read returns the bytes the child has written so far. It returns io.EOF
// once the child is gone.
func (c *nvidiaChild) read() ([]byte, error) {
	if c.buf == nil {
		c.buf = make([]byte, 8192)
	}
	for {
		n, err := syscall.Read(c.readFD, c.buf)
		if n > 0 {
			return c.buf[:n], nil
		}
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, nil
			}
			return nil, err
		}
		// The write end is closed: the child is gone.
		c.reap()
		return nil, io.EOF
	}
}

// reap waits for the child so it does not stay behind as a zombie.
func (c *nvidiaChild) reap() {
	if c.waited {
		return
	}
	c.waited = true
	_ = c.cmd.Wait()
}

// stop ends the child and waits for it.
func (c *nvidiaChild) stop() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	c.reap()
	_ = syscall.Close(c.readFD)
}

// nvidiaStream turns the output of a long-lived nvidia-smi child into the
// newest complete tick: the rows of one report, one row per GPU. A tick is
// never mixed with the next one, so the GPUs of a snapshot come from the
// same report.
type nvidiaStream struct {
	proc   nvidiaProcess
	period time.Duration
	now    func() time.Time

	// buf holds the bytes of a row that has not ended yet.
	buf []byte
	// pending holds the rows of the tick being filled, tick the newest
	// complete one.
	pending [][]byte
	tick    [][]byte
	// key is the bus id that starts a tick; tickLen is how many rows a
	// tick holds, learned from the repetition of that key.
	key     string
	tickLen int

	started time.Time
	lastRow time.Time
	err     error
}

// newNvidiaStream wraps a child that reports rows on a loop.
func newNvidiaStream(proc nvidiaProcess, period time.Duration, now func() time.Time) *nvidiaStream {
	return &nvidiaStream{proc: proc, period: period, now: now, started: now()}
}

// rows drains the child and returns the newest complete tick as CSV rows.
// It reports false while the child has not produced a whole tick yet.
func (s *nvidiaStream) rows() ([]byte, bool) {
	s.drain()
	if len(s.tick) == 0 {
		return nil, false
	}
	return joinNvidiaRows(s.tick), true
}

// drain reads what the child has written and frames the rows it carries.
func (s *nvidiaStream) drain() {
	for {
		data, err := s.proc.read()
		if len(data) > 0 {
			s.feed(data)
		}
		if err != nil {
			s.err = err
			return
		}
		if len(data) == 0 {
			return
		}
	}
}

// feed frames the rows in the child's output.
func (s *nvidiaStream) feed(data []byte) {
	s.buf = append(s.buf, data...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			return
		}
		line := bytes.TrimSpace(s.buf[:i])
		s.buf = s.buf[i+1:]
		if len(line) == 0 {
			continue
		}
		s.row(bytes.Clone(line))
	}
}

// row adds one row to the report being framed. Every report starts with the
// row of the first bus id, so the repetition of that bus id closes the report
// before it and opens the next one. The size of a report is learned from
// that repetition, so the framing needs no hint about how many GPUs there
// are.
func (s *nvidiaStream) row(row []byte) {
	key := nvidiaRowKey(row)
	s.lastRow = s.now()
	switch {
	case len(s.pending) == 0:
		// The first row of a report.
		if s.tickLen > 0 && key != s.key {
			// The set of GPUs changed: learn the size of a report again.
			s.tickLen = 0
		}
		s.pending = [][]byte{row}
		s.key = key
	case key == s.key:
		// The row starts the next report, so the one being filled is
		// complete.
		s.tick = s.pending
		s.pending = [][]byte{row}
		if s.tickLen == 0 {
			s.tickLen = len(s.tick)
		}
	default:
		// Another row of the report being filled.
		s.pending = append(s.pending, row)
	}
}

// stop ends the child and waits for it.
func (s *nvidiaStream) stop() {
	s.proc.stop()
}

// stale reports that the child is gone or that its output stopped moving.
func (s *nvidiaStream) stale() bool {
	if s.err != nil {
		return true
	}
	last := s.lastRow
	if last.IsZero() {
		last = s.started
	}
	return s.now().Sub(last) > nvidiaStaleFactor*s.period
}

// nvidiaRowKey is the bus id a row starts with. It marks the first row of
// every report.
func nvidiaRowKey(row []byte) string {
	line := string(row)
	if i := strings.IndexByte(line, ','); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// joinNvidiaRows renders the rows of one report as the CSV the sampler
// parses.
func joinNvidiaRows(rows [][]byte) []byte {
	var out []byte
	for _, row := range rows {
		out = append(out, row...)
		out = append(out, '\n')
	}
	return out
}

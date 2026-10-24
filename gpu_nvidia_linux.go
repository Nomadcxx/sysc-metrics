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
	// nvidiaQuery prefixes each row with the number of GPUs in the report,
	// which frames ticks even when the device set changes.
	nvidiaQuery = "--query-gpu=count,pci.bus_id,utilization.gpu,temperature.gpu,memory.used,memory.total,name"
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
// newest complete tick. The count field marks how many rows belong to each
// report and is stripped before the rows reach the existing CSV parser.
type nvidiaStream struct {
	proc   nvidiaProcess
	period time.Duration
	now    func() time.Time

	// buf holds the bytes of a row that has not ended yet.
	buf []byte
	// pending holds the rows of the tick being filled, tick the newest
	// complete one, and pendingCount is the row count for that report.
	pending      [][]byte
	tick         [][]byte
	pendingCount int

	started time.Time
	tickAt  time.Time
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
	if len(s.tick) == 0 || s.stale() {
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

// row adds one row to the report being framed. The count prefix ends a report
// without relying on a GPU remaining first in the device list.
func (s *nvidiaStream) row(row []byte) {
	count, data, ok := nvidiaRowCount(row)
	if !ok {
		s.pending = nil
		s.pendingCount = 0
		return
	}
	if count != s.pendingCount {
		s.pending = nil
		s.pendingCount = count
	}
	s.pending = append(s.pending, data)
	if len(s.pending) == s.pendingCount {
		s.tick = s.pending
		s.pending = nil
		s.pendingCount = 0
		s.tickAt = s.now()
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
	last := s.tickAt
	if last.IsZero() {
		last = s.started
	}
	return s.now().Sub(last) > nvidiaStaleFactor*s.period
}

// nvidiaRowCount removes and parses the row count prefix emitted by
// nvidia-smi, returning the CSV row expected by parseNvidiaCSV.
func nvidiaRowCount(row []byte) (int, []byte, bool) {
	i := bytes.IndexByte(row, ',')
	if i < 0 {
		return 0, nil, false
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(row[:i])))
	if err != nil || count < 1 {
		return 0, nil, false
	}
	return count, bytes.TrimSpace(row[i+1:]), true
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

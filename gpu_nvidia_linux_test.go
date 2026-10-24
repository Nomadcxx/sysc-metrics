//go:build linux

package metrics

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeNvidiaChild hands out scripted output instead of running nvidia-smi.
type fakeNvidiaChild struct {
	chunks  [][]byte
	err     error
	stopped bool
}

func (c *fakeNvidiaChild) read() ([]byte, error) {
	if len(c.chunks) == 0 {
		if c.err != nil {
			return nil, c.err
		}
		return nil, nil
	}
	chunk := c.chunks[0]
	c.chunks = c.chunks[1:]
	return chunk, nil
}

func (c *fakeNvidiaChild) stop() {
	c.stopped = true
}

// fakeClock is a clock the test moves by hand.
type fakeClock struct {
	at time.Time
}

func (c *fakeClock) now() time.Time {
	return c.at
}

const (
	// nvidiaRowA and nvidiaRowB are the rows of two GPUs.
	nvidiaRowA = "0000:08:00.0, 37, 61, 463, 8188, GeForce RTX 4090"
	nvidiaRowB = "0000:09:00.0, 12, 45, 128, 8192, GeForce RTX 4090"
)

func nvidiaStreamRows(count int, rows ...string) []byte {
	var out strings.Builder
	for _, row := range rows {
		out.WriteString(strconv.Itoa(count))
		out.WriteString(", ")
		out.WriteString(row)
		out.WriteByte('\n')
	}
	return []byte(out.String())
}

func TestNvidiaStreamServesTheNewestCompleteReport(t *testing.T) {
	clock := &fakeClock{at: time.Unix(0, 0)}
	child := &fakeNvidiaChild{chunks: [][]byte{
		nvidiaStreamRows(2, nvidiaRowA, nvidiaRowB),
	}}
	stream := newNvidiaStream(child, time.Second, clock.now)

	rows, ok := stream.rows()
	if !ok {
		t.Fatal("no report was served after all GPU rows arrived")
	}
	if strings.Count(string(rows), "\n") != 2 {
		t.Fatalf("report = %q, want two rows", rows)
	}

	child.chunks = append(child.chunks, nvidiaStreamRows(2, nvidiaRowA, nvidiaRowB))
	rows, ok = stream.rows()
	if !ok {
		t.Fatal("no report was served")
	}
	text := string(rows)
	if !strings.Contains(text, nvidiaRowA) || !strings.Contains(text, nvidiaRowB) {
		t.Fatalf("report = %q, want both rows of one report", text)
	}
	if strings.Count(text, "\n") != 2 {
		t.Fatalf("report = %q, want two rows", text)
	}
}

func TestNvidiaStreamNeverMixesTwoReports(t *testing.T) {
	clock := &fakeClock{at: time.Unix(0, 0)}
	child := &fakeNvidiaChild{chunks: [][]byte{
		nvidiaStreamRows(2, nvidiaRowA, nvidiaRowB),
		nvidiaStreamRows(2, "0000:08:00.0, 5, 6, 7, 8, GeForce RTX 4090"),
	}}
	stream := newNvidiaStream(child, time.Second, clock.now)

	// The second report arrives one row at a time. The served report is
	// the complete one, never a mix of both.
	rows, ok := stream.rows()
	if !ok {
		t.Fatal("no report was served")
	}
	text := string(rows)
	if !strings.Contains(text, nvidiaRowA) || !strings.Contains(text, nvidiaRowB) {
		t.Fatalf("report = %q, want the complete report", text)
	}
	if strings.Contains(text, "5, 6, 7, 8") {
		t.Fatalf("report = %q, want no row of the next report", text)
	}
}

func TestNvidiaStreamServesTheNewestReportOfOneGPU(t *testing.T) {
	const (
		rowA = "0000:08:00.0, 37, 61, 463, 8188, GeForce RTX 4090"
		rowB = "0000:08:00.0, 5, 6, 7, 8, GeForce RTX 4090"
		rowC = "0000:08:00.0, 9, 10, 11, 12, GeForce RTX 4090"
	)
	clock := &fakeClock{at: time.Unix(0, 0)}
	child := &fakeNvidiaChild{chunks: [][]byte{
		nvidiaStreamRows(1, rowA, rowB),
	}}
	stream := newNvidiaStream(child, time.Second, clock.now)

	// One GPU means one row per report; the stream keeps the newest row.
	rows, ok := stream.rows()
	if !ok {
		t.Fatal("no report was served")
	}
	if !strings.Contains(string(rows), "5, 6, 7, 8") {
		t.Fatalf("report = %q, want the newest report", rows)
	}

	child.chunks = append(child.chunks, nvidiaStreamRows(1, rowC))
	rows, ok = stream.rows()
	if !ok {
		t.Fatal("no report was served")
	}
	if !strings.Contains(string(rows), "9, 10, 11, 12") {
		t.Fatalf("report = %q, want the newest complete report", rows)
	}
}

func TestNvidiaStreamRelearnsTheReportSizeWhenGPUCountChanges(t *testing.T) {
	clock := &fakeClock{at: time.Unix(0, 0)}
	child := &fakeNvidiaChild{chunks: [][]byte{
		nvidiaStreamRows(2, nvidiaRowA, nvidiaRowB),
		nvidiaStreamRows(1, "0000:0a:00.0, 9, 10, 11, 12, GeForce RTX 4090"),
	}}
	stream := newNvidiaStream(child, time.Second, clock.now)
	rows, ok := stream.rows()
	if !ok {
		t.Fatal("no report was served")
	}
	text := string(rows)
	if !strings.Contains(text, "0000:0a:00.0") || strings.Contains(text, nvidiaRowA) || strings.Contains(text, nvidiaRowB) {
		t.Fatalf("report = %q, want only the single GPU in the new report", text)
	}
}

func TestNvidiaStreamHandlesTheFirstGPUChangingBetweenReports(t *testing.T) {
	const rowC = "0000:0a:00.0, 9, 10, 11, 12, GeForce RTX 4090"
	clock := &fakeClock{at: time.Unix(0, 0)}
	child := &fakeNvidiaChild{chunks: [][]byte{
		[]byte("2, " + nvidiaRowA + "\n2, " + nvidiaRowB + "\n" +
			"2, " + nvidiaRowB + "\n2, " + rowC + "\n"),
	}}
	stream := newNvidiaStream(child, time.Second, clock.now)

	rows, ok := stream.rows()
	if !ok {
		t.Fatal("no report was served")
	}
	text := string(rows)
	if !strings.Contains(text, nvidiaRowB) || !strings.Contains(text, rowC) {
		t.Fatalf("report = %q, want the rows after the first GPU changed", text)
	}
	if strings.Contains(text, nvidiaRowA) {
		t.Fatalf("report = %q, want no row from the previous GPU set", text)
	}
}

func TestNvidiaStreamKeepsARowThatArrivesInParts(t *testing.T) {
	clock := &fakeClock{at: time.Unix(0, 0)}
	child := &fakeNvidiaChild{chunks: [][]byte{
		append(nvidiaStreamRows(2, nvidiaRowA), []byte("2, 0000:08:00")...),
		[]byte(".0, 12, 45, 128, 8192, GeForce RTX 4090\n"),
	}}
	stream := newNvidiaStream(child, time.Second, clock.now)

	rows, ok := stream.rows()
	if !ok {
		t.Fatal("no report was served")
	}
	if !strings.Contains(string(rows), "37, 61") {
		t.Fatalf("report = %q, want the first row", rows)
	}
}

func TestNvidiaStreamReportsAGoneChild(t *testing.T) {
	clock := &fakeClock{at: time.Unix(0, 0)}
	child := &fakeNvidiaChild{err: io.EOF}
	stream := newNvidiaStream(child, time.Second, clock.now)

	if _, ok := stream.rows(); ok {
		t.Fatal("a report was served from a child that is gone")
	}
	if !stream.stale() {
		t.Fatal("the child that is gone was not reported as stale")
	}
	if !errors.Is(stream.err, io.EOF) {
		t.Fatalf("err = %v, want EOF", stream.err)
	}
}

func TestNvidiaStreamReportsASilentChild(t *testing.T) {
	clock := &fakeClock{at: time.Unix(0, 0)}
	child := &fakeNvidiaChild{}
	stream := newNvidiaStream(child, time.Second, clock.now)

	clock.at = clock.at.Add(time.Second)
	if stream.stale() {
		t.Fatal("the child was reported as stale before it could report")
	}
	clock.at = clock.at.Add(2 * time.Second)
	if !stream.stale() {
		t.Fatal("the silent child was not reported as stale")
	}
}

func TestNvidiaStreamExpiresTheLastCompleteReport(t *testing.T) {
	clock := &fakeClock{at: time.Unix(0, 0)}
	child := &fakeNvidiaChild{chunks: [][]byte{
		[]byte("1, " + nvidiaRowA + "\n1, " + nvidiaRowB + "\n"),
	}}
	stream := newNvidiaStream(child, time.Second, clock.now)
	if _, ok := stream.rows(); !ok {
		t.Fatal("no report was served")
	}

	clock.at = clock.at.Add(3 * time.Second)
	if rows, ok := stream.rows(); ok {
		t.Fatalf("stale report %q was served", rows)
	}
	if !stream.stale() {
		t.Fatal("the last complete report was not reported as stale")
	}
}

func TestGPUSamplerReadsTheNvidiaRowsFromTheChild(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card0", "0000:08:00.0", "0x10de", "0x2684")
	child := &fakeNvidiaChild{chunks: [][]byte{
		nvidiaStreamRows(1, nvidiaRowA, nvidiaRowA),
	}}
	sampler := newGPUSampler(root, t.TempDir(), nil, func() ([]byte, error) {
		t.Fatal("the one-shot nvidia-smi ran while the child had rows")
		return nil, nil
	})
	sampler.start = func(period time.Duration) (nvidiaProcess, error) {
		return child, nil
	}
	snap, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GPUs) != 1 {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
	g := snap.GPUs[0]
	if !g.Usage.Valid || g.Usage.Fraction != 0.37 {
		t.Fatalf("usage = %#v", g.Usage)
	}
	if !g.TempValid || g.Celsius != 61 {
		t.Fatalf("temp = %#v", g)
	}
	if len(snap.Issues) != 0 {
		t.Fatalf("issues = %#v", snap.Issues)
	}
}

func TestGPUSamplerFallsBackWhenTheChildIsGone(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card0", "0000:08:00.0", "0x10de", "0x2684")
	child := &fakeNvidiaChild{err: io.EOF}
	called := false
	sampler := newGPUSampler(root, t.TempDir(), nil, func() ([]byte, error) {
		called = true
		return []byte(nvidiaRowA + "\n"), nil
	})
	sampler.start = func(period time.Duration) (nvidiaProcess, error) {
		return child, nil
	}
	snap, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("the one-shot nvidia-smi did not cover the sample")
	}
	if len(snap.GPUs) != 1 || !snap.GPUs[0].Usage.Valid {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
	if len(snap.Issues) != 1 || snap.Issues[0].Source != "nvidia-smi" {
		t.Fatalf("issues = %#v", snap.Issues)
	}
	if !child.stopped {
		t.Fatal("the child that is gone was not stopped")
	}
}

func TestGPUSamplerFallsBackWhenTheChildExitsAfterAReport(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card0", "0000:08:00.0", "0x10de", "0x2684")
	child := &fakeNvidiaChild{
		chunks: [][]byte{[]byte("1, " + nvidiaRowA + "\n1, " + nvidiaRowB + "\n")},
		err:    io.EOF,
	}
	called := false
	sampler := newGPUSampler(root, t.TempDir(), nil, func() ([]byte, error) {
		called = true
		return []byte(nvidiaRowA + "\n"), nil
	})
	sampler.start = func(period time.Duration) (nvidiaProcess, error) {
		return child, nil
	}

	snap, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("the one-shot nvidia-smi did not cover the stale report")
	}
	if len(snap.Issues) != 1 || snap.Issues[0].Source != "nvidia-smi" {
		t.Fatalf("issues = %#v", snap.Issues)
	}
	if !child.stopped {
		t.Fatal("the exited child was not stopped")
	}
}

func TestGPUSamplerStopsStaleChildWhenFallbackFails(t *testing.T) {
	clock := &fakeClock{at: time.Unix(1, 0)}
	child := &fakeNvidiaChild{chunks: [][]byte{nvidiaStreamRows(1, nvidiaRowA)}}
	fallbackErr := errors.New("nvidia-smi fallback failed")
	fallbackCalls := 0
	sampler := newGPUSampler("/nonexistent", "/nonexistent", nil, func() ([]byte, error) {
		fallbackCalls++
		return nil, fallbackErr
	})
	sampler.now = clock.now
	sampler.start = func(time.Duration) (nvidiaProcess, error) {
		return child, nil
	}
	if rows, err := sampler.nvidiaRows(); err != nil || !strings.Contains(string(rows), nvidiaRowA) {
		t.Fatalf("initial child rows = %q, %v", rows, err)
	}

	clock.at = clock.at.Add(3 * time.Second)
	if _, err := sampler.nvidiaRows(); !errors.Is(err, fallbackErr) {
		t.Fatalf("stale fallback error = %v, want %v", err, fallbackErr)
	}
	if fallbackCalls != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallbackCalls)
	}
	if !child.stopped || sampler.stream != nil {
		t.Fatal("stale child remained open after fallback failed")
	}
	if sampler.nvidiaRetryAt.IsZero() {
		t.Fatal("failed fallback did not schedule a child restart backoff")
	}
}

func TestGPUSamplerCloseStopsTheChildAndLaterSamplesStartANewOne(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card0", "0000:08:00.0", "0x10de", "0x2684")
	child := &fakeNvidiaChild{chunks: [][]byte{
		nvidiaStreamRows(1, nvidiaRowA, nvidiaRowA),
	}}
	started := 0
	sampler := newGPUSampler(root, t.TempDir(), nil, func() ([]byte, error) {
		return []byte(nvidiaRowA + "\n"), nil
	})
	sampler.start = func(period time.Duration) (nvidiaProcess, error) {
		started++
		return child, nil
	}
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Fatalf("children started = %d, want 1", started)
	}
	if err := sampler.Close(); err != nil {
		t.Fatal(err)
	}
	if !child.stopped {
		t.Fatal("Close did not stop the child")
	}
	if err := sampler.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	if started != 2 {
		t.Fatalf("children started = %d, want a new child after Close", started)
	}
}

func TestGPUSamplerWaitsBeforeRestartingAFailedChild(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card0", "0000:08:00.0", "0x10de", "0x2684")
	attempts := 0
	sampler := newGPUSampler(root, t.TempDir(), nil, func() ([]byte, error) {
		return []byte(nvidiaRowA + "\n"), nil
	})
	sampler.start = func(period time.Duration) (nvidiaProcess, error) {
		attempts++
		return nil, errors.New("nvidia-smi: executable file not found")
	}
	snap, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Issues) != 1 || snap.Issues[0].Source != "nvidia-smi" {
		t.Fatalf("issues = %#v", snap.Issues)
	}
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("children started = %d, want the backoff to hold the retry", attempts)
	}
}

func TestGPUSamplerFollowsTheCadenceOfItsConsumer(t *testing.T) {
	sampler := newGPUSampler("/nonexistent", "/nonexistent", nil, nil)
	clock := &fakeClock{at: time.Unix(0, 0)}
	sampler.now = clock.now
	if got := sampler.nvidiaPeriod(); got != nvidiaDefaultPeriod {
		t.Fatalf("period = %s, want %s", got, nvidiaDefaultPeriod)
	}
	sampler.lastSample = clock.at
	clock.at = clock.at.Add(200 * time.Millisecond)
	if got := sampler.nvidiaPeriod(); got != nvidiaMinPeriod {
		t.Fatalf("period = %s, want %s", got, nvidiaMinPeriod)
	}
	clock.at = clock.at.Add(30 * time.Second)
	if got := sampler.nvidiaPeriod(); got != nvidiaMaxPeriod {
		t.Fatalf("period = %s, want %s", got, nvidiaMaxPeriod)
	}
}

func BenchmarkGPUSamplerNVIDIA(b *testing.B) {
	if _, err := runNvidiaSMI(); err != nil {
		b.Skipf("nvidia-smi is not available: %v", err)
	}
	sampler := NewGPUSampler()
	defer sampler.Close()
	// The child reports on a loop: the measurement starts once its rows
	// are framed into a report, so it covers the steady state and not the
	// samples the child has not reported yet.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := sampler.Sample(); err != nil {
			b.Fatal(err)
		}
		if sampler.stream != nil && len(sampler.stream.tick) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sampler.Sample(); err != nil {
			b.Fatal(err)
		}
	}
}

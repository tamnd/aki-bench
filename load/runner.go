package load

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Mode selects the load generator's timing discipline.
type Mode int

const (
	// ClosedLoop issues the next command as soon as the previous reply lands.
	// Throughput is whatever the server can sustain. This measures service time.
	ClosedLoop Mode = iota
	// OpenLoop issues commands on a fixed schedule independent of server speed,
	// at the configured target rate. When the server stalls, the latency a
	// queued request would have seen is reconstructed, which is the
	// coordinated-omission correction. This measures response time under load.
	OpenLoop
)

// CommandGen produces the next command's argument vector for a given connection.
// argv[0] is the command name. The seq argument is a per-connection monotonically
// increasing counter so a generator can vary keys across a key space.
type CommandGen func(conn int, seq int64) [][]byte

// Config describes one load run.
type Config struct {
	Addr        string        // host:port of the target server
	Connections int           // number of concurrent TCP connections
	Pipeline    int           // commands in flight per connection before reading replies
	Duration    time.Duration // wall-clock run length
	Requests    int64         // total requests to issue, used when Duration is zero
	Mode        Mode          // closed-loop or open-loop
	TargetRate  int64         // open-loop aggregate target ops/sec, required in OpenLoop
	DialTimeout time.Duration // per-connection dial timeout
	Gen         CommandGen    // command source
}

func (c Config) withDefaults() Config {
	if c.Connections <= 0 {
		c.Connections = 1
	}
	if c.Pipeline <= 0 {
		c.Pipeline = 1
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 5 * time.Second
	}
	return c
}

// Result is the outcome of a load run.
type Result struct {
	Ops          int64         // successful operations
	Errors       int64         // operations that returned a transport or protocol error
	Elapsed      time.Duration // wall-clock duration of the run
	Hist         *Histogram    // per-operation latency in nanoseconds
	Connected    int           // connections that came up
	BytesRead    int64         // wire bytes read from the sockets (replies)
	BytesWritten int64         // wire bytes written to the sockets (commands)
}

// OpsPerSec returns the achieved throughput.
func (r Result) OpsPerSec() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Ops) / r.Elapsed.Seconds()
}

// BytesPerSec returns the total wire bandwidth the run moved, both directions
// summed. This is the CF20 column: on giant-value rows a cell where every
// server sits at the box's copy ceiling is bandwidth-bound and its ops/s parity
// is manufactured, so bytes/s travels with every row to make that visible.
func (r Result) BytesPerSec() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.BytesRead+r.BytesWritten) / r.Elapsed.Seconds()
}

// Run executes the load described by cfg and returns the aggregated result.
// It dials all connections up front, skips any that fail to connect, and runs the
// survivors. If no connection comes up it returns an error.
func Run(ctx context.Context, cfg Config) (Result, error) {
	cfg = cfg.withDefaults()

	clients := make([]*Client, 0, cfg.Connections)
	for i := 0; i < cfg.Connections; i++ {
		cl, err := Dial(cfg.Addr, cfg.DialTimeout)
		if err != nil {
			continue
		}
		clients = append(clients, cl)
	}
	if len(clients) == 0 {
		return Result{}, &DialError{Addr: cfg.Addr}
	}
	defer func() {
		for _, cl := range clients {
			_ = cl.Close()
		}
	}()

	runCtx := ctx
	var cancel context.CancelFunc
	if cfg.Duration > 0 {
		runCtx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	var ops, errs atomic.Int64
	perConnReq := int64(0)
	if cfg.Requests > 0 {
		perConnReq = cfg.Requests / int64(len(clients))
		if perConnReq < 1 {
			perConnReq = 1
		}
	}

	hists := make([]*Histogram, len(clients))
	start := time.Now()

	var wg sync.WaitGroup
	for i, cl := range clients {
		wg.Add(1)
		h := NewLatencyHistogram()
		hists[i] = h
		go func(conn int, cl *Client, h *Histogram) {
			defer wg.Done()
			if cfg.Mode == OpenLoop {
				driveOpen(runCtx, conn, cl, h, cfg, len(clients), perConnReq, &ops, &errs)
			} else {
				driveClosed(runCtx, conn, cl, h, cfg, perConnReq, &ops, &errs)
			}
		}(i, cl, h)
	}
	wg.Wait()
	elapsed := time.Since(start)

	merged := NewLatencyHistogram()
	for _, h := range hists {
		merged.Merge(h)
	}

	var bytesRead, bytesWritten int64
	for _, cl := range clients {
		r, w := cl.BytesOnWire()
		bytesRead += r
		bytesWritten += w
	}

	return Result{
		Ops:          ops.Load(),
		Errors:       errs.Load(),
		Elapsed:      elapsed,
		Hist:         merged,
		Connected:    len(clients),
		BytesRead:    bytesRead,
		BytesWritten: bytesWritten,
	}, nil
}

// driveClosed runs one connection in closed-loop mode: send a pipeline batch,
// read its replies, repeat. Latency is timed from first send of a batch to the
// reply of each command in it, which keeps the pipeline's per-op timing honest.
func driveClosed(ctx context.Context, conn int, cl *Client, h *Histogram, cfg Config, perConnReq int64, ops, errs *atomic.Int64) {
	var seq int64
	var issued int64
	for {
		if ctx.Err() != nil {
			return
		}
		if perConnReq > 0 && issued >= perConnReq {
			return
		}

		batch := cfg.Pipeline
		sendTimes := make([]time.Time, batch)
		for i := 0; i < batch; i++ {
			argv := cfg.Gen(conn, seq)
			seq++
			if err := cl.WriteCommand(argv); err != nil {
				errs.Add(1)
				return
			}
			sendTimes[i] = time.Now()
		}
		if err := cl.Flush(); err != nil {
			errs.Add(1)
			return
		}
		for i := 0; i < batch; i++ {
			err := cl.ReadReply()
			now := time.Now()
			if err != nil {
				errs.Add(1)
				return
			}
			h.RecordValue(now.Sub(sendTimes[i]).Nanoseconds())
			ops.Add(1)
			issued++
		}
	}
}

// driveOpen runs one connection in open-loop mode. Each connection owns a slice
// of the aggregate target rate and issues commands on its own fixed schedule.
// Latency is measured against the intended send time, not the actual one, so a
// late issue still counts its queueing delay. The histogram records the corrected
// value so a stall backfills the requests it swallowed.
func driveOpen(ctx context.Context, conn int, cl *Client, h *Histogram, cfg Config, nConns int, perConnReq int64, ops, errs *atomic.Int64) {
	rate := cfg.TargetRate
	if rate <= 0 {
		rate = int64(nConns) // degenerate but keeps the loop alive
	}
	perConnRate := float64(rate) / float64(nConns)
	if perConnRate <= 0 {
		perConnRate = 1
	}
	interval := time.Duration(float64(time.Second) / perConnRate)
	expected := interval.Nanoseconds()

	var seq int64
	var issued int64
	begin := time.Now()
	for {
		if ctx.Err() != nil {
			return
		}
		if perConnReq > 0 && issued >= perConnReq {
			return
		}

		intended := begin.Add(time.Duration(issued) * interval)
		if d := time.Until(intended); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}

		argv := cfg.Gen(conn, seq)
		seq++
		if err := cl.WriteCommand(argv); err != nil {
			errs.Add(1)
			return
		}
		if err := cl.Flush(); err != nil {
			errs.Add(1)
			return
		}
		if err := cl.ReadReply(); err != nil {
			errs.Add(1)
			return
		}
		latency := time.Since(intended).Nanoseconds()
		h.RecordCorrectedValue(latency, expected)
		ops.Add(1)
		issued++
	}
}

// DialError reports that no connection to the target could be established.
type DialError struct{ Addr string }

func (e *DialError) Error() string { return "load: could not connect to " + e.Addr }

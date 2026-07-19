// Package tailgroup supervises a dynamic set of tail.Tailers: Reconcile
// starts tailers for newly desired paths and stops ones no longer wanted.
// It knows nothing about Docker (or any other source of desired paths);
// it is the generic building block for tailing a set of files that
// changes at runtime.
package tailgroup

import (
	"context"
	"log/slog"
	"sync"

	"github.com/shellsecrets/peekastokk/internal/tail"
)

// Group runs one tail.Tailer per desired path, all feeding one channel.
type Group struct {
	ctx  context.Context
	out  chan<- tail.Line
	opts tail.Options
	log  *slog.Logger

	mu      sync.Mutex
	running map[string]context.CancelFunc
	wg      sync.WaitGroup
}

// NewGroup builds a Group whose tailers all send to out and are options
// clones of opts. ctx is the parent of every tailer's context.
func NewGroup(ctx context.Context, out chan<- tail.Line, opts tail.Options) *Group {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Group{
		ctx:     ctx,
		out:     out,
		opts:    opts,
		log:     opts.Logger,
		running: make(map[string]context.CancelFunc),
	}
}

// Reconcile makes the running tailer set match desired: new paths get a
// tailer, absent paths get theirs cancelled. It never blocks on a tailer
// actually exiting — the tailer's own poll loop notices cancellation.
func (g *Group) Reconcile(desired []string) {
	want := make(map[string]bool, len(desired))
	for _, p := range desired {
		want[p] = true
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	for path, cancel := range g.running {
		if !want[path] {
			g.log.Info("stopping tailer", "file", path)
			cancel()
			delete(g.running, path)
		}
	}
	for path := range want {
		if _, ok := g.running[path]; ok {
			continue
		}
		g.log.Info("starting tailer", "file", path)
		childCtx, cancel := context.WithCancel(g.ctx)
		g.running[path] = cancel
		t := tail.New(path, g.opts)
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()
			t.Run(childCtx, g.out)
		}()
	}
}

// Stop cancels every tailer and waits for all of them to exit.
func (g *Group) Stop() {
	g.mu.Lock()
	for path, cancel := range g.running {
		cancel()
		delete(g.running, path)
	}
	g.mu.Unlock()
	g.wg.Wait()
}

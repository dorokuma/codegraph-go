package extraction

import "context"

// indexContext exposes the existing lifetime signal to post-extraction work,
// including SQL queries. Unlike a bridging goroutine, this adapter needs no
// separate lifetime/cleanup when an orchestrator has no shutdown signal.
func (o *Orchestrator) indexContext() context.Context {
	return shutdownContext{Context: context.Background(), done: o.done}
}

type shutdownContext struct {
	context.Context
	done <-chan struct{}
}

func (c shutdownContext) Done() <-chan struct{} { return c.done }

func (c shutdownContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

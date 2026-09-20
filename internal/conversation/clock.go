package conversation

import "time"

// Clock is where the engine reads the time and waits, so a test drives both
// instead of sleeping.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

type Timer interface {
	Chan() <-chan time.Time
	Reset(d time.Duration)
	Stop()
}

// Wall is the clock of the machine.
type Wall struct{}

func (Wall) Now() time.Time { return time.Now() }

func (Wall) NewTimer(d time.Duration) Timer {
	t := time.NewTimer(d)
	t.Stop()
	return &wallTimer{t: t}
}

type wallTimer struct{ t *time.Timer }

func (w *wallTimer) Chan() <-chan time.Time { return w.t.C }
func (w *wallTimer) Reset(d time.Duration)  { w.t.Reset(d) }
func (w *wallTimer) Stop()                  { w.t.Stop() }

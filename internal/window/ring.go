// Package window implements the time-bucketed event counters behind every SLI
// window.
//
// A Ring keeps fixed-resolution buckets of (total, good, bad) event counts for
// a bounded retention. Buckets are right-closed: bucket b covers the interval
// (b*res, (b+1)*res], so a sample stamped exactly on an evaluation boundary T
// is counted by the evaluation at T. Window sums use a two-level layout
// (buckets grouped into fixed-size blocks with running block sums), so a sum
// over n buckets costs O(n/B + B) instead of O(n), while out-of-order writes
// stay O(1).
package window

import (
	"errors"
	"time"
)

// BlockSize is the number of buckets per block. With 1-minute buckets a block
// is an hour, so a 30-day window sum touches ~720 block sums and at most
// 2*64 buckets.
const BlockSize = 64

// ErrTooOld is returned when a write targets a bucket that has already been
// recycled.
var ErrTooOld = errors.New("sample is older than the ring retention")

// Counts are event counts for a bucket or a window.
type Counts struct {
	Total float64
	Good  float64
	Bad   float64
}

// Add returns c + o.
func (c Counts) Add(o Counts) Counts {
	return Counts{Total: c.Total + o.Total, Good: c.Good + o.Good, Bad: c.Bad + o.Bad}
}

// Sub returns c - o.
func (c Counts) Sub(o Counts) Counts {
	return Counts{Total: c.Total - o.Total, Good: c.Good - o.Good, Bad: c.Bad - o.Bad}
}

// Ring is a circular buffer of time buckets. It is not safe for concurrent
// use; the engine serialises access.
type Ring struct {
	res     int64 // bucket width in nanoseconds
	n       int64 // number of buckets, a multiple of BlockSize
	buckets []Counts
	blocks  []Counts
	head    int64 // absolute index of the newest bucket in use
	started bool
}

// New returns a Ring with the given resolution that can answer window sums up
// to retention long.
func New(resolution, retention time.Duration) (*Ring, error) {
	if resolution <= 0 {
		return nil, errors.New("window: resolution must be positive")
	}
	if retention < resolution {
		return nil, errors.New("window: retention must be at least one bucket")
	}
	need := (int64(retention) + int64(resolution) - 1) / int64(resolution)
	n := (need + BlockSize - 1) / BlockSize * BlockSize
	return &Ring{
		res:     int64(resolution),
		n:       n,
		buckets: make([]Counts, n),
		blocks:  make([]Counts, n/BlockSize),
	}, nil
}

// Resolution returns the bucket width.
func (r *Ring) Resolution() time.Duration { return time.Duration(r.res) }

// Capacity returns the longest window the ring can sum.
func (r *Ring) Capacity() time.Duration { return time.Duration(r.n * r.res) }

// BucketIndex returns the absolute index of the bucket containing t.
func (r *Ring) BucketIndex(t time.Time) int64 {
	ns := t.UnixNano()
	// ceil(ns/res) - 1. Go division truncates toward zero, which already is
	// the ceiling for negative values.
	q := ns / r.res
	if ns%r.res > 0 {
		q++
	}
	return q - 1
}

// Add records counts at time t. Writes into the future advance the ring and
// recycle the oldest buckets; writes older than the retention fail with
// ErrTooOld.
func (r *Ring) Add(t time.Time, c Counts) error {
	idx := r.BucketIndex(t)
	if !r.started {
		r.started = true
		r.head = idx
	}
	if idx > r.head {
		r.advance(idx)
	}
	if idx <= r.head-r.n {
		return ErrTooOld
	}
	p := r.pos(idx)
	r.buckets[p] = r.buckets[p].Add(c)
	b := p / BlockSize
	r.blocks[b] = r.blocks[b].Add(c)
	return nil
}

// Advance moves the head to the bucket containing t without writing, so that
// windows ending at t ignore stale data from a previous lap.
func (r *Ring) Advance(t time.Time) {
	idx := r.BucketIndex(t)
	if !r.started {
		r.started = true
		r.head = idx
		return
	}
	if idx > r.head {
		r.advance(idx)
	}
}

func (r *Ring) advance(idx int64) {
	if idx-r.head >= r.n {
		clear(r.buckets)
		clear(r.blocks)
		r.head = idx
		return
	}
	dirty := map[int64]bool{}
	for i := r.head + 1; i <= idx; i++ {
		p := r.pos(i)
		r.buckets[p] = Counts{}
		dirty[p/BlockSize] = true
	}
	// Recompute touched block sums from their buckets so floating-point
	// error never accumulates across laps.
	for b := range dirty {
		var s Counts
		for _, c := range r.buckets[b*BlockSize : (b+1)*BlockSize] {
			s = s.Add(c)
		}
		r.blocks[b] = s
	}
	r.head = idx
}

func (r *Ring) pos(idx int64) int64 {
	p := idx % r.n
	if p < 0 {
		p += r.n
	}
	return p
}

// Sum returns the counts in the window (end-w, end]. end should be aligned to
// the resolution and w a multiple of it; Sum rounds w up to whole buckets.
// Buckets older than the retention or newer than the head count as empty.
//
// The ring always holds exactly the absolute buckets (head-n, head], one per
// position, so every block lying inside [lo, hi] has a consistent block sum.
func (r *Ring) Sum(end time.Time, w time.Duration) Counts {
	if !r.started || w <= 0 {
		return Counts{}
	}
	hi := r.BucketIndex(end)
	nb := (int64(w) + r.res - 1) / r.res
	lo := hi - nb + 1
	if hi > r.head {
		hi = r.head
	}
	if oldest := r.head - r.n + 1; lo < oldest {
		lo = oldest
	}
	if lo > hi {
		return Counts{}
	}
	return r.sumRange(lo, hi)
}

// sumRange sums absolute bucket indexes [lo, hi], which must lie within the
// live part of the ring.
func (r *Ring) sumRange(lo, hi int64) Counts {
	var s Counts
	i := lo
	// Leading partial block.
	for i <= hi && r.pos(i)%BlockSize != 0 {
		s = s.Add(r.buckets[r.pos(i)])
		i++
	}
	// Whole blocks.
	for i+BlockSize-1 <= hi {
		s = s.Add(r.blocks[r.pos(i)/BlockSize])
		i += BlockSize
	}
	// Trailing partial block.
	for ; i <= hi; i++ {
		s = s.Add(r.buckets[r.pos(i)])
	}
	return s
}

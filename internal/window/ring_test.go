package window

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0).UTC().Truncate(time.Hour)

func TestBucketBoundariesAreRightClosed(t *testing.T) {
	r, err := New(time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(t0.Add(time.Minute), Counts{Total: 1}); err != nil { // exactly on a boundary
		t.Fatal(err)
	}
	if err := r.Add(t0.Add(time.Minute+time.Nanosecond), Counts{Total: 10}); err != nil { // next bucket
		t.Fatal(err)
	}
	if got := r.Sum(t0.Add(time.Minute), time.Minute).Total; got != 1 {
		t.Fatalf("window ending on the boundary = %v, want 1", got)
	}
	if got := r.Sum(t0.Add(2*time.Minute), time.Minute).Total; got != 10 {
		t.Fatalf("next window = %v, want 10", got)
	}
	if got := r.Sum(t0.Add(2*time.Minute), 2*time.Minute).Total; got != 11 {
		t.Fatalf("two-bucket window = %v", got)
	}
	if r.BucketIndex(time.Unix(0, 0)) != -1 || r.BucketIndex(time.Unix(0, -1)) != -1 || r.BucketIndex(time.Unix(-60, 0)) != -2 {
		t.Fatal("bucket index around zero")
	}
}

func TestRetentionAndRecycling(t *testing.T) {
	r, err := New(time.Minute, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r.Capacity() < 2*time.Hour || r.Resolution() != time.Minute {
		t.Fatalf("capacity %v", r.Capacity())
	}
	for i := 1; i <= 120; i++ {
		if err := r.Add(t0.Add(time.Duration(i)*time.Minute), Counts{Total: 1, Bad: 1}); err != nil {
			t.Fatal(err)
		}
	}
	end := t0.Add(120 * time.Minute)
	if got := r.Sum(end, 2*time.Hour); got.Total != 120 || got.Bad != 120 {
		t.Fatalf("full window %+v", got)
	}
	// Jump far ahead: everything is recycled.
	far := t0.Add(1000 * time.Hour)
	r.Advance(far)
	if got := r.Sum(far, 2*time.Hour); got.Total != 0 {
		t.Fatalf("after jump %+v", got)
	}
	if err := r.Add(t0.Add(time.Minute), Counts{Total: 1}); !errors.Is(err, ErrTooOld) {
		t.Fatalf("old write err = %v", err)
	}
	// A window longer than the capacity is clamped, not wrapped.
	if got := r.Sum(far, 10000*time.Hour); got.Total != 0 {
		t.Fatalf("oversized window %+v", got)
	}
}

func TestEmptyAndInvalid(t *testing.T) {
	if _, err := New(0, time.Hour); err == nil {
		t.Fatal("zero resolution")
	}
	if _, err := New(time.Hour, time.Minute); err == nil {
		t.Fatal("retention below resolution")
	}
	r, _ := New(time.Minute, time.Hour)
	if got := r.Sum(t0, time.Hour); got != (Counts{}) {
		t.Fatal("empty ring")
	}
	r.Advance(t0)
	r.Advance(t0.Add(-time.Hour)) // moving backwards is a no-op
	if got := r.Sum(t0, 0); got != (Counts{}) {
		t.Fatal("zero window")
	}
	if got := r.Sum(t0.Add(-3*time.Hour), time.Minute); got != (Counts{}) {
		t.Fatal("window entirely before retention")
	}
	c := Counts{Total: 3, Good: 2, Bad: 1}
	if c.Sub(c) != (Counts{}) {
		t.Fatal("Sub")
	}
}

// naive is the obvious reference implementation: a map from bucket index to
// counts with an explicit retention horizon.
type naive struct {
	res     int64
	n       int64
	head    int64
	started bool
	m       map[int64]Counts
}

func (nv *naive) idx(t time.Time) int64 {
	ns := t.UnixNano()
	q := ns / nv.res
	if ns%nv.res > 0 {
		q++
	}
	return q - 1
}

func (nv *naive) add(t time.Time, c Counts) bool {
	i := nv.idx(t)
	if !nv.started {
		nv.started, nv.head = true, i
	}
	if i > nv.head {
		nv.head = i
	}
	if i <= nv.head-nv.n {
		return false
	}
	nv.m[i] = nv.m[i].Add(c)
	return true
}

func (nv *naive) sum(end time.Time, w time.Duration) Counts {
	if !nv.started {
		return Counts{}
	}
	hi := nv.idx(end)
	lo := hi - (int64(w)+nv.res-1)/nv.res + 1
	var s Counts
	for i, c := range nv.m {
		if i >= lo && i <= hi && i > nv.head-nv.n && i <= nv.head {
			s = s.Add(c)
		}
	}
	return s
}

// TestMatchesNaiveReference drives random interleavings of in-order,
// out-of-order, too-old and far-future writes through both implementations
// and compares every window sum.
func TestMatchesNaiveReference(t *testing.T) {
	for seed := uint64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*7919))
		res := time.Duration(1+rng.IntN(60)) * time.Second
		retention := time.Duration(1+rng.IntN(400)) * res
		r, err := New(res, retention)
		if err != nil {
			t.Fatal(err)
		}
		ref := &naive{res: int64(res), n: r.n, m: map[int64]Counts{}}
		now := t0
		for step := 0; step < 3000; step++ {
			switch k := rng.IntN(100); {
			case k < 70: // near the head, possibly slightly late
				now = now.Add(time.Duration(rng.Int64N(int64(3 * res))))
				ts := now.Add(-time.Duration(rng.Int64N(int64(5 * res))))
				c := Counts{Total: float64(rng.IntN(100)), Good: float64(rng.IntN(50)), Bad: float64(rng.IntN(10))}
				errR := r.Add(ts, c)
				okN := ref.add(ts, c)
				if (errR == nil) != okN {
					t.Fatalf("seed %d step %d: ring err=%v naive ok=%v", seed, step, errR, okN)
				}
			case k < 72: // big jump
				now = now.Add(time.Duration(rng.Int64N(int64(2 * retention))))
				r.Advance(now)
				ref.add(now, Counts{})
			case k < 74: // very old write
				ts := now.Add(-2 * retention)
				if (r.Add(ts, Counts{Total: 1}) == nil) != ref.add(ts, Counts{Total: 1}) {
					t.Fatalf("seed %d: old write disagreement", seed)
				}
			default:
				end := now.Truncate(res).Add(time.Duration(rng.IntN(3)-1) * res)
				w := time.Duration(1+rng.IntN(int(r.n)+20)) * res
				got, want := r.Sum(end, w), ref.sum(end, w)
				if got != want {
					t.Fatalf("seed %d step %d: Sum(%v, %v) = %+v, want %+v", seed, step, end, w, got, want)
				}
			}
		}
	}
}

func BenchmarkSum30d(b *testing.B) {
	r, _ := New(time.Minute, 30*24*time.Hour)
	for i := 0; i < 30*24*60; i++ {
		_ = r.Add(t0.Add(time.Duration(i)*time.Minute), Counts{Total: 100, Bad: 1})
	}
	end := t0.Add(30 * 24 * time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Sum(end, 30*24*time.Hour)
	}
}

func BenchmarkAdd(b *testing.B) {
	r, _ := New(time.Minute, 30*24*time.Hour)
	for i := 0; i < b.N; i++ {
		_ = r.Add(t0.Add(time.Duration(i)*time.Second), Counts{Total: 1})
	}
}

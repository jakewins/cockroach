// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package spanlatch

import (
	"context"
	"unsafe"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/spanset"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
)

// A Manager maintains an interval tree of key and key range latches. Latch
// acquitions affecting keys or key ranges must wait on already-acquired latches
// which overlap their key ranges to be released.
//
// Latch acquisition attempts invoke Manager.Acquire and provide details about
// the spans that they plan to touch and the timestamps they plan to touch them
// at. Acquire inserts the latch into the Manager's tree and waits on
// prerequisite latch attempts that are already tracked by the Manager.
// Manager.Acquire blocks until the latch acquisition completes, at which point
// it returns a Guard, which is scoped to the lifetime of the latch ownership.
//
// When the latches are no longer needed, they are released by invoking
// Manager.Release with the Guard returned when the latches were originally
// acquired. Doing so removes the latches from the Manager's tree and signals to
// dependent latch acquisitions that they no longer need to wait on the released
// latches.
//
// Manager is safe for concurrent use by multiple goroutines. Concurrent access
// is made efficient using a copy-on-write technique to capture immutable
// snapshots of the type's inner btree structures. Using this strategy, tasks
// requiring mutual exclusion are limited to updating the type's trees and
// grabbing snapshots. Notably, scanning for and waiting on prerequisite latches
// is performed outside of the mutual exclusion zone. This means that the work
// performed under lock is linear with respect to the number of spans that a
// latch acquisition declares but NOT linear with respect to the number of other
// latch attempts that it will wait on.
//
// Manager's zero value can be used directly.
type Manager struct {
	mu      syncutil.Mutex
	idAlloc uint64
	scopes  [spanset.NumSpanScope]scopedManager
}

// scopedManager is a latch manager scoped to either local or global keys.
// See spanset.SpanScope.
type scopedManager struct {
	readSet latchList
	trees   [spanset.NumSpanAccess]btree
}

// latches are stored in the Manager's btrees. They represent the latching
// of a single key span.
type latch struct {
	id         uint64
	span       roachpb.Span
	ts         hlc.Timestamp
	done       *signal
	next, prev *latch // readSet linked-list.
}

func (la *latch) inReadSet() bool {
	return la.next != nil
}

// Guard is a handle to a set of acquired latches. It is returned by
// Manager.Acquire and accepted by Manager.Release.
type Guard struct {
	done signal
	// latches [spanset.NumSpanScope][spanset.NumSpanAccess][]latch, but half the size.
	latchesPtrs [spanset.NumSpanScope][spanset.NumSpanAccess]unsafe.Pointer
	latchesLens [spanset.NumSpanScope][spanset.NumSpanAccess]int32
}

func (lg *Guard) latches(s spanset.SpanScope, a spanset.SpanAccess) []latch {
	len := lg.latchesLens[s][a]
	if len == 0 {
		return nil
	}
	const maxArrayLen = 1 << 31
	return (*[maxArrayLen]latch)(lg.latchesPtrs[s][a])[:len:len]
}

func (lg *Guard) setLatches(s spanset.SpanScope, a spanset.SpanAccess, latches []latch) {
	lg.latchesPtrs[s][a] = unsafe.Pointer(&latches[0])
	lg.latchesLens[s][a] = int32(len(latches))
}

func allocGuardAndLatches(nLatches int) (*Guard, []latch) {
	// Guard would be an ideal candidate for object pooling, but without
	// reference counting its latches we can't know whether they're still
	// referenced by other tree snapshots. The latches hold a reference to
	// the signal living on the Guard, so the guard can't be recycled while
	// latches still point to it.
	if nLatches <= 1 {
		alloc := new(struct {
			g       Guard
			latches [1]latch
		})
		return &alloc.g, alloc.latches[:nLatches]
	} else if nLatches <= 2 {
		alloc := new(struct {
			g       Guard
			latches [2]latch
		})
		return &alloc.g, alloc.latches[:nLatches]
	} else if nLatches <= 4 {
		alloc := new(struct {
			g       Guard
			latches [4]latch
		})
		return &alloc.g, alloc.latches[:nLatches]
	} else if nLatches <= 8 {
		alloc := new(struct {
			g       Guard
			latches [8]latch
		})
		return &alloc.g, alloc.latches[:nLatches]
	}
	return new(Guard), make([]latch, nLatches)
}

func newGuard(spans *spanset.SpanSet, ts hlc.Timestamp) *Guard {
	nLatches := 0
	for s := spanset.SpanScope(0); s < spanset.NumSpanScope; s++ {
		for a := spanset.SpanAccess(0); a < spanset.NumSpanAccess; a++ {
			nLatches += len(spans.GetSpans(a, s))
		}
	}

	guard, latches := allocGuardAndLatches(nLatches)
	for s := spanset.SpanScope(0); s < spanset.NumSpanScope; s++ {
		for a := spanset.SpanAccess(0); a < spanset.NumSpanAccess; a++ {
			ss := spans.GetSpans(a, s)
			n := len(ss)
			if n == 0 {
				continue
			}

			ssLatches := latches[:n]
			for i := range ssLatches {
				latch := &latches[i]
				latch.span = ss[i]
				latch.ts = ifGlobal(ts, s)
				latch.done = &guard.done
				// latch.setID() in Manager.insert, under lock.
			}
			guard.setLatches(s, a, ssLatches)
			latches = latches[n:]
		}
	}
	if len(latches) != 0 {
		panic("alloc too large")
	}
	return guard
}

// Acquire acquires latches from the Manager for each of the provided spans, at
// the specified timestamp. In doing so, it waits for latches over all
// overlapping spans to be released before returning. If the provided context
// is canceled before the method is done waiting for overlapping latches to
// be released, it stops waiting and releases all latches that it has already
// acquired.
//
// It returns a Guard which must be provided to Release.
func (m *Manager) Acquire(
	ctx context.Context, spans *spanset.SpanSet, ts hlc.Timestamp,
) (*Guard, error) {
	lg, snap := m.sequence(spans, ts)
	defer snap.close()

	err := m.wait(ctx, lg, ts, snap)
	if err != nil {
		m.Release(lg)
		return nil, err
	}
	return lg, nil
}

// sequence locks the manager, captures an immutable snapshot, inserts latches
// for each of the specified spans into the manager's interval trees, and
// unlocks the manager. The role of the method is to sequence latch acquisition
// attempts.
func (m *Manager) sequence(spans *spanset.SpanSet, ts hlc.Timestamp) (*Guard, snapshot) {
	lg := newGuard(spans, ts)

	m.mu.Lock()
	snap := m.snapshotLocked(spans)
	m.insertLocked(lg)
	m.mu.Unlock()
	return lg, snap
}

// snapshot is an immutable view into the latch manager's state.
type snapshot struct {
	trees [spanset.NumSpanScope][spanset.NumSpanAccess]btree
}

// close closes the snapshot and releases any associated resources.
func (sn *snapshot) close() {
	for s := spanset.SpanScope(0); s < spanset.NumSpanScope; s++ {
		for a := spanset.SpanAccess(0); a < spanset.NumSpanAccess; a++ {
			sn.trees[s][a].Reset()
		}
	}
}

// snapshotLocked captures an immutable snapshot of the latch manager. It takes
// a spanset to limit the amount of state captured.
func (m *Manager) snapshotLocked(spans *spanset.SpanSet) snapshot {
	var snap snapshot
	for s := spanset.SpanScope(0); s < spanset.NumSpanScope; s++ {
		sm := &m.scopes[s]
		reading := len(spans.GetSpans(spanset.SpanReadOnly, s)) > 0
		writing := len(spans.GetSpans(spanset.SpanReadWrite, s)) > 0

		if writing {
			sm.flushReadSetLocked()
			snap.trees[s][spanset.SpanReadOnly] = sm.trees[spanset.SpanReadOnly].Clone()
		}
		if writing || reading {
			snap.trees[s][spanset.SpanReadWrite] = sm.trees[spanset.SpanReadWrite].Clone()
		}
	}
	return snap
}

// flushReadSetLocked flushes the read set into the read interval tree.
func (sm *scopedManager) flushReadSetLocked() {
	for sm.readSet.len > 0 {
		latch := sm.readSet.front()
		sm.readSet.remove(latch)
		sm.trees[spanset.SpanReadOnly].Set(latch)
	}
}

// insertLocked inserts the latches owned by the provided Guard into the
// Manager.
func (m *Manager) insertLocked(lg *Guard) {
	for s := spanset.SpanScope(0); s < spanset.NumSpanScope; s++ {
		sm := &m.scopes[s]
		for a := spanset.SpanAccess(0); a < spanset.NumSpanAccess; a++ {
			latches := lg.latches(s, a)
			for i := range latches {
				latch := &latches[i]
				latch.id = m.nextID()
				switch a {
				case spanset.SpanReadOnly:
					// Add reads to the readSet. They only need to enter
					// the read tree if they're flushed by a write capturing
					// a snapshot.
					sm.readSet.pushBack(latch)
				case spanset.SpanReadWrite:
					// Add writes directly to the write tree.
					sm.trees[spanset.SpanReadWrite].Set(latch)
				default:
					panic("unknown access")
				}
			}
		}
	}
}

func (m *Manager) nextID() uint64 {
	m.idAlloc++
	return m.idAlloc
}

// ignoreFn is used for non-interference of earlier reads with later writes.
//
// However, this is only desired for the global scope. Reads and writes to local
// keys are specified to always interfere, regardless of their timestamp. This
// is done to avoid confusion with local keys declared as part of proposer
// evaluated KV.
//
// This is also disabled in the global scope if either of the timestamps are
// empty. In those cases, we consider the latch without a timestamp to be a
// non-MVCC operation that affects all timestamps in the key range.
type ignoreFn func(ts, other hlc.Timestamp) bool

func ignoreLater(ts, other hlc.Timestamp) bool   { return !ts.IsEmpty() && ts.Less(other) }
func ignoreEarlier(ts, other hlc.Timestamp) bool { return !other.IsEmpty() && other.Less(ts) }
func ignoreNothing(ts, other hlc.Timestamp) bool { return false }

func ifGlobal(ts hlc.Timestamp, s spanset.SpanScope) hlc.Timestamp {
	switch s {
	case spanset.SpanGlobal:
		return ts
	case spanset.SpanLocal:
		// All local latches interfere.
		return hlc.Timestamp{}
	default:
		panic("unknown scope")
	}
}

// wait waits for all interfering latches in the provided snapshot to complete
// before returning.
func (m *Manager) wait(ctx context.Context, lg *Guard, ts hlc.Timestamp, snap snapshot) error {
	for s := spanset.SpanScope(0); s < spanset.NumSpanScope; s++ {
		tr := &snap.trees[s]
		for a := spanset.SpanAccess(0); a < spanset.NumSpanAccess; a++ {
			latches := lg.latches(s, a)
			for i := range latches {
				latch := &latches[i]
				switch a {
				case spanset.SpanReadOnly:
					// Wait for writes at equal or lower timestamps.
					it := tr[spanset.SpanReadWrite].MakeIter()
					if err := iterAndWait(ctx, &it, latch, ts, ignoreLater); err != nil {
						return err
					}
				case spanset.SpanReadWrite:
					// Wait for all other writes.
					//
					// It is cheaper to wait on an already released latch than
					// it is an unreleased latch so we prefer waiting on longer
					// latches first. We expect writes to take longer than reads
					// to release their latches, so we wait on them first.
					it := tr[spanset.SpanReadWrite].MakeIter()
					if err := iterAndWait(ctx, &it, latch, ts, ignoreNothing); err != nil {
						return err
					}
					// Wait for reads at equal or higher timestamps.
					it = tr[spanset.SpanReadOnly].MakeIter()
					if err := iterAndWait(ctx, &it, latch, ts, ignoreEarlier); err != nil {
						return err
					}
				default:
					panic("unknown access")
				}
			}
		}
	}
	return nil
}

// iterAndWait uses the provided iterator to wait on all latches that overlap
// with the search latch and which should not be ignored given their timestamp
// and the supplied ignoreFn.
func iterAndWait(
	ctx context.Context, it *iterator, search *latch, ts hlc.Timestamp, ignore ignoreFn,
) error {
	done := ctx.Done()
	for it.FirstOverlap(search); it.Valid(); it.NextOverlap() {
		latch := it.Cur()
		if latch.done.signaled() {
			continue
		}
		if ignore(ts, latch.ts) {
			continue
		}
		select {
		case <-latch.done.signalChan():
		case <-done:
			return ctx.Err()
		}
	}
	return nil
}

// Release releases the latches held by the provided Guard. After being called,
// dependent latch acquisition attempts can complete if not blocked on any other
// owned latches.
func (m *Manager) Release(lg *Guard) {
	lg.done.signal()

	m.mu.Lock()
	m.removeLocked(lg)
	m.mu.Unlock()
}

// removeLocked removes the latches owned by the provided Guard from the
// Manager. Must be called with mu held.
func (m *Manager) removeLocked(lg *Guard) {
	for s := spanset.SpanScope(0); s < spanset.NumSpanScope; s++ {
		sm := &m.scopes[s]
		for a := spanset.SpanAccess(0); a < spanset.NumSpanAccess; a++ {
			latches := lg.latches(s, a)
			for i := range latches {
				latch := &latches[i]
				if latch.inReadSet() {
					sm.readSet.remove(latch)
				} else {
					sm.trees[a].Delete(latch)
				}
			}
		}
	}
}
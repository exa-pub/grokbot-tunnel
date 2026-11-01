package replies

import "google.golang.org/protobuf/proto"

// Limits for a single submit batch.
const (
	maxBatchFrames = 256
	maxBatchBytes  = 8 << 20 // serialized frames
	maxBatchData   = 1 << 20 // file payload
)

// Batch fairly interleaves requests while preserving order within each one.
// Metadata goes first; file chunks, at most one per request, are skipped every
// other batch while metadata is waiting so neither kind starves the other.
// Requests are served in q.order, which Accepted rotates; it forgets those
// without pending frames.
func (q *Queue) Batch() []*Frame {
	b := newBatcher(q.pending, q.order)
	q.order, q.ordered = b.order, make(map[string]bool, len(b.order))
	for _, id := range b.order {
		q.ordered[id] = true
	}
	for progress := true; progress; {
		progress = false
		for _, id := range b.order {
			progress = b.take(id, false) || progress
		}
	}
	if len(b.out) == 0 || !q.previousFile {
		for _, id := range b.order {
			b.take(id, true)
		}
	}
	q.previousFile = b.data > 0
	return b.out
}

// batcher fills one submit batch from per-request lanes.
type batcher struct {
	lanes map[string][]*Frame
	order []string // request IDs in serving order
	out   []*Frame
	size  int
	data  int
}

// newBatcher splits pending into lanes served in order, skipping requests
// without pending frames, and then in order of first appearance.
func newBatcher(pending []*Frame, order []string) *batcher {
	b := &batcher{lanes: map[string][]*Frame{}}
	for _, f := range pending {
		b.lanes[f.RequestId] = append(b.lanes[f.RequestId], f)
	}
	listed := make(map[string]bool, len(b.lanes))
	for _, id := range order {
		if len(b.lanes[id]) > 0 && !listed[id] {
			listed[id] = true
			b.order = append(b.order, id)
		}
	}
	for _, f := range pending {
		if !listed[f.RequestId] {
			listed[f.RequestId] = true
			b.order = append(b.order, f.RequestId)
		}
	}
	return b
}

// take moves the head of the request's lane into the batch if it is of the
// wanted kind and fits. The first frame always fits.
func (b *batcher) take(id string, files bool) bool {
	q := b.lanes[id]
	if len(q) == 0 {
		return false
	}
	f := q[0]
	data := len(f.GetFile().GetData())
	if (data > 0) != files {
		return false
	}
	size := proto.Size(f)
	full := len(b.out) >= maxBatchFrames || b.size+size > maxBatchBytes || b.data+data > maxBatchData
	if len(b.out) > 0 && full {
		return false
	}
	b.out = append(b.out, f)
	b.size += size
	b.data += data
	b.lanes[id] = q[1:]
	return true
}

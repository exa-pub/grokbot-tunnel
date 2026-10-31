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
func (q *Queue) Batch() []*Frame {
	b := newBatcher(q.pending)
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
	order []string // request IDs in order of first appearance
	out   []*Frame
	size  int
	data  int
}

func newBatcher(pending []*Frame) *batcher {
	b := &batcher{lanes: map[string][]*Frame{}}
	for _, f := range pending {
		if _, ok := b.lanes[f.RequestId]; !ok {
			b.order = append(b.order, f.RequestId)
		}
		b.lanes[f.RequestId] = append(b.lanes[f.RequestId], f)
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

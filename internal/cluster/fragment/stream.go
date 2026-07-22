package fragment

import (
	"io"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// Streaming write/read path. Unlike Encode/Decode, which hold whole fragment
// payloads in memory, these APIs move bytes through io.Reader/io.Writer in
// fixed-size blocks, so an arbitrarily large object never has to fit in RAM.
//
// Parity cannot be produced in the same pass as the data it protects (the
// codec must read the finished data fragments), so encoding is two-phase:
// EncodeStream first streams the body into the data fragments (replicas or EC
// data shards) in a single pass, then re-reads them via the caller's SourceFunc
// (clusterstore points it at the staging files) to produce the remainder
// fragments. The phase split matches scheme.WriteQuorum: the replica schemes
// ack after W=2 synchronous copies, and the third target's payload — the
// RF=2.5 parity or the RF=3 trailing replica — is produced behind the async
// queue.

// writeQuorumReplicas is the number of replicas the synchronous data phase
// writes for the replica schemes (scheme.WriteQuorum for RF=2.5 and RF=3).
const writeQuorumReplicas = 2

// Item describes one planned fragment: its destination, what it holds, its
// index, and the exact payload size for an object of the planned length.
type Item struct {
	Target placement.Target
	Kind   Kind
	Index  int
	Size   int64
}

// SinkFunc opens the writer that will store one planned fragment's payload.
// EncodeStream closes every writer it opens.
type SinkFunc func(item Item) (io.WriteCloser, error)

// SourceFunc reopens an already-written fragment payload for reading.
// EncodeStream may call it more than once per item — the RF=2.5 parity pass
// reads the primary replica through two concurrent readers (one per half).
type SourceFunc func(item Item) (io.ReadCloser, error)

// OpenFunc opens a surviving fragment payload by index, returning (nil, nil)
// when that fragment is lost. DecodeStream may call it up to twice per index
// (a degraded EC read consumes present shards once for reconstruction and once
// for the final join).
type OpenFunc func(index int) (io.ReadCloser, error)

// StageFunc provides scratch storage (typically an O_TMPFILE / temp file) for
// an EC shard rebuilt during a degraded read. cleanup releases the storage and
// may be nil.
type StageFunc func(index int) (rw io.ReadWriteSeeker, cleanup func(), err error)

// Plan places the scheme's targets for key and returns the fragment layout —
// destination, kind, index and payload size — without producing any payload
// bytes. It returns ErrInsufficientTargets when the cluster cannot host the
// scheme.
func Plan(topo *cluster.Topology, s scheme.Scheme, key string, objSize int64) ([]Item, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	if objSize < 0 {
		return nil, errors.Errorf("negative object size %d", objSize)
	}

	targets := placement.Place(topo, key, s.Copies())
	if len(targets) < s.Copies() {
		return nil, errors.Wrapf(ErrInsufficientTargets, "need %d, got %d", s.Copies(), len(targets))
	}

	switch s.Kind {
	case scheme.RF3:
		items := make([]Item, 3)
		for i, t := range targets {
			items[i] = Item{Target: t, Kind: Replica, Index: i, Size: objSize}
		}

		return items, nil
	case scheme.RF25:
		half := (objSize + 1) / 2
		if objSize == 0 {
			half = 0
		}

		return []Item{
			{Target: targets[0], Kind: Replica, Index: 0, Size: objSize},
			{Target: targets[1], Kind: Replica, Index: 1, Size: objSize},
			{Target: targets[2], Kind: Parity, Index: 2, Size: half},
		}, nil
	case scheme.EC:
		codec, err := s.Codec()
		if err != nil {
			return nil, err
		}

		shardSize := codec.ShardSize(objSize)

		items := make([]Item, s.K+s.M)
		for i, t := range targets {
			items[i] = Item{Target: t, Kind: Shard, Index: i, Size: shardSize}
		}

		return items, nil
	default:
		return nil, errors.Errorf("unknown scheme kind %d", s.Kind)
	}
}

// EncodeStream streams exactly objSize bytes from body into the planned
// fragments. Data fragments are written in a single pass; parity fragments are
// then computed by re-reading the written data fragments through reopen. plan
// must be the unmodified result of Plan for the same scheme and size.
func EncodeStream(plan []Item, s scheme.Scheme, objSize int64, body io.Reader, sink SinkFunc, reopen SourceFunc) error {
	if err := EncodeDataStream(plan, s, objSize, body, sink); err != nil {
		return err
	}

	return EncodeParityStream(plan, s, objSize, sink, reopen)
}

// writeReplicas fans body out to every replica sink in one pass, enforcing the
// exact size.
func writeReplicas(items []Item, objSize int64, body io.Reader, sink SinkFunc) error {
	writers := make([]io.Writer, len(items))
	closers := make([]io.WriteCloser, len(items))

	for i, it := range items {
		w, err := sink(it)
		if err != nil {
			_ = closeWriters(closers[:i])
			return errors.Wrapf(err, "open sink %d", it.Index)
		}

		writers[i], closers[i] = w, w
	}

	if objSize > 0 {
		if _, err := io.CopyN(io.MultiWriter(writers...), body, objSize); err != nil {
			_ = closeWriters(closers)
			return errors.Wrap(err, "stream replicas")
		}
	}

	return closeWriters(closers)
}

// writeHalfParity computes the RF=2.5 parity fragment by re-reading the primary
// replica (plan[0]) through two readers, one per half.
func writeHalfParity(plan []Item, objSize int64, sink SinkFunc, reopen SourceFunc) error {
	pw, err := sink(plan[2])
	if err != nil {
		return errors.Wrap(err, "open parity sink")
	}

	if objSize == 0 {
		if err := pw.Close(); err != nil {
			return errors.Wrap(err, "close parity sink")
		}

		return nil
	}

	first, err := reopen(plan[0])
	if err != nil {
		_ = pw.Close()
		return errors.Wrap(err, "reopen primary replica")
	}

	defer func() { _ = first.Close() }()

	second, err := reopen(plan[0])
	if err != nil {
		_ = pw.Close()
		return errors.Wrap(err, "reopen primary replica for second half")
	}

	defer func() { _ = second.Close() }()

	// Position the second reader at the half boundary.
	half := (objSize + 1) / 2
	if _, err := io.CopyN(io.Discard, second, half); err != nil {
		_ = pw.Close()
		return errors.Wrap(err, "seek to second half")
	}

	if err := scheme.HalfParityStream(first, second, objSize, pw); err != nil {
		_ = pw.Close()
		return err
	}

	if err := pw.Close(); err != nil {
		return errors.Wrap(err, "close parity sink")
	}

	return nil
}

// writeECShards splits body into the k data shard sinks in one pass, then
// re-reads them to produce the m parity shards.
func writeECShards(plan []Item, s scheme.Scheme, objSize int64, body io.Reader, sink SinkFunc) error {
	if objSize == 0 {
		// Every shard is empty; just materialize the fragments.
		for _, it := range plan {
			w, err := sink(it)
			if err != nil {
				return errors.Wrapf(err, "open sink %d", it.Index)
			}

			if err := w.Close(); err != nil {
				return errors.Wrapf(err, "close sink %d", it.Index)
			}
		}

		return nil
	}

	codec, err := s.Codec()
	if err != nil {
		return err
	}

	// Phase 1: single pass body → k data shards.
	dataW := make([]io.Writer, s.K)
	dataC := make([]io.WriteCloser, s.K)

	for i := range s.K {
		w, err := sink(plan[i])
		if err != nil {
			_ = closeWriters(dataC[:i])
			return errors.Wrapf(err, "open data sink %d", i)
		}

		dataW[i], dataC[i] = w, w
	}

	if err := codec.SplitStream(body, dataW, objSize); err != nil {
		_ = closeWriters(dataC)
		return err
	}

	if err := closeWriters(dataC); err != nil {
		return err
	}

	return nil
}

// EncodeDataStream is the synchronous half of the write path: it streams the
// body into only the write-quorum fragments (the first two replicas for the
// replica schemes, or the EC data shards) in a single pass — the fragments a
// write quorum waits on. The remainder is produced afterwards by
// EncodeParityStream (typically behind the async repair queue). EncodeStream
// chains the two for callers that want both at once.
func EncodeDataStream(plan []Item, s scheme.Scheme, objSize int64, body io.Reader, sink SinkFunc) error {
	if err := s.Validate(); err != nil {
		return err
	}

	if len(plan) != s.Copies() {
		return errors.Errorf("plan has %d items, scheme needs %d", len(plan), s.Copies())
	}

	switch s.Kind {
	case scheme.RF3, scheme.RF25:
		return writeReplicas(plan[:writeQuorumReplicas], objSize, body, sink)
	case scheme.EC:
		return writeECShards(plan, s, objSize, body, sink)
	default:
		return errors.Errorf("unknown scheme kind %d", s.Kind)
	}
}

// EncodeParityStream produces the remainder fragments for an already-written
// object by re-reading its data fragments: the RF=2.5 half-parity, the RF=3
// trailing replica, or the EC parity shards. It is the asynchronous half of
// the write path (see EncodeDataStream). plan must be the full Plan result.
func EncodeParityStream(plan []Item, s scheme.Scheme, objSize int64, sink SinkFunc, reopen SourceFunc) error {
	if err := s.Validate(); err != nil {
		return err
	}

	if len(plan) != s.Copies() {
		return errors.Errorf("plan has %d items, scheme needs %d", len(plan), s.Copies())
	}

	switch s.Kind {
	case scheme.RF3:
		return writeThirdReplica(plan, objSize, sink, reopen)
	case scheme.RF25:
		return writeHalfParity(plan, objSize, sink, reopen)
	case scheme.EC:
		return writeECParity(plan, s, objSize, sink, reopen)
	default:
		return errors.Errorf("unknown scheme kind %d", s.Kind)
	}
}

// writeThirdReplica produces the RF=3 trailing replica by re-reading the
// primary (plan[0]) — the async remainder of an RF=3 write, mirroring the
// RF=2.5 parity pass.
func writeThirdReplica(plan []Item, objSize int64, sink SinkFunc, reopen SourceFunc) error {
	w, err := sink(plan[2])
	if err != nil {
		return errors.Wrap(err, "open third replica sink")
	}

	if objSize > 0 {
		r, err := reopen(plan[0])
		if err != nil {
			_ = w.Close()
			return errors.Wrap(err, "reopen primary replica")
		}

		_, err = io.CopyN(w, r, objSize)
		_ = r.Close()

		if err != nil {
			_ = w.Close()
			return errors.Wrap(err, "stream third replica")
		}
	}

	if err := w.Close(); err != nil {
		return errors.Wrap(err, "close third replica sink")
	}

	return nil
}

// writeECParity re-reads the k data shards and streams the m parity shards.
func writeECParity(plan []Item, s scheme.Scheme, objSize int64, sink SinkFunc, reopen SourceFunc) error {
	if objSize == 0 {
		return nil
	}

	codec, err := s.Codec()
	if err != nil {
		return err
	}

	readers := make([]io.Reader, s.K)
	rc := make([]io.Closer, 0, s.K)

	defer func() { closeAll(rc) }()

	for i := range s.K {
		r, err := reopen(plan[i])
		if err != nil {
			return errors.Wrapf(err, "reopen data shard %d", i)
		}

		readers[i] = r
		rc = append(rc, r)
	}

	parityW := make([]io.Writer, s.M)
	parityC := make([]io.WriteCloser, s.M)

	for i := range s.M {
		w, err := sink(plan[s.K+i])
		if err != nil {
			_ = closeWriters(parityC[:i])
			return errors.Wrapf(err, "open parity sink %d", s.K+i)
		}

		parityW[i], parityC[i] = w, w
	}

	if err := codec.EncodeStream(readers, parityW); err != nil {
		_ = closeWriters(parityC)
		return err
	}

	return closeWriters(parityC)
}

// DecodeStream reconstructs the object into w from surviving fragments. For the
// replica schemes any surviving full replica is streamed straight through; for
// EC the data shards are joined, reconstructing missing ones from parity into
// stage-provided scratch storage first (stage may be nil when no degraded read
// is expected — a degraded read then fails). Returns ErrUnrecoverable when too
// few fragments survive.
func DecodeStream(s scheme.Scheme, origLen int64, open OpenFunc, stage StageFunc, w io.Writer) error {
	if err := s.Validate(); err != nil {
		return err
	}

	if origLen == 0 {
		return nil
	}

	switch s.Kind {
	case scheme.RF25, scheme.RF3:
		return decodeReplicaStream(s, origLen, open, w)
	case scheme.EC:
		return decodeECStream(s, origLen, open, stage, w)
	default:
		return errors.Errorf("unknown scheme kind %d", s.Kind)
	}
}

// decodeReplicaStream streams the first surviving full replica.
func decodeReplicaStream(s scheme.Scheme, origLen int64, open OpenFunc, w io.Writer) error {
	for i := range s.FullReplicas() {
		r, err := open(i)
		if err != nil {
			return errors.Wrapf(err, "open replica %d", i)
		}

		if r == nil {
			continue
		}

		_, err = io.CopyN(w, r, origLen)
		_ = r.Close()

		if err != nil {
			return errors.Wrap(err, "stream replica")
		}

		return nil
	}

	return errors.Wrap(ErrUnrecoverable, "no surviving replica")
}

// decodeECStream joins the data shards, reconstructing missing ones first.
func decodeECStream(s scheme.Scheme, origLen int64, open OpenFunc, stage StageFunc, w io.Writer) error {
	codec, err := s.Codec()
	if err != nil {
		return err
	}

	total := s.K + s.M

	// Open the data shards; the happy path needs nothing else.
	data := make([]io.ReadCloser, s.K)
	missing := 0

	for i := range s.K {
		r, err := open(i)
		if err != nil {
			closeRead(data[:i])
			return errors.Wrapf(err, "open shard %d", i)
		}

		if r == nil {
			missing++
		}

		data[i] = r
	}

	if missing == 0 {
		defer closeRead(data)

		readers := make([]io.Reader, s.K)
		for i, r := range data {
			readers[i] = r
		}

		return codec.JoinStream(w, readers, origLen)
	}

	if stage == nil {
		closeRead(data)
		return errors.New("degraded EC read requires shard staging (stage is nil)")
	}

	// Degraded: bring in parity, rebuild the missing data shards into staging.
	valid := make([]io.Reader, total)
	present := s.K - missing

	var openClosers []io.Closer

	for i := range s.K {
		if data[i] != nil {
			valid[i] = data[i]
			openClosers = append(openClosers, data[i])
		}
	}

	for i := s.K; i < total; i++ {
		r, err := open(i)
		if err != nil {
			closeAll(openClosers)
			return errors.Wrapf(err, "open parity shard %d", i)
		}

		if r != nil {
			valid[i] = r
			present++

			openClosers = append(openClosers, r)
		}
	}

	if present < s.K {
		closeAll(openClosers)
		return errors.Wrapf(ErrUnrecoverable, "have %d shards, need %d", present, s.K)
	}

	fill := make([]io.Writer, total)
	staged := make(map[int]io.ReadWriteSeeker, missing)

	var cleanups []func()

	release := func() {
		for _, f := range cleanups {
			f()
		}
	}

	for i := range s.K {
		if valid[i] != nil {
			continue
		}

		rw, cleanup, err := stage(i)
		if err != nil {
			closeAll(openClosers)
			release()

			return errors.Wrapf(err, "stage shard %d", i)
		}

		fill[i] = rw
		staged[i] = rw

		if cleanup != nil {
			cleanups = append(cleanups, cleanup)
		}
	}

	defer release()

	if err := codec.ReconstructStream(valid, fill); err != nil {
		closeAll(openClosers)
		return err
	}

	closeAll(openClosers)

	// Join from a mix of freshly-reopened present shards and rewound staged
	// rebuilds.
	readers := make([]io.Reader, s.K)

	var joinClosers []io.Closer

	defer func() { closeAll(joinClosers) }()

	for i := range s.K {
		if rw, ok := staged[i]; ok {
			if _, err := rw.Seek(0, io.SeekStart); err != nil {
				return errors.Wrapf(err, "rewind staged shard %d", i)
			}

			readers[i] = rw

			continue
		}

		r, err := open(i)
		if err != nil || r == nil {
			return errors.Wrapf(errors.Wrap(err, "fragment disappeared during decode"), "reopen shard %d", i)
		}

		readers[i] = r
		joinClosers = append(joinClosers, r)
	}

	return codec.JoinStream(w, readers, origLen)
}

// closeWriters closes every non-nil writer, returning the first error (a failed
// close on a sink means the fragment may not be durable).
func closeWriters(ws []io.WriteCloser) error {
	var first error

	for _, w := range ws {
		if w == nil {
			continue
		}

		if err := w.Close(); err != nil && first == nil {
			first = errors.Wrap(err, "close fragment sink")
		}
	}

	return first
}

// closeRead closes every non-nil reader, ignoring errors (read side).
func closeRead(rs []io.ReadCloser) {
	for _, r := range rs {
		if r != nil {
			_ = r.Close()
		}
	}
}

// closeAll closes every closer, ignoring errors (read side).
func closeAll(cs []io.Closer) {
	for _, c := range cs {
		_ = c.Close()
	}
}

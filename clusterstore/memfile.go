package clusterstore

import (
	"io"

	"github.com/go-faster/errors"
)

// memStage is the fragment.StageFunc used for degraded EC reads: rebuilt
// shards are staged in memory. A shard is objSize/k, so the worst case holds
// up to m in-flight shards; spilling large shards to temp files is a follow-up
// once the filesystem fragment store lands.
func memStage(int) (io.ReadWriteSeeker, func(), error) {
	return &memFile{}, nil, nil
}

// memFile is a minimal in-memory io.ReadWriteSeeker.
type memFile struct {
	buf []byte
	off int64
}

func (f *memFile) Read(p []byte) (int, error) {
	if f.off >= int64(len(f.buf)) {
		return 0, io.EOF
	}

	n := copy(p, f.buf[f.off:])
	f.off += int64(n)

	return n, nil
}

func (f *memFile) Write(p []byte) (int, error) {
	if end := f.off + int64(len(p)); end > int64(len(f.buf)) {
		grown := make([]byte, end)
		copy(grown, f.buf)
		f.buf = grown
	}

	n := copy(f.buf[f.off:], p)
	f.off += int64(n)

	return n, nil
}

func (f *memFile) Seek(offset int64, whence int) (int64, error) {
	var base int64

	switch whence {
	case io.SeekStart:
		base = 0
	case io.SeekCurrent:
		base = f.off
	case io.SeekEnd:
		base = int64(len(f.buf))
	default:
		return 0, errors.Errorf("invalid whence %d", whence)
	}

	pos := base + offset
	if pos < 0 {
		return 0, errors.New("negative seek position")
	}

	f.off = pos

	return pos, nil
}

package state

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ting-organization/goting/internal/hashing"
)

// Snapshots let a node restart from a verified state instead of
// re-executing its whole chain. They are a local storage format, not part of
// the protocol: a snapshot is only accepted when the records it holds hash to
// the state root of the block it names.
//
//	magic   "TINGSNAP"
//	version u32 = 1
//	block   bytes32   BlockId the state follows
//	height  u64
//	root    bytes32   state root
//	count   u64
//	count × (key bytes32, u32 length, value), keys strictly increasing

var snapMagic = []byte("TINGSNAP")

// SnapshotInfo describes a snapshot.
type SnapshotInfo struct {
	Block  hashing.Hash
	Height uint64
	Root   hashing.Hash
	Count  uint64
}

// WriteSnapshot writes t atomically to path (via a temporary file).
func WriteSnapshot(path string, info SnapshotInfo, t Tree) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	info.Root = t.Root()
	info.Count = uint64(t.Len())
	hdr := make([]byte, 0, 8+4+32+8+32+8)
	hdr = append(hdr, snapMagic...)
	hdr = binary.LittleEndian.AppendUint32(hdr, 1)
	hdr = append(hdr, info.Block[:]...)
	hdr = binary.LittleEndian.AppendUint64(hdr, info.Height)
	hdr = append(hdr, info.Root[:]...)
	hdr = binary.LittleEndian.AppendUint64(hdr, info.Count)
	w.Write(hdr)
	var werr error
	t.Walk(func(k Key, v []byte) bool {
		var l [4]byte
		binary.LittleEndian.PutUint32(l[:], uint32(len(v)))
		if _, werr = w.Write(k[:]); werr != nil {
			return false
		}
		if _, werr = w.Write(l[:]); werr != nil {
			return false
		}
		_, werr = w.Write(v)
		return werr == nil
	})
	if werr == nil {
		werr = w.Flush()
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// ReadSnapshotInfo reads only a snapshot's header.
func ReadSnapshotInfo(path string) (SnapshotInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return SnapshotInfo{}, err
	}
	defer f.Close()
	return readSnapHeader(bufio.NewReader(f))
}

func readSnapHeader(r io.Reader) (SnapshotInfo, error) {
	var info SnapshotInfo
	hdr := make([]byte, 8+4+32+8+32+8)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return info, fmt.Errorf("snapshot header: %w", err)
	}
	if !bytes.Equal(hdr[:8], snapMagic) || binary.LittleEndian.Uint32(hdr[8:]) != 1 {
		return info, errors.New("not a version 1 snapshot")
	}
	copy(info.Block[:], hdr[12:44])
	info.Height = binary.LittleEndian.Uint64(hdr[44:])
	copy(info.Root[:], hdr[52:84])
	info.Count = binary.LittleEndian.Uint64(hdr[84:])
	return info, nil
}

// ReadSnapshot loads a snapshot and verifies that its records hash to the
// root it declares.
func ReadSnapshot(path string) (SnapshotInfo, Tree, error) {
	f, err := os.Open(path)
	if err != nil {
		return SnapshotInfo{}, Tree{}, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	info, err := readSnapHeader(r)
	if err != nil {
		return info, Tree{}, err
	}
	st, err := f.Stat()
	if err != nil {
		return info, Tree{}, err
	}
	// Each record takes at least 36 bytes: bound the count by the file size.
	if info.Count > uint64(st.Size())/36 {
		return info, Tree{}, errors.New("snapshot record count exceeds file size")
	}
	recs := make([]Change, 0, info.Count)
	var prev Key
	var rec [36]byte
	for i := uint64(0); i < info.Count; i++ {
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			return info, Tree{}, fmt.Errorf("snapshot record %d: %w", i, err)
		}
		var k Key
		copy(k[:], rec[:32])
		if i > 0 && bytes.Compare(k[:], prev[:]) <= 0 {
			return info, Tree{}, errors.New("snapshot keys out of order")
		}
		prev = k
		n := binary.LittleEndian.Uint32(rec[32:])
		if uint64(n) > uint64(st.Size()) {
			return info, Tree{}, errors.New("snapshot value exceeds file size")
		}
		v := make([]byte, n)
		if _, err := io.ReadFull(r, v); err != nil {
			return info, Tree{}, fmt.Errorf("snapshot record %d value: %w", i, err)
		}
		recs = append(recs, Change{Key: k, Value: v})
	}
	if _, err := r.ReadByte(); err != io.EOF {
		return info, Tree{}, errors.New("trailing bytes after snapshot records")
	}
	t := Build(recs)
	if t.Root() != info.Root {
		return info, Tree{}, fmt.Errorf("snapshot records hash to %s, not %s", t.Root(), info.Root)
	}
	return info, t, nil
}

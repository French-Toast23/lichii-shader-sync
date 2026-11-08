// Package cache reads and writes Lichii/Eden "yuzucach" Vulkan pipeline caches
// (vulkan.fork-vNN.bin). Layout verified on fork v52, v53 and v54.
//
// File  = "yuzucach" + u32 version + entries...
// Entry = u32 numEnvs + numEnvs * env + key
// key   = 24 bytes (compute pipeline: one env with stage 5) or 0x420 bytes (graphics)
package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
)

const (
	Magic        = "yuzucach"
	HeaderSize   = 12
	GraphicsKey  = 0x420
	ComputeKey   = 24
	envHeader    = 0x4C // 5 x u64 counts + 9 x u32
	sphSize      = 0x50
	stageExtra78 = 0x5C // stages 7 and 8 carry an extra block
	replSize     = 9    // cbuf replacement: u64 key + u8
	maxCode      = 0x400000
)

// MinVersion/MaxVersion bound the cache versions this layout is known for.
// Newer versions are accepted only if every entry parses (Validate).
const MinVersion = 52

type Entry struct {
	Data []byte   // full entry bytes
	Key  []byte   // pipeline key (tail of Data)
	ID   [16]byte // sha256(key)[:16], used to dedupe across files
}

func le32(b []byte, o int) uint32 { return binary.LittleEndian.Uint32(b[o:]) }
func le64(b []byte, o int) uint64 { return binary.LittleEndian.Uint64(b[o:]) }

// envSize returns the size of the env at p and its stage.
func envSize(b []byte, p int) (int, uint32, error) {
	if p+envHeader > len(b) {
		return 0, 0, errors.New("truncated env header")
	}
	code := le64(b, p)
	ntt, npf, ncv, ncr := le64(b, p+8), le64(b, p+16), le64(b, p+24), le64(b, p+32)
	if code > maxCode || ntt > 4096 || npf > 4096 || ncv > 65536 || ncr > 65536 {
		return 0, 0, errors.New("implausible env counts")
	}
	stage := le32(b, p+0x48)
	n := envHeader + int(code) + int(ntt)*8 + int(npf)*8 + int(ncv)*12 + int(ncr)*replSize
	switch stage {
	case 0, 4:
		n += sphSize
	case 5:
		n += 16
	case 7, 8:
		n += sphSize + stageExtra78
	default:
		return 0, 0, fmt.Errorf("unknown shader stage %d", stage)
	}
	return n, stage, nil
}

// next parses one entry starting at p.
func next(b []byte, p int) (Entry, int, error) {
	if p+4 > len(b) {
		return Entry{}, 0, errors.New("truncated entry")
	}
	ne := le32(b, p)
	if ne < 1 || ne > 5 {
		return Entry{}, 0, fmt.Errorf("bad env count %d", ne)
	}
	q := p + 4
	var first uint32
	for i := uint32(0); i < ne; i++ {
		n, st, err := envSize(b, q)
		if err != nil {
			return Entry{}, 0, err
		}
		if i == 0 {
			first = st
		}
		q += n
	}
	k := GraphicsKey
	if ne == 1 && first == 5 {
		k = ComputeKey
	}
	q += k
	if q > len(b) {
		return Entry{}, 0, errors.New("entry runs past end")
	}
	e := Entry{Data: b[p:q], Key: b[q-k : q]}
	h := sha256.Sum256(e.Key)
	copy(e.ID[:], h[:16])
	return e, q, nil
}

// ParseEntries parses a raw run of entries (no header). It fails on any malformed entry.
func ParseEntries(b []byte) ([]Entry, error) {
	var out []Entry
	for p := 0; p < len(b); {
		e, q, err := next(b, p)
		if err != nil {
			return nil, fmt.Errorf("entry %d at %d: %w", len(out), p, err)
		}
		out = append(out, e)
		p = q
	}
	return out, nil
}

type File struct {
	Version uint32
	Entries []Entry
}

func Parse(b []byte) (*File, error) {
	if len(b) < HeaderSize || !bytes.Equal(b[:8], []byte(Magic)) {
		return nil, errors.New("not a yuzucach file")
	}
	v := le32(b, 8)
	if v < MinVersion {
		return nil, fmt.Errorf("cache version %d is older than supported (%d)", v, MinVersion)
	}
	es, err := ParseEntries(b[HeaderSize:])
	if err != nil {
		return nil, err
	}
	return &File{Version: v, Entries: es}, nil
}

func Load(path string) (*File, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Header(version uint32) []byte {
	h := make([]byte, HeaderSize)
	copy(h, Magic)
	binary.LittleEndian.PutUint32(h[8:], version)
	return h
}

package cache

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// Index is what sync needs to know about a local cache without loading it:
// its version, entry count, the ID of every entry and the byte size of the valid data.
type Index struct {
	Version uint32
	IDs     map[[16]byte]bool
	Count   int
	Size    int64 // header + all complete entries
}

// countingReader tracks the absolute offset of a buffered reader.
type countingReader struct {
	f   *os.File
	r   *bufio.Reader
	off int64
}

func (c *countingReader) read(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(c.r, b)
	c.off += int64(n)
	return b, err
}

// skip moves forward n bytes: within the buffer it discards, otherwise it seeks the file
// (so shader code is never read from disk).
func (c *countingReader) skip(n int64) error {
	if n <= int64(c.r.Buffered()) {
		d, err := c.r.Discard(int(n))
		c.off += int64(d)
		return err
	}
	c.off += n
	if _, err := c.f.Seek(c.off, io.SeekStart); err != nil {
		return err
	}
	c.r.Reset(c.f)
	return nil
}

// Each calls fn for every entry of a cache file, with the full entry bytes and its ID, reading
// sequentially (shader code included). It returns the file version. Validation rules match Parse.
func Each(path string, fn func(entry []byte, id [16]byte) error) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	h := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, h); err != nil || !bytes.Equal(h[:8], []byte(Magic)) {
		return 0, errors.New("not a yuzucach file")
	}
	ver := binary.LittleEndian.Uint32(h[8:])
	for n := 0; ; n++ {
		var e []byte
		nb := make([]byte, 4)
		if _, err := io.ReadFull(r, nb); err == io.EOF {
			return ver, nil
		} else if err != nil {
			return 0, fmt.Errorf("entry %d: truncated", n)
		}
		e = append(e, nb...)
		ne := binary.LittleEndian.Uint32(nb)
		if ne < 1 || ne > 5 {
			return 0, fmt.Errorf("entry %d: bad env count %d", n, ne)
		}
		var first uint32
		for i := uint32(0); i < ne; i++ {
			eh := make([]byte, envHeader)
			if _, err := io.ReadFull(r, eh); err != nil {
				return 0, fmt.Errorf("entry %d: truncated env header", n)
			}
			size, stage, err := envSize(eh, 0)
			if err != nil {
				return 0, fmt.Errorf("entry %d: %v", n, err)
			}
			if i == 0 {
				first = stage
			}
			rest := make([]byte, size-envHeader)
			if _, err := io.ReadFull(r, rest); err != nil {
				return 0, fmt.Errorf("entry %d: truncated env", n)
			}
			e = append(e, eh...)
			e = append(e, rest...)
		}
		k := GraphicsKey
		if ne == 1 && first == 5 {
			k = ComputeKey
		}
		key := make([]byte, k)
		if _, err := io.ReadFull(r, key); err != nil {
			return 0, fmt.Errorf("entry %d: truncated key", n)
		}
		e = append(e, key...)
		var id [16]byte
		s := sha256.Sum256(key)
		copy(id[:], s[:16])
		if err := fn(e, id); err != nil {
			return 0, err
		}
	}
}

// Scan walks a cache file entry by entry, reading only each env header and the key and
// skipping the shader code, so memory stays small and big files scan quickly.
// It applies the same validation rules as Parse.
func Scan(path string) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	end := st.Size()
	c := &countingReader{f: f, r: bufio.NewReaderSize(f, 64<<10)}
	h, err := c.read(HeaderSize)
	if err != nil || !bytes.Equal(h[:8], []byte(Magic)) {
		return nil, errors.New("not a yuzucach file")
	}
	ix := &Index{Version: binary.LittleEndian.Uint32(h[8:]), IDs: map[[16]byte]bool{}}
	if ix.Version < MinVersion {
		return nil, fmt.Errorf("cache version %d is older than supported (%d)", ix.Version, MinVersion)
	}
	for c.off < end {
		start := c.off
		nb, err := c.read(4)
		if err != nil {
			return nil, fmt.Errorf("entry %d at %d: truncated entry", ix.Count, start)
		}
		ne := binary.LittleEndian.Uint32(nb)
		if ne < 1 || ne > 5 {
			return nil, fmt.Errorf("entry %d at %d: bad env count %d", ix.Count, start, ne)
		}
		var first uint32
		for i := uint32(0); i < ne; i++ {
			eh, err := c.read(envHeader)
			if err != nil {
				return nil, fmt.Errorf("entry %d at %d: truncated env header", ix.Count, start)
			}
			// reuse envSize's rules on the header alone (it only reads the first 0x4C bytes)
			n, stage, err := envSize(eh, 0)
			if err != nil {
				return nil, fmt.Errorf("entry %d at %d: %v", ix.Count, start, err)
			}
			if i == 0 {
				first = stage
			}
			if c.off+int64(n-envHeader) > end {
				return nil, fmt.Errorf("entry %d at %d: runs past end", ix.Count, start)
			}
			if err := c.skip(int64(n - envHeader)); err != nil {
				return nil, err
			}
		}
		k := GraphicsKey
		if ne == 1 && first == 5 {
			k = ComputeKey
		}
		key, err := c.read(k)
		if err != nil || c.off > end {
			return nil, fmt.Errorf("entry %d at %d: runs past end", ix.Count, start)
		}
		var id [16]byte
		s := sha256.Sum256(key)
		copy(id[:], s[:16])
		ix.IDs[id] = true
		ix.Count++
	}
	ix.Size = c.off
	return ix, nil
}

// Package pack builds and reads the published shader packs.
//
// Per game (title ID) there is one manifest, <TITLEID>.json, kept as an asset of the rolling
// "manifests" release. It lists chunk files; each chunk is a gzip-compressed run of raw cache
// entries (no header) stored in a release tagged <TITLEID>-<date>. Chunks are append-only: a new
// pack reuses every earlier chunk (by the tag that first published it) and only adds new ones,
// so users download just what's new.
package pack

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"sort"

	"lichii-sync/cache"
)

const Format = 1

type Chunk struct {
	File    string   `json:"file"`    // <sha256 prefix>.gz
	Tag     string   `json:"tag"`     // release tag that holds the file
	SHA256  string   `json:"sha256"`  // of the compressed file
	Size    int64    `json:"size"`    // compressed bytes
	Entries int      `json:"entries"` //
	IDs     []string `json:"ids"`     // hex sha256(key)[:16] of each entry, in file order
}

type Manifest struct {
	Format  int     `json:"format"`
	TitleID string  `json:"title_id"`       // upper-case hex
	Name    string  `json:"name,omitempty"` // game name shown in the GUI
	Source  uint32  `json:"source_version"` // cache version the newest entries came from
	Total   int     `json:"total_entries"`
	Chunks  []Chunk `json:"chunks"`
}

func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Format != Format {
		return nil, fmt.Errorf("manifest format %d not supported", m.Format)
	}
	return &m, nil
}

func LoadManifest(path string) (*Manifest, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseManifest(b)
}

func (m *Manifest) Save(path string) error {
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(path, b, 0o644)
}

func (m *Manifest) Has() map[string]bool {
	h := map[string]bool{}
	for _, c := range m.Chunks {
		for _, id := range c.IDs {
			h[id] = true
		}
	}
	return h
}

// Build adds every entry of f that prev doesn't have to new chunks in outDir.
func Build(f *cache.File, titleID, tag, outDir string, prev *Manifest, perChunk int) (*Manifest, []Chunk, error) {
	m := &Manifest{Format: Format, TitleID: titleID, Source: f.Version}
	have := map[string]bool{}
	if prev != nil {
		m.Name = prev.Name // keep the game name across pack updates
		m.Chunks = append(m.Chunks, prev.Chunks...)
		have = prev.Has()
	}
	var fresh []cache.Entry
	for _, e := range f.Entries {
		id := hex.EncodeToString(e.ID[:])
		if !have[id] {
			have[id] = true
			fresh = append(fresh, e)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return string(fresh[i].ID[:]) < string(fresh[j].ID[:]) })
	var added []Chunk
	for i := 0; i < len(fresh); i += perChunk {
		end := i + perChunk
		if end > len(fresh) {
			end = len(fresh)
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
		c := Chunk{Tag: tag, Entries: end - i}
		for _, e := range fresh[i:end] {
			zw.Write(e.Data)
			c.IDs = append(c.IDs, hex.EncodeToString(e.ID[:]))
		}
		zw.Close()
		z := buf.Bytes()
		sum := sha256.Sum256(z)
		c.SHA256 = hex.EncodeToString(sum[:])
		c.File = c.SHA256[:16] + ".gz"
		c.Size = int64(len(z))
		if err := ioutil.WriteFile(filepath.Join(outDir, c.File), z, 0o644); err != nil {
			return nil, nil, err
		}
		m.Chunks = append(m.Chunks, c)
		added = append(added, c)
	}
	for _, c := range m.Chunks {
		m.Total += c.Entries
	}
	return m, added, nil
}

// Decode verifies and unpacks a downloaded chunk, returning its parsed entries.
func Decode(z []byte, c Chunk) ([]cache.Entry, error) {
	sum := sha256.Sum256(z)
	if hex.EncodeToString(sum[:]) != c.SHA256 {
		return nil, fmt.Errorf("chunk %s: checksum mismatch", c.File)
	}
	zr, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %v", c.File, err)
	}
	raw, err := ioutil.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %v", c.File, err)
	}
	es, err := cache.ParseEntries(raw)
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %v", c.File, err)
	}
	if len(es) != c.Entries {
		return nil, fmt.Errorf("chunk %s: %d entries, manifest says %d", c.File, len(es), c.Entries)
	}
	for i, e := range es {
		if hex.EncodeToString(e.ID[:]) != c.IDs[i] {
			return nil, fmt.Errorf("chunk %s: entry %d id mismatch", c.File, i)
		}
	}
	return es, nil
}

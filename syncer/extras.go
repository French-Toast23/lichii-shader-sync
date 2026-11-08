package syncer

import (
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"lichii-sync/cache"
)

// ErrCancelled is returned when the user stops a sync; the cache being written is left unchanged.
var ErrCancelled = errors.New("cancelled")

// ---- share my shaders ----

// ExportExtras writes every pipeline the user has for g that the online pack doesn't, from all of
// g's cache files (deduplicated), as a gzip-compressed cache file (header + entries).
// It returns how many pipelines were written. The result can be merged with `lichii-sync pack`.
func ExportExtras(g *Game, out string) (int, error) {
	if g.Manifest == nil {
		return 0, fmt.Errorf("no online pack to compare with")
	}
	online := map[[16]byte]bool{}
	for _, c := range g.Manifest.Chunks {
		for _, s := range c.IDs {
			if id, ok := hexID(s); ok {
				online[id] = true
			}
		}
	}
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	zw, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	var version uint32
	wrote := map[[16]byte]bool{}
	n := 0
	for _, cf := range g.Files {
		if cf.idx == nil || cf.Extra <= 0 {
			continue
		}
		if version == 0 { // newest-format file first is not needed: entries are version-independent
			version = cf.Version
			zw.Write(cache.Header(version))
		}
		_, err := cache.Each(cf.Path, func(e []byte, id [16]byte) error {
			if online[id] || wrote[id] {
				return nil
			}
			wrote[id] = true
			n++
			_, err := zw.Write(e)
			return err
		})
		if err != nil {
			zw.Close()
			f.Close()
			os.Remove(tmp)
			return 0, fmt.Errorf("%s: %v", filepath.Base(cf.Path), err)
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	f.Close()
	if n == 0 {
		os.Remove(tmp)
		return 0, nil
	}
	os.Remove(out)
	return n, os.Rename(tmp, out)
}

// ---- backups ----

// Backup is a <cache>.bak left by the last update of that cache file.
type Backup struct {
	Path, Current string
	Size          int64
}

// Backups lists every .bak next to a cache file under data\cache\shader.
func Backups(data string) []Backup {
	var out []Backup
	root := filepath.Join(data, "cache", "shader")
	dirs, _ := ioutil.ReadDir(root)
	for _, d := range dirs {
		if !d.IsDir() || !TitleRe.MatchString(d.Name()) {
			continue
		}
		ents, _ := ioutil.ReadDir(filepath.Join(root, d.Name()))
		for _, e := range ents {
			name := e.Name()
			if !strings.HasSuffix(name, ".bak") || !cacheName.MatchString(strings.TrimSuffix(name, ".bak")) {
				continue
			}
			p := filepath.Join(root, d.Name(), name)
			out = append(out, Backup{Path: p, Current: strings.TrimSuffix(p, ".bak"), Size: e.Size()})
		}
	}
	return out
}

// DeleteBackups removes the given backups and returns the bytes freed.
func DeleteBackups(bs []Backup) (int64, error) {
	var freed int64
	for _, b := range bs {
		if err := os.Remove(b.Path); err != nil {
			return freed, err
		}
		freed += b.Size
	}
	return freed, nil
}

// Undo swaps a cache file with its backup, so the previous version becomes current and the
// updated one becomes the backup (running it again redoes the update).
func Undo(b Backup) error {
	tmp := b.Current + ".lichii-sync.swap"
	if err := os.Rename(b.Current, tmp); err != nil {
		return err
	}
	if err := os.Rename(b.Path, b.Current); err != nil {
		os.Rename(tmp, b.Current)
		return err
	}
	return os.Rename(tmp, b.Path)
}

func hexOf(id [16]byte) string { return hex.EncodeToString(id[:]) }

// ---- app update check ----

// Version of this build; releases are tagged the same way (v1.2.3).
const Version = "v1.0.5"

// LatestApp returns the newest app release tag (v*) and its page, ignoring pack/manifest releases.
func LatestApp(repo string) (tag, page string, err error) {
	b, _, err := get("https://api.github.com/repos/" + repo + "/releases?per_page=30")
	if err != nil {
		return "", "", err
	}
	var rels []struct {
		Tag        string `json:"tag_name"`
		URL        string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(b, &rels); err != nil {
		return "", "", err
	}
	for _, r := range rels {
		if r.Draft || r.Prerelease || !appTag.MatchString(r.Tag) {
			continue
		}
		if tag == "" || newer(r.Tag, tag) {
			tag, page = r.Tag, r.URL
		}
	}
	return tag, page, nil
}

var appTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// newer reports whether version tag a is newer than b (both vX.Y.Z).
func newer(a, b string) bool {
	x, y := appTag.FindStringSubmatch(a), appTag.FindStringSubmatch(b)
	if x == nil || y == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		p, _ := strconv.Atoi(x[i])
		q, _ := strconv.Atoi(y[i])
		if p != q {
			return p > q
		}
	}
	return false
}

// UpdateAvailable returns the newer release tag and page, or "" when this build is current.
func UpdateAvailable(repo string) (string, string) {
	tag, page, err := LatestApp(repo)
	if err != nil || tag == "" || !newer(tag, Version) {
		return "", ""
	}
	return tag, page
}

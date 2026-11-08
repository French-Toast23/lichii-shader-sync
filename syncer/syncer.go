// Package syncer finds local Lichii shader caches and merges in the published packs.
// Shared by the command-line tool and the GUI.
package syncer

import (
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"lichii-sync/cache"
	"lichii-sync/pack"
)

// Repo holds the packs: manifests in release "manifests" as <TITLEID>.json,
// chunks in releases tagged <TITLEID>-<date>.
var Repo = "oceanpacificpeace/lichii-shader-sync"

const ManifestTag = "manifests"

var cacheName = regexp.MustCompile(`^vulkan(-[a-z0-9]+)?\.fork-v(\d+)\.bin$`)
var TitleRe = regexp.MustCompile(`^[0-9a-fA-F]{16}$`)

// Reporter receives progress. Implementations must be safe to call from a worker goroutine.
type Reporter interface {
	Log(msg string)
	Progress(done, total int64) // bytes of chunks downloaded so far / to download
	Cancelled() bool            // checked between chunks; true stops the sync cleanly
}

func DefaultData() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "eden")
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "eden")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "share", "eden")
}

func LichiiRunning() bool {
	var out []byte
	if runtime.GOOS == "windows" {
		out, _ = exec.Command("tasklist", "/FI", "IMAGENAME eq lichii.exe", "/NH").Output()
	} else {
		out, _ = exec.Command("pgrep", "-x", "lichii").Output()
	}
	return strings.Contains(strings.ToLower(string(out)), "lichii")
}

var client = &http.Client{Timeout: 10 * time.Minute}

func get(url string) ([]byte, int, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "lichii-sync")
	r, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, r.StatusCode, fmt.Errorf("%s: HTTP %d", url, r.StatusCode)
	}
	b, err := ioutil.ReadAll(r.Body)
	return b, 200, err
}

// CacheFile is one local vulkan*.fork-vNN.bin. Each Lichii build reads its own file.
type CacheFile struct {
	Path      string
	Version   uint32
	Pipelines int
	Missing   int   // online pipelines this file lacks
	Download  int64 // bytes to fetch for them
	Extra     int   // pipelines this file has that the online pack doesn't
	Err       error
	idx       *cache.Index
}

// Label names the Lichii build that uses this cache file.
func (cf *CacheFile) Label() string {
	base := filepath.Base(cf.Path)
	fork, nameV := "", ""
	if mm := cacheName.FindStringSubmatch(base); mm != nil {
		nameV = mm[2]
		if mm[1] != "" {
			fork = strings.Title(strings.TrimPrefix(mm[1], "-")) + " "
		}
	}
	if nameV != "" && nameV != fmt.Sprint(cf.Version) {
		// file name and header disagree (e.g. a renamed copy): show both so rows stay distinct
		return fmt.Sprintf("%sbuild (file v%s, cache v%d)", fork, nameV, cf.Version)
	}
	switch {
	case fork != "":
		return fmt.Sprintf("%sbuild (cache v%d)", fork, cf.Version)
	case cf.Version == 53:
		return "Build 95 era (cache v53)"
	case cf.Version == 54:
		return "Build 115+ (cache v54)"
	}
	return fmt.Sprintf("Cache v%d", cf.Version)
}

// Game is one title folder under cache\shader.
type Game struct {
	TitleID  string
	Dir      string
	Files    []*CacheFile
	Manifest *pack.Manifest // nil when no pack is online
	Missing  int            // pipelines online that the smallest local file lacks
	Download int64          // bytes to fetch for that file
}

// Scan lists every game cache folder and, for each, the local caches and the online pack.
func Scan(repo, data string, r Reporter) ([]*Game, error) {
	root := filepath.Join(data, "cache", "shader")
	dirs, err := ioutil.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("no shader cache folder at %s - boot a game once in Lichii first", root)
	}
	base := "https://github.com/" + repo + "/releases/download/"
	var games []*Game
	for _, d := range dirs {
		if !d.IsDir() || !TitleRe.MatchString(d.Name()) {
			continue
		}
		g := &Game{TitleID: strings.ToUpper(d.Name()), Dir: filepath.Join(root, d.Name())}
		ents, _ := ioutil.ReadDir(g.Dir)
		for _, de := range ents {
			mm := cacheName.FindStringSubmatch(de.Name())
			if mm == nil {
				continue
			}
			v, _ := strconv.Atoi(mm[2])
			cf := &CacheFile{Path: filepath.Join(g.Dir, de.Name()), Version: uint32(v)}
			if ix, err := cache.Scan(cf.Path); err != nil {
				cf.Err = err
			} else {
				cf.idx, cf.Pipelines = ix, ix.Count
				cf.Version = ix.Version // trust the header, not the file name
			}
			g.Files = append(g.Files, cf)
		}
		mb, code, err := get(base + ManifestTag + "/" + g.TitleID + ".json")
		if err == nil {
			if m, err := pack.ParseManifest(mb); err == nil {
				g.Manifest = m
			} else {
				r.Log(fmt.Sprintf("%s: bad manifest: %v", g.TitleID, err))
			}
		} else if code != 404 {
			return nil, fmt.Errorf("%s: fetching manifest: %v", g.TitleID, err)
		}
		if g.Manifest != nil {
			for _, cf := range g.Files {
				if cf.idx == nil {
					continue
				}
				_, size, miss := plan(cf, g.Manifest)
				cf.Missing, cf.Download = miss, size
				cf.Extra = cf.Pipelines - (g.Manifest.Total - miss)
				if miss > g.Missing {
					g.Missing, g.Download = miss, size
				}
			}
		}
		games = append(games, g)
	}
	return games, nil
}

func hexID(s string) ([16]byte, bool) {
	var id [16]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return id, false
	}
	copy(id[:], b)
	return id, true
}

// plan returns the chunks a cache file needs, their total size, and how many pipelines it lacks.
func plan(cf *CacheFile, m *pack.Manifest) ([]pack.Chunk, int64, int) {
	var need []pack.Chunk
	var size int64
	miss := 0
	for _, c := range m.Chunks {
		n := 0
		for _, s := range c.IDs {
			if id, ok := hexID(s); ok && !cf.idx.IDs[id] {
				n++
			}
		}
		if n > 0 {
			need = append(need, c)
			size += c.Size
			miss += n
		}
	}
	return need, size, miss
}

// chunkStore downloads each chunk once per run into a temp folder, so several cache files
// can reuse it without holding everything in memory.
type chunkStore struct {
	base, dir string
}

func (s *chunkStore) entries(c pack.Chunk) ([]cache.Entry, error) {
	p := filepath.Join(s.dir, c.File)
	z, err := ioutil.ReadFile(p)
	if err != nil {
		if z, _, err = get(s.base + c.Tag + "/" + c.File); err != nil {
			return nil, err
		}
		ioutil.WriteFile(p, z, 0o644)
	}
	return pack.Decode(z, c)
}

// SyncGame downloads what each local cache file of g is missing and merges it in.
func SyncGame(repo string, g *Game, r Reporter) error {
	return SyncFiles(repo, g, g.Files, r)
}

// SyncFiles is SyncGame limited to the given cache files of g (e.g. the rows a user ticked).
func SyncFiles(repo string, g *Game, files []*CacheFile, r Reporter) error {
	if g.Manifest == nil {
		return nil
	}
	// download onto the cache's own drive (next to the game folders), not %TEMP% on C:
	dir, err := ioutil.TempDir(filepath.Dir(g.Dir), ".lichii-sync-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	store := &chunkStore{base: "https://github.com/" + repo + "/releases/download/", dir: dir}
	for _, cf := range files {
		name := filepath.Base(cf.Path)
		if cf.idx == nil {
			r.Log(fmt.Sprintf("skip %s: %v", name, cf.Err))
			continue
		}
		need, size, miss := plan(cf, g.Manifest)
		if miss == 0 {
			r.Log(fmt.Sprintf("%s: up to date (%d pipelines)", name, cf.Pipelines))
			continue
		}
		r.Log(fmt.Sprintf("%s: getting %d new pipelines (%.0f MB)", name, miss, float64(size)/1e6))
		added, err := mergeInto(cf, need, size, store, r)
		if err == ErrCancelled {
			r.Log(name + ": cancelled, file unchanged")
			return ErrCancelled
		}
		if err != nil {
			r.Log(fmt.Sprintf("%s: skipped, %v (your file was not changed)", name, err))
			continue
		}
		r.Log(fmt.Sprintf("%s: done, %d pipelines (+%d)", name, cf.Pipelines+added, added))
		cf.Pipelines += added
		cf.Missing, cf.Download = 0, 0
	}
	return nil
}

// mergeInto streams the original file into a temp file, appends the missing entries chunk by
// chunk, re-validates the result with a full scan, keeps the original as .bak and swaps in.
func mergeInto(cf *CacheFile, need []pack.Chunk, size int64, store *chunkStore, r Reporter) (int, error) {
	tmp := cf.Path + ".lichii-sync.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	fail := func(e error) (int, error) { out.Close(); os.Remove(tmp); return 0, e }
	in, err := os.Open(cf.Path)
	if err != nil {
		return fail(err)
	}
	_, err = io.Copy(out, io.LimitReader(in, cf.idx.Size))
	in.Close()
	if err != nil {
		return fail(err)
	}
	have := cf.idx.IDs
	added := 0
	var done int64
	r.Progress(0, size)
	for _, c := range need {
		if r.Cancelled() {
			return fail(ErrCancelled)
		}
		es, err := store.entries(c)
		if err != nil {
			return fail(err)
		}
		for _, e := range es {
			if !have[e.ID] {
				if _, err := out.Write(e.Data); err != nil {
					return fail(err)
				}
				have[e.ID] = true
				added++
			}
		}
		done += c.Size
		r.Progress(done, size)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	ix, err := cache.Scan(tmp)
	if err != nil || ix.Version != cf.Version || ix.Count != cf.idx.Count+added {
		os.Remove(tmp)
		if err == nil {
			err = fmt.Errorf("merged file did not check out")
		}
		return 0, fmt.Errorf("validation failed: %v", err)
	}
	bak := cf.Path + ".bak"
	os.Remove(bak)
	if err := os.Rename(cf.Path, bak); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, cf.Path); err != nil {
		return 0, err
	}
	cf.idx = ix
	return added, nil
}

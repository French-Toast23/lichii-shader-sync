// Package syncer finds local Lichii shader caches and merges in the published packs.
// Shared by the command-line tool and the GUI.
package syncer

import (
	"encoding/hex"
	"encoding/json"
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
	"sync"
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
	return getWithAccept(url, "")
}

func getWithAccept(url, accept string) ([]byte, int, error) {
	var lastCode int
	var lastErr error
	const maxRetries = 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 1000 * time.Millisecond)
		}
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("User-Agent", "lichii-sync")
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if attempt > 0 {
			req.Header.Set("Cache-Control", "no-cache")
			req.Header.Set("Pragma", "no-cache")
		}
		if strings.Contains(url, "api.github.com") {
			if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			} else if tok := os.Getenv("GH_TOKEN"); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		r, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		lastCode = r.StatusCode
		if r.StatusCode == 200 {
			b, err := ioutil.ReadAll(r.Body)
			r.Body.Close()
			return b, 200, err
		}
		r.Body.Close()
		lastErr = fmt.Errorf("%s: HTTP %d", url, r.StatusCode)
		if r.StatusCode == 504 && attempt >= 1 {
			return nil, r.StatusCode, lastErr
		}
		if r.StatusCode != 429 && r.StatusCode != 500 && r.StatusCode != 502 && r.StatusCode != 503 && r.StatusCode != 504 {
			return nil, r.StatusCode, lastErr
		}
	}
	return nil, lastCode, lastErr
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

// shaderRoot is the folder holding the per-title shader caches. Windows and portable installs
// keep it under the data folder, but on Linux Lichii follows the XDG spec and puts the cache
// somewhere else entirely: $XDG_CACHE_HOME/eden/shader, not <data>/cache/shader
// (src/common/fs/path_util.cpp: eden_path_cache = GetDataDirectory("XDG_CACHE_HOME") / EDEN_DIR,
// ShaderDir = eden_path_cache / SHADER_DIR).
func shaderRoot(data string) string {
	underData := filepath.Join(data, "cache", "shader")
	if runtime.GOOS == "windows" {
		return underData
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		cache = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	// A data folder with its own cache (portable install, or one given with -data) comes first,
	// so we never sync a different install than the one asked for; otherwise use the XDG cache.
	for _, r := range []string{underData, filepath.Join(cache, "eden", "shader")} {
		if fi, err := os.Stat(r); err == nil && fi.IsDir() {
			return r
		}
	}
	return underData
}

// getBaseURL returns the base URL for releases downloads. Can be overridden via LICHII_MIRROR.
func getBaseURL(repo string) string {
	if m := os.Getenv("LICHII_MIRROR"); m != "" {
		if !strings.HasSuffix(m, "/") {
			m += "/"
		}
		return m
	}
	return "https://github.com/" + repo + "/releases/download/"
}

// Scan lists every game cache folder and, for each, the local caches and the online pack.
func Scan(repo, data string, r Reporter) ([]*Game, error) {
	root := shaderRoot(data)
	dirs, err := ioutil.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("no shader cache folder at %s - boot a game once in Lichii first", root)
	}
	base := getBaseURL(repo)
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
		if err != nil && code != 404 {
			if r != nil {
				r.Log(fmt.Sprintf("%s: direct manifest download failed (%v), trying GitHub API...", g.TitleID, err))
			}
			var apiErr error
			mb, apiErr = fetchManifestAPI(repo, g.TitleID)
			if apiErr == nil {
				err = nil
				code = 200
			}
		}
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

var (
	manifestMu     sync.Mutex
	manifestAssets map[string]string // TITLEID.JSON -> api asset download url
)

func fetchManifestAPI(repo, titleID string) ([]byte, error) {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	if manifestAssets == nil {
		tagURL := "https://api.github.com/repos/" + repo + "/releases/tags/" + ManifestTag
		b, code, err := get(tagURL)
		if err != nil {
			return nil, fmt.Errorf("fetching manifests release (HTTP %d): %v", code, err)
		}
		var rel struct {
			Assets []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"assets"`
		}
		if err := json.Unmarshal(b, &rel); err != nil {
			return nil, err
		}
		manifestAssets = make(map[string]string)
		for _, a := range rel.Assets {
			manifestAssets[strings.ToUpper(a.Name)] = a.URL
		}
	}
	assetURL, ok := manifestAssets[strings.ToUpper(titleID)+".JSON"]
	if !ok {
		return nil, fmt.Errorf("no manifest asset for %s", titleID)
	}
	b, code, err := getWithAccept(assetURL, "application/octet-stream")
	if err != nil {
		return nil, fmt.Errorf("fetching manifest asset %s (HTTP %d): %v", titleID, code, err)
	}
	return b, nil
}

// chunkStore downloads each chunk once per run into a temp folder, so several cache files
// can reuse it without holding everything in memory.
type chunkStore struct {
	repo       string
	base       string
	dir        string
	r          Reporter
	mu         sync.Mutex
	directDown bool                         // true if releases/download has failed with 5xx
	tagAssets  map[string]map[string]string // tag -> filename -> api asset download url
}

func (s *chunkStore) entries(c pack.Chunk) ([]cache.Entry, error) {
	p := filepath.Join(s.dir, c.File)
	z, err := ioutil.ReadFile(p)
	if err == nil {
		if es, err := pack.Decode(z, c); err == nil {
			return es, nil
		}
		os.Remove(p)
	}

	s.mu.Lock()
	skipDirect := s.directDown
	s.mu.Unlock()

	var code int
	if !skipDirect {
		z, code, err = get(s.base + c.Tag + "/" + c.File)
		if err != nil && (code == 504 || code == 502 || code == 503) {
			s.mu.Lock()
			s.directDown = true
			s.mu.Unlock()
		}
	}
	if skipDirect || err != nil {
		if !skipDirect && s.r != nil {
			s.r.Log(fmt.Sprintf("%s: direct download failed (%v), switching to GitHub API...", c.File, err))
		}
		var apiErr error
		z, apiErr = s.fetchAPI(c)
		if apiErr != nil {
			if strings.Contains(apiErr.Error(), "403") && s.r != nil {
				s.r.Log("GitHub API rate limit reached (60/hr). Set GITHUB_TOKEN or retry in a few minutes.")
			}
			if err != nil {
				return nil, fmt.Errorf("%s: HTTP %d (API fallback: %v)", s.base+c.Tag+"/"+c.File, code, apiErr)
			}
			return nil, apiErr
		}
	}
	ioutil.WriteFile(p, z, 0o644)
	return pack.Decode(z, c)
}

func (s *chunkStore) fetchAPI(c pack.Chunk) ([]byte, error) {
	s.mu.Lock()
	if s.tagAssets == nil {
		s.tagAssets = make(map[string]map[string]string)
	}
	assets, ok := s.tagAssets[c.Tag]
	if !ok {
		tagURL := "https://api.github.com/repos/" + s.repo + "/releases/tags/" + c.Tag
		b, code, err := get(tagURL)
		if err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("release tag %s: HTTP %d: %v", c.Tag, code, err)
		}
		var rel struct {
			Assets []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"assets"`
		}
		if err := json.Unmarshal(b, &rel); err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("parsing release %s: %v", c.Tag, err)
		}
		assets = make(map[string]string)
		for _, a := range rel.Assets {
			assets[a.Name] = a.URL
		}
		s.tagAssets[c.Tag] = assets
	}
	assetURL, found := assets[c.File]
	s.mu.Unlock()
	if !found {
		return nil, fmt.Errorf("asset %s not found in release %s", c.File, c.Tag)
	}
	b, code, err := getWithAccept(assetURL, "application/octet-stream")
	if err != nil {
		return nil, fmt.Errorf("asset %s: HTTP %d: %v", c.File, code, err)
	}
	return b, nil
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
	// Use .chunks folder to preserve downloaded chunks across runs if a sync is interrupted
	dir := filepath.Join(filepath.Dir(g.Dir), ".chunks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	store := &chunkStore{
		repo: repo,
		base: getBaseURL(repo),
		dir:  dir,
		r:    r,
	}
	var syncErr error
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
			syncErr = err
			continue
		}
		r.Log(fmt.Sprintf("%s: done, %d pipelines (+%d)", name, cf.Pipelines+added, added))
		cf.Pipelines += added
		cf.Missing, cf.Download = 0, 0
	}
	if syncErr == nil {
		os.RemoveAll(dir)
	}
	return syncErr
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

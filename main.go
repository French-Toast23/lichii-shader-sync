// lichii-sync (command line): shares Lichii/Eden Vulkan shader caches through GitHub Releases,
// per game title ID. The Windows GUI lives in ./gui.
//
//	lichii-sync sync [-title ID] [-data DIR] [-launch lichii.exe] [-dry-run]
//	lichii-sync pack -title ID [-prev manifest.json] CACHE OUTDIR TAG   (maintainer)
//	lichii-sync info CACHE
//	lichii-sync export [-title ID] OUT.bin.gz   your shaders the online pack lacks (to share)
package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"lichii-sync/cache"
	"lichii-sync/pack"
	"lichii-sync/syncer"
)

type cli struct{ last int }

func (c *cli) Log(m string)    { fmt.Println(m) }
func (c *cli) Cancelled() bool { return false }
func (c *cli) Progress(done, total int64) {
	if total <= 0 {
		return
	}
	p := int(done * 100 / total)
	if p/10 != c.last/10 || p == 100 {
		fmt.Printf("  %d%%\n", p)
	}
	c.last = p
}

func main() {
	if len(os.Args) < 2 {
		if err := runSync(syncer.Repo, syncer.DefaultData(), "", "", false); err != nil {
			fmt.Println("\nERROR:", err)
		}
		fmt.Print("\nPress Enter to close...")
		bufio.NewReader(os.Stdin).ReadString('\n')
		return
	}
	var err error
	switch os.Args[1] {
	case "sync":
		fs := flag.NewFlagSet("sync", flag.ExitOnError)
		repo := fs.String("repo", syncer.Repo, "GitHub owner/repo with the packs")
		data := fs.String("data", syncer.DefaultData(), "Lichii data folder (holds cache\\shader)")
		title := fs.String("title", "", "only this title ID (default: every game with a pack)")
		launch := fs.String("launch", "", "lichii.exe to start after syncing")
		dry := fs.Bool("dry-run", false, "only report what would be downloaded")
		fs.Parse(os.Args[2:])
		err = runSync(*repo, *data, *title, *launch, *dry)
	case "pack":
		fs := flag.NewFlagSet("pack", flag.ExitOnError)
		prev := fs.String("prev", "", "current manifest.json (reuse its chunks)")
		per := fs.Int("per-chunk", 1500, "entries per chunk")
		title := fs.String("title", "", "title ID (required)")
		fs.Parse(os.Args[2:])
		a := fs.Args()
		if len(a) != 3 || !syncer.TitleRe.MatchString(*title) {
			err = fmt.Errorf("usage: pack -title TITLEID [-prev manifest.json] CACHE OUTDIR TAG")
			break
		}
		err = runPack(a[0], a[1], a[2], *prev, strings.ToUpper(*title), *per)
	case "export":
		fs := flag.NewFlagSet("export", flag.ExitOnError)
		data := fs.String("data", syncer.DefaultData(), "Lichii data folder (holds cache\\shader)")
		title := fs.String("title", "0400486024DEC000", "title ID")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			err = fmt.Errorf("usage: export [-title TITLEID] OUT.bin.gz")
			break
		}
		err = runExport(*data, *title, fs.Arg(0))
	case "info":
		var f *cache.File
		if f, err = loadCache(os.Args[2]); err == nil {
			fmt.Printf("%s: version %d, %d entries\n", filepath.Base(os.Args[2]), f.Version, len(f.Entries))
		}
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func runSync(repo, data, only, launch string, dry bool) error {
	if !dry && filepath.Clean(data) == filepath.Clean(syncer.DefaultData()) && syncer.LichiiRunning() {
		return fmt.Errorf("Lichii is running - close it first (it writes to the cache while open)")
	}
	r := &cli{}
	games, err := syncer.Scan(repo, data, r)
	if err != nil {
		return err
	}
	n := 0
	for _, g := range games {
		if only != "" && !strings.EqualFold(only, g.TitleID) {
			continue
		}
		if g.Manifest == nil {
			if only != "" {
				fmt.Printf("%s: no shader pack online\n", g.TitleID)
			}
			continue
		}
		n++
		fmt.Printf("== %s: %d pipelines online\n", g.TitleID, g.Manifest.Total)
		if len(g.Files) == 0 {
			fmt.Println("  no cache file yet - boot the game once in your Lichii build, then run this again")
			continue
		}
		if dry {
			fmt.Printf("  would add %d pipelines (%.0f MB)\n", g.Missing, float64(g.Download)/1e6)
			continue
		}
		if err := syncer.SyncGame(repo, g, r); err != nil {
			return fmt.Errorf("%s: %v", g.TitleID, err)
		}
	}
	if n == 0 {
		fmt.Println("No games with a shader pack online were found.")
	}
	if launch != "" {
		fmt.Println("starting", launch)
		return exec.Command(launch).Start()
	}
	return nil
}

func runExport(data, title, out string) error {
	games, err := syncer.Scan(syncer.Repo, data, &cli{})
	if err != nil {
		return err
	}
	for _, g := range games {
		if strings.EqualFold(g.TitleID, title) {
			n, err := syncer.ExportExtras(g, out)
			if err == nil {
				fmt.Printf("exported %d pipelines the online pack doesn't have -> %s\n", n, out)
			}
			return err
		}
	}
	return fmt.Errorf("no cache folder for %s", title)
}

// loadCache reads a cache file, gunzipping it first if it ends in .gz (shared exports).
func loadCache(path string) (*cache.File, error) {
	if !strings.HasSuffix(strings.ToLower(path), ".gz") {
		return cache.Load(path)
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	return cache.Parse(b)
}

func runPack(cachePath, outDir, tag, prevPath, title string, per int) error {
	f, err := loadCache(cachePath)
	if err != nil {
		return err
	}
	var prev *pack.Manifest
	if prevPath != "" {
		if prev, err = pack.LoadManifest(prevPath); err != nil {
			return err
		}
		if prev.TitleID != title {
			return fmt.Errorf("previous manifest is for %s", prev.TitleID)
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	m, added, err := pack.Build(f, title, tag, outDir, prev, per)
	if err != nil {
		return err
	}
	if err := m.Save(filepath.Join(outDir, title+".json")); err != nil {
		return err
	}
	n := 0
	for _, c := range added {
		n += c.Entries
	}
	fmt.Printf("%d entries read, %d new in %d new chunks; manifest %s.json lists %d entries in %d chunks\n",
		len(f.Entries), n, len(added), title, m.Total, len(m.Chunks))
	return nil
}

// Lichii Shader Sync - Windows GUI. One button: update shaders for every game with a pack online.
package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"lichii-sync/syncer"
)

const iconID = 1 // RT_GROUP_ICON embedded in rsrc_windows_amd64.syso

var (
	pink     = walk.RGB(0xD9, 0x2B, 0x5A)
	pinkDark = walk.RGB(0xA8, 0x1D, 0x45)
	green    = walk.RGB(0x2E, 0xA0, 0x43)
	grey     = walk.RGB(0x6B, 0x6B, 0x75)
)

// ---- settings ----

type settings struct {
	LichiiExe string `json:"lichii_exe"`
	DataDir   string `json:"data_dir"`
}

func settingsPath() string {
	return filepath.Join(os.Getenv("APPDATA"), "lichii-sync", "settings.json")
}

func loadSettings() settings {
	s := settings{}
	if b, err := ioutil.ReadFile(settingsPath()); err == nil {
		json.Unmarshal(b, &s)
	}
	if s.DataDir == "" {
		s.DataDir = syncer.DefaultData()
	}
	return s
}

func (s settings) save() {
	os.MkdirAll(filepath.Dir(settingsPath()), 0o755)
	b, _ := json.MarshalIndent(s, "", " ")
	ioutil.WriteFile(settingsPath(), b, 0o644)
}

// ---- games table ----

type row struct {
	name, have, online, status string
	pending                    bool
}

type gameModel struct {
	walk.TableModelBase
	rows []row
}

func (m *gameModel) RowCount() int { return len(m.rows) }
func (m *gameModel) Value(r, c int) interface{} {
	x := m.rows[r]
	return [...]string{x.name, x.have, x.online, x.status}[c]
}

// ---- app ----

type app struct {
	mw        *walk.MainWindow
	table     *walk.TableView
	model     *gameModel
	status    *walk.Label
	bar       *walk.ProgressBar
	log       *walk.TextEdit
	syncBtn   *walk.PushButton
	launchBtn *walk.PushButton
	cfg       settings
	found     []*syncer.Game
	busy      bool
}

func (a *app) Log(m string) { a.mw.Synchronize(func() { a.log.AppendText(m + "\r\n") }) }
func (a *app) Progress(done, total int64) {
	a.mw.Synchronize(func() {
		if total > 0 {
			a.bar.SetValue(int(done * 1000 / total))
			a.setStatus(fmt.Sprintf("Downloading shaders...  %.0f / %.0f MB", float64(done)/1e6, float64(total)/1e6), grey)
		}
	})
}

func (a *app) setStatus(s string, c walk.Color) {
	a.status.SetTextColor(c)
	a.status.SetText(s)
}

func (a *app) setBusy(b bool) {
	a.busy = b
	a.syncBtn.SetEnabled(!b)
	a.launchBtn.SetEnabled(!b)
}

func gameName(g *syncer.Game) string {
	if g.Manifest != nil && g.Manifest.Name != "" {
		return g.Manifest.Name
	}
	return g.TitleID
}

func thousands(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func (a *app) refresh() {
	a.setBusy(true)
	a.setStatus("Checking your games...", grey)
	go func() {
		games, err := syncer.Scan(syncer.Repo, a.cfg.DataDir, a)
		a.mw.Synchronize(func() {
			a.setBusy(false)
			if err != nil {
				a.setStatus(err.Error(), pinkDark)
				return
			}
			a.found = games
			var rows []row
			todo := 0
			for _, g := range games {
				if g.Manifest == nil {
					continue
				}
				online := thousands(g.Manifest.Total)
				if len(g.Files) == 0 {
					rows = append(rows, row{name: gameName(g), have: "–", online: online, status: "Play it once in Lichii first"})
					continue
				}
				// one row per Lichii build: each build keeps its own cache file
				for _, f := range g.Files {
					r := row{name: gameName(g) + "  ·  " + f.Label(), have: thousands(f.Pipelines), online: online}
					switch {
					case f.Err != nil:
						r.have, r.status = "–", "Unreadable cache, skipped"
					case f.Missing > 0:
						r.status = fmt.Sprintf("⬇ %s new  (%.0f MB)", thousands(f.Missing), float64(f.Download)/1e6)
						r.pending = true
						todo++
					case f.Extra > 0:
						r.status = fmt.Sprintf("✔ Up to date  (+%s of your own)", thousands(f.Extra))
					default:
						r.status = "✔ Up to date"
					}
					rows = append(rows, r)
				}
			}
			a.model.rows = rows
			a.model.PublishRowsReset()
			switch {
			case len(rows) == 0:
				a.setStatus("No games with shared shaders yet. Play a game once in Lichii, then check again.", grey)
				a.syncBtn.SetText("Check again")
			case todo > 0:
				a.setStatus(fmt.Sprintf("New shaders are ready for %d of your Lichii builds.", todo), pinkDark)
				a.syncBtn.SetText("⬇  Update shaders")
			default:
				a.bar.SetValue(1000)
				a.setStatus("✔  All your games are up to date.", green)
				a.syncBtn.SetText("⟳  Check again")
			}
		})
	}()
}

func (a *app) doSync() {
	if a.busy {
		return
	}
	pending := false
	for _, g := range a.found {
		if g.Manifest != nil && g.Missing > 0 && len(g.Files) > 0 {
			pending = true
		}
	}
	if !pending {
		a.refresh()
		return
	}
	if filepath.Clean(a.cfg.DataDir) == filepath.Clean(syncer.DefaultData()) && syncer.LichiiRunning() {
		walk.MsgBox(a.mw, "Close Lichii first", "Lichii is running. Close it, then click Update shaders again.", walk.MsgBoxIconWarning)
		return
	}
	a.setBusy(true)
	a.bar.SetValue(0)
	a.setStatus("Starting...", grey)
	go func() {
		var err error
		for _, g := range a.found {
			if g.Manifest == nil || g.Missing == 0 {
				continue
			}
			a.Log("== " + gameName(g))
			if err = syncer.SyncGame(syncer.Repo, g, a); err != nil {
				break
			}
		}
		a.mw.Synchronize(func() {
			a.setBusy(false)
			if err != nil {
				a.Log("ERROR: " + err.Error())
				a.setStatus("Something went wrong - your shaders were not changed. See details.", pinkDark)
				return
			}
			a.refresh()
		})
	}()
}

func (a *app) doLaunch() {
	exe := a.cfg.LichiiExe
	if _, err := os.Stat(exe); exe == "" || err != nil {
		dlg := walk.FileDialog{Title: "Where is lichii.exe?", Filter: "lichii.exe|lichii.exe|Programs (*.exe)|*.exe"}
		if ok, _ := dlg.ShowOpen(a.mw); !ok {
			return
		}
		exe = dlg.FilePath
		a.cfg.LichiiExe = exe
		a.cfg.save()
	}
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		walk.MsgBox(a.mw, "Could not start Lichii", err.Error(), walk.MsgBoxIconError)
		return
	}
	a.mw.Close()
}

func main() {
	a := &app{cfg: loadSettings(), model: &gameModel{}}
	appIcon, _ := walk.NewIconFromResourceId(iconID)
	bigIcon, _ := walk.NewIconFromResourceIdWithSize(iconID, walk.Size{Width: 64, Height: 64})

	err := MainWindow{
		AssignTo: &a.mw,
		Title:    "Lichii Shader Sync",
		Icon:     appIcon,
		Size:     Size{Width: 760, Height: 560},
		MinSize:  Size{Width: 520, Height: 420},
		Layout:   VBox{MarginsZero: true, SpacingZero: true},
		Children: []Widget{
			// header band
			Composite{
				Background: SolidColorBrush{Color: pink},
				MaxSize:    Size{Height: 92},
				Layout:     HBox{Margins: Margins{Left: 18, Top: 14, Right: 18, Bottom: 14}, Spacing: 14},
				Children: []Widget{
					ImageView{Image: bigIcon, MinSize: Size{Width: 64, Height: 64}, MaxSize: Size{Width: 64, Height: 64}, Mode: ImageViewModeIdeal},
					Composite{
						Layout: VBox{MarginsZero: true, Spacing: 2},
						Children: []Widget{
							VSpacer{},
							Label{Text: "Lichii Shader Sync", Font: Font{Family: "Segoe UI Semibold", PointSize: 17}, TextColor: walk.RGB(255, 255, 255)},
							Label{Text: "Shaders other players already built - fewer stutters on first play.", Font: Font{Family: "Segoe UI", PointSize: 9}, TextColor: walk.RGB(0xFF, 0xE3, 0xEA)},
							VSpacer{},
						},
					},
					HSpacer{},
				},
			},
			// body
			Composite{
				Layout: VBox{Margins: Margins{Left: 18, Top: 14, Right: 18, Bottom: 16}, Spacing: 10},
				Children: []Widget{
					Label{Text: "YOUR GAMES", Font: Font{Family: "Segoe UI Semibold", PointSize: 8}, TextColor: grey},
					TableView{
						AssignTo:            &a.table,
						Model:               a.model,
						AlternatingRowBG:    true,
						ColumnsOrderable:    false,
						LastColumnStretched: true,
						Font:                Font{Family: "Segoe UI", PointSize: 10},
						MinSize:             Size{Height: 120},
						Columns: []TableViewColumn{
							{Title: "Game  ·  Lichii build", Width: 330},
							{Title: "Your shaders", Width: 95, Alignment: AlignFar},
							{Title: "Online", Width: 80, Alignment: AlignFar},
							{Title: "Status"},
						},
					},
					Label{AssignTo: &a.status, Text: "Starting...", Font: Font{Family: "Segoe UI", PointSize: 10}},
					ProgressBar{AssignTo: &a.bar, MaxValue: 1000, MinSize: Size{Height: 14}},
					Composite{
						Layout: HBox{MarginsZero: true, Spacing: 10},
						Children: []Widget{
							PushButton{AssignTo: &a.syncBtn, Text: "⬇  Update shaders", Font: Font{Family: "Segoe UI Semibold", PointSize: 11},
								MinSize: Size{Width: 220, Height: 44}, OnClicked: a.doSync},
							PushButton{AssignTo: &a.launchBtn, Text: "▶  Launch Lichii", Font: Font{Family: "Segoe UI", PointSize: 11},
								MinSize: Size{Width: 160, Height: 44}, OnClicked: a.doLaunch},
							HSpacer{},
						},
					},
					Label{Text: "DETAILS", Font: Font{Family: "Segoe UI Semibold", PointSize: 8}, TextColor: grey},
					TextEdit{AssignTo: &a.log, ReadOnly: true, VScroll: true, MinSize: Size{Height: 70},
						Font: Font{Family: "Consolas", PointSize: 9}},
				},
			},
		},
	}.Create()
	if err != nil {
		walk.MsgBox(nil, "Lichii Shader Sync", err.Error(), walk.MsgBoxIconError)
		return
	}
	a.refresh()
	a.mw.Run()
}

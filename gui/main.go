// Lichii Shader Sync - Windows GUI. One button: update shaders for every game with a pack online.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"

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

// Settings live in %APPDATA%\lichii-sync\settings.json, or next to the app when %APPDATA% can't be
// written (e.g. a locked-down C: drive). A copy next to the app wins when both exist.
func settingsPaths() (local, appdata string) {
	if exe, err := os.Executable(); err == nil {
		local = filepath.Join(filepath.Dir(exe), "lichii-sync-settings.json")
	}
	return local, filepath.Join(os.Getenv("APPDATA"), "lichii-sync", "settings.json")
}

func loadSettings() settings {
	s := settings{}
	local, appdata := settingsPaths()
	for _, p := range []string{local, appdata} {
		if b, err := ioutil.ReadFile(p); err == nil {
			b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) // tolerate a UTF-8 BOM (e.g. saved by Notepad)
			json.Unmarshal(b, &s)
			break
		}
	}
	if s.DataDir == "" {
		s.DataDir = detectData(s.LichiiExe)
	}
	return s
}

// detectData prefers a portable Lichii ("user" folder next to lichii.exe) over %APPDATA%\eden.
func detectData(lichiiExe string) string {
	if lichiiExe != "" {
		p := filepath.Join(filepath.Dir(lichiiExe), "user")
		if st, err := os.Stat(filepath.Join(p, "cache", "shader")); err == nil && st.IsDir() {
			return p
		}
	}
	return syncer.DefaultData()
}

func (s settings) save() error {
	b, _ := json.MarshalIndent(s, "", " ")
	local, appdata := settingsPaths()
	if _, err := os.Stat(local); err == nil { // already using the copy next to the app
		return ioutil.WriteFile(local, b, 0o644)
	}
	os.MkdirAll(filepath.Dir(appdata), 0o755)
	if err := ioutil.WriteFile(appdata, b, 0o644); err != nil {
		return ioutil.WriteFile(local, b, 0o644)
	}
	return nil
}

// ---- games table ----

type row struct {
	name, have, online, status string
	pending                    bool // has new shaders to download
	checked                    bool // ticked by the user
	game                       *syncer.Game
	file                       *syncer.CacheFile
}

type gameModel struct {
	walk.TableModelBase
	rows     []row
	onChange func()
}

func (m *gameModel) RowCount() int { return len(m.rows) }
func (m *gameModel) Value(r, c int) interface{} {
	x := m.rows[r]
	return [...]string{x.name, x.have, x.online, x.status}[c]
}

// Checked / SetChecked make the TableView show a checkbox per row (walk.ItemChecker).
// Only rows with new shaders can be ticked.
func (m *gameModel) Checked(r int) bool { return m.rows[r].checked }
func (m *gameModel) SetChecked(r int, checked bool) error {
	m.rows[r].checked = checked && m.rows[r].pending
	if !m.rows[r].pending {
		m.PublishRowChanged(r)
	}
	if m.onChange != nil {
		m.onChange()
	}
	return nil
}

// selected returns the ticked rows that have something to download.
func (m *gameModel) selected() []row {
	var out []row
	for _, r := range m.rows {
		if r.checked && r.pending {
			out = append(out, r)
		}
	}
	return out
}

// ---- app ----

type app struct {
	mw          *walk.MainWindow
	table       *walk.TableView
	model       *gameModel
	status      *walk.Label
	bar         *walk.ProgressBar
	log         *walk.TextEdit
	syncBtn     *walk.PushButton
	launchBtn   *walk.PushButton
	updLink     *walk.LinkLabel
	detailsLink *walk.LinkLabel
	// menu actions
	dataAct, exeAct, shareAct, undoAct, bakAct *walk.Action
	cfg                                        settings
	found                                      []*syncer.Game
	busy                                       bool
	syncing                                    bool  // a download is running (the main button is Cancel)
	cancel                                     int32 // set to 1 to stop the running sync
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
	for _, act := range []*walk.Action{a.dataAct, a.exeAct, a.shareAct, a.undoAct, a.bakAct} {
		act.SetEnabled(!b)
	}
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
				a.setStatus(err.Error()+"  (Settings → Lichii data folder)", pinkDark)
				return
			}
			a.found = games
			// remember rows the user unticked, so a refresh doesn't tick them again
			unticked := map[string]bool{}
			for _, r := range a.model.rows {
				if r.pending && !r.checked && r.file != nil {
					unticked[r.file.Path] = true
				}
			}
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
					r := row{name: gameName(g) + "  ·  " + f.Label(), have: thousands(f.Pipelines), online: online, game: g, file: f}
					switch {
					case f.Err != nil:
						r.have, r.status = "–", "Unreadable cache, skipped"
					case f.Missing > 0:
						r.status = fmt.Sprintf("⬇ %s new  (%.0f MB)", thousands(f.Missing), float64(f.Download)/1e6)
						r.pending, r.checked = true, !unticked[f.Path]
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
			for i := range rows { // make the checkboxes redraw from the model
				a.model.PublishRowChanged(i)
			}
			switch {
			case len(rows) == 0:
				a.setStatus("No games with shared shaders yet. Play a game once in Lichii, then check again.", grey)
				a.syncBtn.SetText("Check again")
			case todo > 0:
				a.setStatus(fmt.Sprintf("New shaders are ready for %d of your Lichii builds. Untick any you don't want.", todo), pinkDark)
				a.updateButton()
			default:
				a.bar.SetValue(1000)
				a.setStatus("✔  All your games are up to date.", green)
				a.syncBtn.SetText("⟳  Check again")
			}
			a.refreshExtras()
		})
	}()
}

// updateButton shows how many ticked rows will be updated.
func (a *app) updateButton() {
	pending := 0
	for _, r := range a.model.rows {
		if r.pending {
			pending++
		}
	}
	if pending == 0 {
		a.syncBtn.SetText("⟳  Check again")
		return
	}
	n := len(a.model.selected())
	a.syncBtn.SetText(fmt.Sprintf("⬇  Update selected (%d)", n))
	a.syncBtn.SetEnabled(n > 0 && !a.busy)
}

func (a *app) doSync() {
	if a.syncing { // the main button is Cancel while downloading
		atomic.StoreInt32(&a.cancel, 1)
		a.syncBtn.SetText("Cancelling...")
		a.syncBtn.SetEnabled(false)
		return
	}
	if a.busy {
		return
	}
	pending := false
	for _, r := range a.model.rows {
		if r.pending {
			pending = true
		}
	}
	if !pending {
		a.refresh()
		return
	}
	sel := a.model.selected()
	if len(sel) == 0 {
		a.setStatus("Tick at least one row to update.", pinkDark)
		return
	}
	if filepath.Clean(a.cfg.DataDir) == filepath.Clean(syncer.DefaultData()) && syncer.LichiiRunning() {
		walk.MsgBox(a.mw, "Close Lichii first", "Lichii is running. Close it, then click Update shaders again.", walk.MsgBoxIconWarning)
		return
	}
	a.setBusy(true)
	a.syncing = true
	atomic.StoreInt32(&a.cancel, 0)
	a.syncBtn.SetText("✕  Cancel")
	a.syncBtn.SetEnabled(true)
	a.bar.SetValue(0)
	a.setStatus("Starting...", grey)
	go func() {
		// group the ticked cache files by game
		var order []*syncer.Game
		files := map[*syncer.Game][]*syncer.CacheFile{}
		for _, r := range sel {
			if _, ok := files[r.game]; !ok {
				order = append(order, r.game)
			}
			files[r.game] = append(files[r.game], r.file)
		}
		var err error
		for _, g := range order {
			a.Log("== " + gameName(g))
			if err = syncer.SyncFiles(syncer.Repo, g, files[g], a); err != nil {
				break
			}
		}
		a.mw.Synchronize(func() {
			a.syncing = false
			a.setBusy(false)
			if err == syncer.ErrCancelled {
				a.refresh()
				a.setStatus("Cancelled - the cache being updated was left unchanged.", pinkDark)
				return
			}
			if err != nil {
				a.Log("ERROR: " + err.Error())
				a.showDetails(true)
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
		a.saveSettings()
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
	a.model.onChange = func() {
		if !a.busy {
			a.updateButton()
		}
	}
	appIcon, _ := walk.NewIconFromResourceId(iconID)
	bigIcon, _ := walk.NewIconFromResourceIdWithSize(iconID, walk.Size{Width: 64, Height: 64})

	err := MainWindow{
		AssignTo: &a.mw,
		Title:    "Lichii Shader Sync",
		Icon:     appIcon,
		Size:     Size{Width: 780, Height: 560},
		MinSize:  Size{Width: 560, Height: 440},
		Layout:   VBox{MarginsZero: true, SpacingZero: true},
		MenuItems: []MenuItem{
			Menu{
				Text: "&Settings",
				Items: []MenuItem{
					Action{AssignTo: &a.dataAct, Text: "Lichii data folder…", OnTriggered: a.doPickData},
					Action{AssignTo: &a.exeAct, Text: "Lichii program…", OnTriggered: a.doResetExe},
					Action{Text: "Open shader folder", OnTriggered: a.openShaderFolder},
					Separator{},
					Action{Text: "E&xit", OnTriggered: func() { a.mw.Close() }},
				},
			},
			Menu{
				Text: "S&haders",
				Items: []MenuItem{
					Action{AssignTo: &a.shareAct, Text: "Share my shaders…", Enabled: false, OnTriggered: a.doShare},
					Separator{},
					Action{AssignTo: &a.undoAct, Text: "Undo last update", Enabled: false, OnTriggered: a.doUndo},
					Action{AssignTo: &a.bakAct, Text: "Delete backups", Enabled: false, OnTriggered: a.doDeleteBackups},
				},
			},
			Menu{
				Text: "&Help",
				Items: []MenuItem{
					Action{Text: "Check for updates", OnTriggered: a.manualUpdateCheck},
					Action{Text: "Project page", OnTriggered: func() { openURL("https://github.com/" + syncer.Repo) }},
					Separator{},
					Action{Text: "About", OnTriggered: a.about},
				},
			},
		},
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
					LinkLabel{AssignTo: &a.updLink, Visible: false, Font: Font{Family: "Segoe UI Semibold", PointSize: 10},
						OnLinkActivated: func(l *walk.LinkLabelLink) { openURL(l.URL()) }},
					Label{Text: "YOUR GAMES", Font: Font{Family: "Segoe UI Semibold", PointSize: 8}, TextColor: grey},
					TableView{
						AssignTo:            &a.table,
						Model:               a.model,
						CheckBoxes:          true,
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
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							HSpacer{},
							LinkLabel{AssignTo: &a.detailsLink, Text: `<a href="toggle">▸ Show details</a>`,
								OnLinkActivated: func(*walk.LinkLabelLink) { a.showDetails(!a.log.Visible()) }},
						},
					},
					TextEdit{AssignTo: &a.log, ReadOnly: true, VScroll: true, Visible: false, MinSize: Size{Height: 90},
						Font: Font{Family: "Consolas", PointSize: 9}},
					VSpacer{},
				},
			},
		},
	}.Create()
	if err != nil {
		walk.MsgBox(nil, "Lichii Shader Sync", err.Error(), walk.MsgBoxIconError)
		return
	}
	a.showDataDir()
	a.refresh()
	a.checkForUpdate()
	a.mw.Run()
}

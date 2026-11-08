package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"

	"github.com/lxn/walk"

	"lichii-sync/syncer"
)

// Cancelled implements syncer.Reporter: the sync loop checks it between chunks.
func (a *app) Cancelled() bool { return atomic.LoadInt32(&a.cancel) == 1 }

func openURL(u string) { exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start() }

func gb(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

// refreshExtras updates the Shaders menu (Share / Undo / Delete backups) after a scan.
func (a *app) refreshExtras() {
	extra := 0
	for _, g := range a.found {
		for _, f := range g.Files {
			if f.Extra > extra {
				extra = f.Extra
			}
		}
	}
	a.shareAct.SetEnabled(extra > 0 && !a.busy)
	if extra > 0 {
		a.shareAct.SetText(fmt.Sprintf("Share my shaders (%s)…", thousands(extra)))
	} else {
		a.shareAct.SetText("Share my shaders…  (nothing new to share)")
	}
	baks := syncer.Backups(a.cfg.DataDir)
	var size int64
	for _, b := range baks {
		size += b.Size
	}
	a.undoAct.SetEnabled(len(baks) > 0 && !a.busy)
	a.bakAct.SetEnabled(len(baks) > 0 && !a.busy)
	if len(baks) > 0 {
		a.bakAct.SetText("Delete backups (" + gb(size) + ")…")
		a.undoAct.SetText("Undo last update…")
	} else {
		a.bakAct.SetText("Delete backups  (none)")
		a.undoAct.SetText("Undo last update  (nothing to undo)")
	}
}

// showDetails expands or collapses the log under the buttons.
func (a *app) showDetails(on bool) {
	a.log.SetVisible(on)
	if on {
		a.detailsLink.SetText(`<a href="toggle">▾ Hide details</a>`)
	} else {
		a.detailsLink.SetText(`<a href="toggle">▸ Show details</a>`)
	}
}

func (a *app) openShaderFolder() {
	p := filepath.Join(a.cfg.DataDir, "cache", "shader")
	if _, err := os.Stat(p); err != nil {
		p = a.cfg.DataDir
	}
	exec.Command("explorer", p).Start()
}

func (a *app) manualUpdateCheck() {
	tag, page := syncer.UpdateAvailable(syncer.Repo)
	if tag == "" {
		walk.MsgBox(a.mw, "Check for updates", "You have the latest version ("+syncer.Version+").", walk.MsgBoxIconInformation)
		return
	}
	if walk.MsgBox(a.mw, "Update available", "Version "+tag+" is available (you have "+syncer.Version+").\n\nOpen the download page?",
		walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) == walk.DlgCmdYes {
		openURL(page)
	}
}

func (a *app) about() {
	walk.MsgBox(a.mw, "About Lichii Shader Sync",
		"Lichii Shader Sync "+syncer.Version+"\n\nDownloads shared Vulkan shader caches into Lichii / Eden.\n\n"+
			"Unofficial companion tool - not affiliated with Lichii or Eden.\ngithub.com/"+syncer.Repo,
		walk.MsgBoxIconInformation)
}

// doShare exports the shaders the user has that the online pack lacks, for posting in Discord.
func (a *app) doShare() {
	var g *syncer.Game
	for _, x := range a.found {
		for _, f := range x.Files {
			if f.Extra > 0 {
				g = x
			}
		}
	}
	if g == nil {
		return
	}
	extra := 0
	for _, f := range g.Files {
		if f.Extra > extra {
			extra = f.Extra
		}
	}
	dlg := walk.FileDialog{
		Title:    "Save your shaders",
		Filter:   "Shader export (*.bin.gz)|*.bin.gz",
		FilePath: filepath.Join(os.Getenv("USERPROFILE"), "Desktop", fmt.Sprintf("%s-my-shaders-%d.bin.gz", g.TitleID, extra)),
	}
	if ok, _ := dlg.ShowSave(a.mw); !ok {
		return
	}
	out := dlg.FilePath
	a.setBusy(true)
	a.setStatus("Collecting your shaders...", grey)
	go func() {
		n, err := syncer.ExportExtras(g, out)
		a.mw.Synchronize(func() {
			a.setBusy(false)
			a.refreshExtras()
			if err != nil {
				a.Log("ERROR: " + err.Error())
				a.setStatus("Export failed - see details.", pinkDark)
				return
			}
			a.Log(fmt.Sprintf("exported %d shaders -> %s", n, out))
			a.setStatus(fmt.Sprintf("✔  Saved %s shaders. Post the file so they can be added to the pack.", thousands(n)), green)
			exec.Command("explorer", "/select,", out).Start()
		})
	}()
}

func (a *app) lichiiClosed() bool {
	if filepath.Clean(a.cfg.DataDir) == filepath.Clean(syncer.DefaultData()) && syncer.LichiiRunning() {
		walk.MsgBox(a.mw, "Close Lichii first", "Lichii is running. Close it, then try again.", walk.MsgBoxIconWarning)
		return false
	}
	return true
}

// doUndo swaps every cache with its backup (restoring the version before the last update).
func (a *app) doUndo() {
	baks := syncer.Backups(a.cfg.DataDir)
	if len(baks) == 0 || !a.lichiiClosed() {
		return
	}
	msg := fmt.Sprintf("Restore the previous version of %d cache file(s)?\n\nThe current files become the backups, so you can redo it the same way.", len(baks))
	if walk.MsgBox(a.mw, "Undo last update", msg, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		return
	}
	for _, b := range baks {
		if err := syncer.Undo(b); err != nil {
			a.Log("ERROR: " + err.Error())
		} else {
			a.Log("restored " + filepath.Base(b.Current))
		}
	}
	a.refresh()
}

// doDeleteBackups removes the .bak copies after confirmation.
func (a *app) doDeleteBackups() {
	baks := syncer.Backups(a.cfg.DataDir)
	if len(baks) == 0 {
		return
	}
	var size int64
	for _, b := range baks {
		size += b.Size
	}
	msg := fmt.Sprintf("Delete %d backup file(s) (%s)?\n\nYou won't be able to undo the last update afterwards.", len(baks), gb(size))
	if walk.MsgBox(a.mw, "Delete backups", msg, walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		return
	}
	freed, err := syncer.DeleteBackups(baks)
	if err != nil {
		a.Log("ERROR: " + err.Error())
	}
	a.setStatus("✔  Freed "+gb(freed)+".", green)
	a.refreshExtras()
}

// checkForUpdate shows a link when a newer release of this app exists.
func (a *app) checkForUpdate() {
	go func() {
		tag, page := syncer.UpdateAvailable(syncer.Repo)
		if tag == "" {
			return
		}
		a.mw.Synchronize(func() {
			a.updLink.SetText(fmt.Sprintf(`New version %s is available - <a href="%s">download it</a>`, tag, page))
			a.updLink.SetVisible(true)
		})
	}()
}

// ---- folders ----

// showDataDir puts the folder in use into the Settings menu; it only appears on the main screen
// (via the status line) when the folder has no shader cache.
func (a *app) showDataDir() {
	a.dataAct.SetText("Lichii data folder…  (" + a.cfg.DataDir + ")")
	exe := "not set"
	if a.cfg.LichiiExe != "" {
		exe = a.cfg.LichiiExe
	}
	a.exeAct.SetText("Lichii program…  (" + exe + ")")
}

// doPickData lets the user point the app at their Lichii data folder (the one holding cache\shader).
func (a *app) doPickData() {
	dlg := walk.FileDialog{Title: "Pick your Lichii data folder (the one that contains cache\\shader)", InitialDirPath: a.cfg.DataDir}
	if ok, _ := dlg.ShowBrowseFolder(a.mw); !ok {
		return
	}
	dir := dlg.FilePath
	// accept the shader folder itself or a game folder too, and walk up to the data folder
	for i := 0; i < 3; i++ {
		if st, err := os.Stat(filepath.Join(dir, "cache", "shader")); err == nil && st.IsDir() {
			break
		}
		dir = filepath.Dir(dir)
	}
	if st, err := os.Stat(filepath.Join(dir, "cache", "shader")); err != nil || !st.IsDir() {
		walk.MsgBox(a.mw, "No shader cache there",
			"That folder doesn't contain cache\\shader.\n\nPick Lichii's data folder: %APPDATA%\\eden for a normal install, or the \"user\" folder next to lichii.exe for a portable one.",
			walk.MsgBoxIconWarning)
		return
	}
	a.cfg.DataDir = dir
	a.saveSettings()
	a.showDataDir()
	a.refresh()
}

// doResetExe forgets the remembered lichii.exe and asks for it again.
func (a *app) doResetExe() {
	dlg := walk.FileDialog{Title: "Where is lichii.exe?", Filter: "lichii.exe|lichii.exe|Programs (*.exe)|*.exe"}
	if a.cfg.LichiiExe != "" {
		dlg.InitialDirPath = filepath.Dir(a.cfg.LichiiExe)
	}
	if ok, _ := dlg.ShowOpen(a.mw); !ok {
		return
	}
	a.cfg.LichiiExe = dlg.FilePath
	// a portable Lichii keeps its data next to the exe: switch to it automatically
	if p := detectData(a.cfg.LichiiExe); p != syncer.DefaultData() && p != a.cfg.DataDir {
		a.cfg.DataDir = p
		a.Log("using portable Lichii data folder " + p)
		a.showDataDir()
		a.refresh()
	}
	a.saveSettings()
	a.showDataDir()
	a.Log("Lichii: " + a.cfg.LichiiExe)
}

func (a *app) saveSettings() {
	if err := a.cfg.save(); err != nil {
		a.Log("could not save settings: " + err.Error())
	}
}

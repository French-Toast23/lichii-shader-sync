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

// refreshExtras updates the Share / Undo / Delete backups buttons after a scan.
func (a *app) refreshExtras() {
	extra := 0
	for _, g := range a.found {
		for _, f := range g.Files {
			if f.Extra > extra {
				extra = f.Extra
			}
		}
	}
	a.shareBtn.SetEnabled(extra > 0 && !a.busy)
	if extra > 0 {
		a.shareBtn.SetText(fmt.Sprintf("⇪  Share my shaders (%s)", thousands(extra)))
	} else {
		a.shareBtn.SetText("⇪  Share my shaders")
	}
	baks := syncer.Backups(a.cfg.DataDir)
	var size int64
	for _, b := range baks {
		size += b.Size
	}
	a.undoBtn.SetEnabled(len(baks) > 0 && !a.busy)
	a.bakBtn.SetEnabled(len(baks) > 0 && !a.busy)
	if len(baks) > 0 {
		a.bakBtn.SetText("🗑  Delete backups (" + gb(size) + ")")
	} else {
		a.bakBtn.SetText("🗑  Delete backups")
	}
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

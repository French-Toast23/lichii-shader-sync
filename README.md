<div align="center">

<img src="assets/icon-256.png" width="128" alt="Lichii Shader Sync icon">

# Lichii Shader Sync

Downloads shared Vulkan shader caches into Lichii / Eden, so games stutter less on first play.

[![Downloads](https://img.shields.io/github/downloads/oceanpacificpeace/lichii-shader-sync/total?color=2ea043&logo=github)](https://github.com/oceanpacificpeace/lichii-shader-sync/releases)
[![OoT shaders](https://img.shields.io/badge/Ocarina%20of%20Time-70%2C874%20shaders-d92b5a?logo=nintendoswitch&logoColor=white)](https://github.com/oceanpacificpeace/lichii-shader-sync/releases/tag/0400486024DEC000-20261107)
[![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux-0078d4?logo=windows&logoColor=white)](https://github.com/oceanpacificpeace/lichii-shader-sync/releases/latest)
[![Go](https://img.shields.io/badge/Go-1.15%2B-00ADD8?logo=go&logoColor=white)](go.mod)

**[⬇ Download for Windows](https://github.com/oceanpacificpeace/lichii-shader-sync/releases/latest/download/Lichii-Shader-Sync.exe)** &nbsp;·&nbsp; [All downloads](https://github.com/oceanpacificpeace/lichii-shader-sync/releases/latest)

</div>

## Use
1. Play the game once in Lichii, then close Lichii completely.
2. Run **Lichii-Shader-Sync.exe** (SmartScreen: *More info → Run anyway*) and click **Update shaders**.
3. Launch Lichii.

> **Build 117+:** keep `EDEN_NX2_PRELOAD_ALL_PIPELINES` off in `nx2_switches.txt` for normal play. These
> builds evict unused pipelines after ~2.5 min, which freezes the game for ~20 s when a big pack is preloaded.
>
> **Optional warm-up** (once, and again after GPU driver updates): set the GPU's shader cache size to
> 10 GB or Unlimited, turn preload on, boot the game, wait at the title screen until compiling finishes,
> quit, then turn preload off. The driver keeps the compiled shaders, so first-time hitches are shorter.

Command line: `lichii-sync sync` (`-dry-run` to preview). Every download is checksum-verified and
validated; your previous cache is kept as `.bak`.

## Publishing a pack
```
lichii-sync pack -title <TITLEID> [-prev <TITLEID>.json] <cache.bin> out/ <TITLEID>-<date>
gh release create <TITLEID>-<date> out/<new chunks>
gh release upload manifests out/<TITLEID>.json --clobber
```

Unofficial; not affiliated with Lichii or Eden.

// Package windowsscripts embeds the Windows image-preparation scripts so the
// controller can push them into a guest through the QEMU guest agent.
package windowsscripts

import "embed"

// Files holds the scripts copied into the guest by `scaleset prepare-windows-template`.
//
//go:embed Install-JitRunnerWatcher.ps1 Watch-JitRunner.ps1 Test-JitConfig.ps1 Initialize-InteractiveRunner.ps1
var Files embed.FS

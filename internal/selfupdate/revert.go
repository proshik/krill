package selfupdate

import (
	"path/filepath"
	"strings"
)

// revertScript returns the shell script the dead-man timer runs if the new
// binary never confirms its startup. It is one line of `; `-separated
// commands and every substituted value goes through shellQuote, so a path
// with a space or a quote in it cannot change what the script does.
//
// The first command makes a late firing harmless: ConfirmStartup removes the
// pending marker before it stops the timer, so a timer that could not be
// stopped finds no marker and exits without touching anything.
func revertScript(bin, runDir, unit, tag string) string {
	pending := shellQuote(filepath.Join(runDir, pendingMarkerName))
	reverted := shellQuote(filepath.Join(runDir, revertedMarkerName))
	prev := shellQuote(bin + prevSuffix)
	qbin := shellQuote(bin)
	qunit := shellQuote(unit)
	return strings.Join([]string{
		"[ -e " + pending + " ] || exit 0",
		"if [ -e " + prev + " ]; then mv -f " + prev + " " + qbin + "; fi",
		"rm -f " + pending,
		"echo " + shellQuote(tag) + " > " + reverted,
		"systemctl reset-failed " + qunit,
		"systemctl restart " + qunit,
	}, "; ")
}

// shellQuote quotes s as a single POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

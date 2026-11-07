//go:build plugin_purego && (linux || darwin) && !android && !ios && (amd64 || arm64) && !windows && !(cgo && (linux || darwin || freebsd))

package pluginhost

// The purego loader opens plugin files directly and never creates shadow copies.
func cleanupStaleShadowPlugins(string, []pluginFile) {}

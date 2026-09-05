//go:build !windows && !(cgo && (linux || darwin || freebsd)) && !(plugin_purego && (linux || darwin) && !android && !ios && (amd64 || arm64))

package pluginhost

const supportPluginValue = "0"

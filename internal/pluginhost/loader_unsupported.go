//go:build !windows && !(cgo && (linux || darwin || freebsd)) && !(plugin_purego && (linux || darwin) && !android && !ios && (amd64 || arm64))

package pluginhost

import "fmt"

type unsupportedLoader struct{}

func (unsupportedLoader) Open(file pluginFile, host *Host) (pluginClient, error) {
	return nil, fmt.Errorf("dynamic library plugin loading is not available in this build: %s", file.Path)
}

func defaultPluginLoader() pluginLoader {
	return unsupportedLoader{}
}

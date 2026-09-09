//go:build cgo && linux && !plugin_purego && (amd64 || arm64)

package pluginhost

import (
	"bytes"
	"os"
	"testing"
)

func TestUnixShutdownRetainsImageButReleasesCallbackContext(t *testing.T) {
	path := buildNativeFixture(t)
	opened, errOpen := defaultPluginLoader().Open(pluginFile{ID: "retention", Path: path}, New())
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	client := opened.(*dynamicLibraryClient)
	client.Shutdown()
	client.Shutdown()
	if client.hostAPI != nil || client.hostCtx != nil || client.api.shutdown != nil {
		t.Fatal("shutdown retained callback context or shutdown entrypoint")
	}
	mappings, errRead := os.ReadFile("/proc/self/maps")
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !bytes.Contains(mappings, []byte(path)) {
		t.Fatal("shutdown unmapped the native image; Go runtime threads may still execute it")
	}
}

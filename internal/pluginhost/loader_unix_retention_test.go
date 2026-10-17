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
	id := *(*uintptr)(client.hostCtx)
	client.Shutdown()
	client.Shutdown()
	// The C host context stays allocated because the resident runtime may still
	// pass it back into cliproxyHostCall; only its registration is released.
	if _, registered := hostCallbackEntries.Load(id); registered || client.api.shutdown != nil || client.api.call != nil {
		t.Fatal("shutdown retained callback registration or plugin entrypoints")
	}
	mappings, errRead := os.ReadFile("/proc/self/maps")
	if errRead != nil {
		t.Fatal(errRead)
	}
	// The loader maps a shadow copy so the source artifact can be replaced.
	if client.tempPath == "" || !bytes.Contains(mappings, []byte(client.tempPath)) {
		t.Fatal("shutdown unmapped the native image; Go runtime threads may still execute it")
	}
}

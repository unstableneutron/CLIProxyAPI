package pluginhost

// SupportPluginHeaderValue reports whether this build includes a native plugin loader.
func SupportPluginHeaderValue() string {
	return supportPluginValue
}

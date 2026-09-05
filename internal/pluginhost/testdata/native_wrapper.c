/* Two distinct images can expose the same initializer through a dependency. */
extern int cliproxy_plugin_init(const void *, void *);
void *fixture_dependency(void) { return (void *)cliproxy_plugin_init; }

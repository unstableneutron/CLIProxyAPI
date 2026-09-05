#include <stdint.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <stdatomic.h>
#include <pthread.h>
#include <unistd.h>

typedef struct { void *ptr; size_t len; } buffer;
typedef struct {
    uint32_t abi_version;
    void *host_ctx;
    int (*call)(void *, const char *, const uint8_t *, size_t, buffer *);
    void (*free_buffer)(void *, size_t);
} host_api;
typedef struct {
    uint32_t abi_version;
    int (*call)(const char *, const uint8_t *, size_t, buffer *);
    void (*free_buffer)(void *, size_t);
    void (*shutdown)(void);
} plugin_api;

_Static_assert(sizeof(buffer) == 16, "ABI buffer");
_Static_assert(sizeof(host_api) == 32, "ABI host");
_Static_assert(sizeof(plugin_api) == 32, "ABI plugin");
_Static_assert(offsetof(host_api, host_ctx) == 8, "ABI context");
_Static_assert(offsetof(plugin_api, call) == 8, "ABI call");

static const host_api *host;
static _Atomic int outstanding, init_count, stopped, entered;
static buffer held;

static void write_buffer(buffer *out, const void *data, size_t len) {
    out->ptr = malloc(len ? len : 1);
    if (!out->ptr) abort();
    out->len = len;
    if (len) memcpy(out->ptr, data, len);
    atomic_fetch_add(&outstanding, 1);
}
static void free_buffer(void *ptr, size_t len) {
    (void)len;
    if (ptr) { free(ptr); atomic_fetch_sub(&outstanding, 1); }
}
static int callback(const char *method, const uint8_t *req, size_t len, buffer *out) {
    buffer response = {0};
    int rc = host->call(host->host_ctx, method, req, len, &response);
    write_buffer(out, response.ptr, response.len);
    if (response.ptr) host->free_buffer(response.ptr, response.len);
    return rc;
}
typedef struct { const uint8_t *req; size_t len; buffer *out; int rc; } thread_args;
static void *foreign_call(void *raw) {
    thread_args *args = raw;
    args->rc = callback("host.http.do", args->req, args->len, args->out);
    return NULL;
}
static int call(const char *method, const uint8_t *req, size_t len, buffer *out) {
    *out = (buffer){0};
    if (!strcmp(method, "noop")) return 0;
    if (!strcmp(method, "stats")) {
        char text[80];
        int n = snprintf(text, sizeof(text), "%d,%d,%d,%d", atomic_load(&outstanding),
                         atomic_load(&init_count), atomic_load(&stopped), atomic_load(&entered));
        write_buffer(out, text, (size_t)n);
        return 0;
    }
    if (!strcmp(method, "delay")) {
        atomic_store(&entered, 1);
        usleep(100000);
    }
    if (!strcmp(method, "late")) {
        buffer response = {0};
        int rc = host->call(host->host_ctx, "missing.method", NULL, 0, &response);
        if (response.ptr) host->free_buffer(response.ptr, response.len);
        char digit = rc ? '1' : '0';
        write_buffer(out, &digit, 1);
        return 0;
    }
    if (!strcmp(method, "hold")) {
        return host->call(host->host_ctx, "missing.method", NULL, 0, &held);
    }
    if (!strcmp(method, "free-held")) {
        host->free_buffer(held.ptr, held.len);
        held = (buffer){0};
        return 0;
    }
    if (!strcmp(method, "foreign")) {
        thread_args args = {req, len, out, 0};
        pthread_t thread;
        if (pthread_create(&thread, NULL, foreign_call, &args)) return 1;
        if (pthread_join(thread, NULL)) return 1;
        return args.rc;
    }
    if (!strncmp(method, "host.", 5)) return callback(method, req, len, out);
    if (!strcmp(method, "error")) {
        const char *text = "{\"ok\":false,\"error\":{\"code\":\"fixture\",\"message\":\"expected\"}}";
        write_buffer(out, text, strlen(text));
        return -7;
    }
    if (!strcmp(method, "raw-error")) return -7;
    if (!strcmp(method, "plugin.register")) {
#ifdef BAD_REGISTRATION
        write_buffer(out, "not-json", 8);
#else
        const char *text = "{\"ok\":true,\"result\":{\"schema_version\":5,\"metadata\":{\"Name\":\"fixture\"},\"capabilities\":{}}}";
        write_buffer(out, text, strlen(text));
#endif
        return 0;
    }
    write_buffer(out, req, len);
    return 0;
}
static void shutdown_plugin(void) { atomic_fetch_add(&stopped, 1); }

#ifndef NO_INIT
int cliproxy_plugin_init(const host_api *input, plugin_api *output) {
    host = input;
    atomic_fetch_add(&init_count, 1);
    *output = (plugin_api){1, call, free_buffer, shutdown_plugin};
    buffer response = {0};
    if (host->call(host->host_ctx, "missing.method", NULL, 0, &response)) return 2;
    if (response.ptr) host->free_buffer(response.ptr, response.len);
#ifdef BAD_ABI
    output->abi_version = 99;
#endif
#ifdef BAD_TABLE
    output->free_buffer = NULL;
#endif
#ifdef FAIL_INIT
    return -3;
#endif
    return 0;
}
#endif

#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>

typedef int CUdevice;
typedef void *CUcontext;
typedef unsigned long long CUdeviceptr;
typedef struct { unsigned char bytes[16]; } CUuuid;
#define LOAD(name, ...) int (*name)(__VA_ARGS__) = dlsym(lib, #name); if (!name) { fprintf(stderr, "missing %s\n", #name); return 2; }
#define CHECK(call) do { int result = (call); if (result) { fprintf(stderr, "%s: CUDA error %d\n", #call, result); return 2; } } while (0)

int main(void) {
    const char *dir = getenv("NVIDIA_CTK_LIBCUDA_DIR");
    char path[4096];
    if (!dir || snprintf(path, sizeof(path), "%s/libcuda.so.1", dir) >= (int)sizeof(path)) return 2;
    void *lib = dlopen(path, RTLD_NOW);
    if (!lib) { fprintf(stderr, "%s\n", dlerror()); return 2; }
    LOAD(cuInit, unsigned int);
    LOAD(cuDeviceGetCount, int *);
    LOAD(cuDeviceGet, CUdevice *, int);
    LOAD(cuDeviceGetUuid, CUuuid *, CUdevice);
    LOAD(cuCtxCreate_v2, CUcontext *, unsigned int, CUdevice);
    LOAD(cuMemAlloc_v2, CUdeviceptr *, size_t);
    LOAD(cuMemsetD32_v2, CUdeviceptr, unsigned int, size_t);
    LOAD(cuMemcpyDtoH_v2, void *, CUdeviceptr, size_t);
    LOAD(cuMemFree_v2, CUdeviceptr);
    LOAD(cuCtxDestroy_v2, CUcontext);
    CHECK(cuInit(0));
    int count = 0;
    CHECK(cuDeviceGetCount(&count));
    if (count != 1) { fprintf(stderr, "expected one visible GPU, got %d\n", count); return 3; }
    CUdevice device; CUuuid uuid; CUcontext ctx; CUdeviceptr memory; unsigned int value = 0;
    CHECK(cuDeviceGet(&device, 0));
    CHECK(cuDeviceGetUuid(&uuid, device));
    CHECK(cuCtxCreate_v2(&ctx, 0, device));
    CHECK(cuMemAlloc_v2(&memory, sizeof(value)));
    CHECK(cuMemsetD32_v2(memory, 42, 1));
    CHECK(cuMemcpyDtoH_v2(&value, memory, sizeof(value)));
    CHECK(cuMemFree_v2(memory));
    CHECK(cuCtxDestroy_v2(ctx));
    printf("uuid=GPU-");
    for (int i = 0; i < 16; i++) { if (i == 4 || i == 6 || i == 8 || i == 10) putchar('-'); printf("%02x", uuid.bytes[i]); }
    printf("\ncount=%d\nvalue=%u\n", count, value);
    dlclose(lib);
    return value == 42 ? 0 : 4;
}

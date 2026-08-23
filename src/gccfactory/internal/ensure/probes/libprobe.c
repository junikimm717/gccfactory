/* libprobe.c -- built as libprobe.so (-shared -fPIC) and dlopen'd by dlopen.c.
   Proves -fPIC codegen, the shared-object link path, and the dynamic loader. */
#include <string.h>

int probe_calls = 0;

int probe_add(int a, int b) {
	probe_calls++;
	return a + b;
}

const char *probe_name(void) {
	return "libprobe";
}

int probe_strlen(const char *s) {
	return (int)strlen(s);
}

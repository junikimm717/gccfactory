/* dlopen.c -- loads ./libprobe.so at runtime and calls into it. Proves the
   dynamic loader, dlsym on both a function and a data symbol, and dlclose. */
#include <dlfcn.h>
#include <stdio.h>
#include <string.h>

int main(void) {
	void *h;
	int (*add)(int, int);
	const char *(*name)(void);
	int *calls;
	void *bogus;

	h = dlopen("./libprobe.so", RTLD_NOW);
	if (!h) {
		printf("BAD dlopen: %s\n", dlerror());
		return 1;
	}
	dlerror();

	*(void **)&add = dlsym(h, "probe_add");
	*(void **)&name = dlsym(h, "probe_name");
	calls = dlsym(h, "probe_calls");
	if (!add || !name || !calls) {
		printf("BAD dlsym: %s\n", dlerror());
		return 1;
	}

	printf("name=%s\n", name());
	printf("add=%d\n", add(19, 23));
	printf("add=%d\n", add(-1, 1));
	printf("calls=%d\n", *calls);

	bogus = dlsym(h, "probe_does_not_exist");
	printf("missing=%s\n", bogus == NULL ? "null" : "found");

	if (dlclose(h) != 0) {
		printf("BAD dlclose: %s\n", dlerror());
		return 1;
	}
	puts("OK dlopen");
	return 0;
}

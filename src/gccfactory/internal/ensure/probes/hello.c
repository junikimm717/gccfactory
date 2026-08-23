/* hello.c -- the smoke test: libc startup, stdio, string, heap. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static char *shout(const char *s) {
	size_t n = strlen(s);
	char *out = malloc(n + 1);
	size_t i;
	if (!out)
		return NULL;
	for (i = 0; i < n; i++)
		out[i] = (s[i] >= 'a' && s[i] <= 'z') ? (char)(s[i] - 32) : s[i];
	out[n] = '\0';
	return out;
}

int main(void) {
	char buf[64];
	char *up;

	snprintf(buf, sizeof buf, "%s/%d/%.2f/%c", "hello", 42, 1.5, 'x');
	if (strcmp(buf, "hello/42/1.50/x") != 0) {
		printf("BAD snprintf: %s\n", buf);
		return 1;
	}
	printf("%s\n", buf);

	up = shout("musl toolchain");
	if (!up)
		return 1;
	printf("%s\n", up);
	free(up);

	if (fflush(stdout) != 0)
		return 1;
	puts("OK hello");
	return 0;
}

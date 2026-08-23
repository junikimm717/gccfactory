/* static.c -- exercises the parts of libc that are most likely to break when
   linked -static: qsort/bsearch (function pointers), strtol, realloc, locale,
   and the stdio buffering setup. */
#include <locale.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int cmp_int(const void *a, const void *b) {
	int x = *(const int *)a, y = *(const int *)b;
	return (x > y) - (x < y);
}

static int cmp_str(const void *a, const void *b) {
	return strcmp(*(const char *const *)a, *(const char *const *)b);
}

int main(void) {
	static const char *words[] = {"delta", "alpha", "charlie", "bravo"};
	int v[] = {9, 4, 7, 1, 8, 2, 6, 3, 5, 0};
	const size_t n = sizeof v / sizeof v[0];
	size_t i;
	int key = 6;
	int *found;
	char *buf;
	long parsed;
	char *end;

	setlocale(LC_ALL, "C");

	qsort(v, n, sizeof v[0], cmp_int);
	fputs("sorted:", stdout);
	for (i = 0; i < n; i++)
		printf(" %d", v[i]);
	putchar('\n');

	found = bsearch(&key, v, n, sizeof v[0], cmp_int);
	printf("bsearch=%d\n", found ? *found : -1);

	qsort(words, sizeof words / sizeof words[0], sizeof words[0], cmp_str);
	printf("words=%s,%s,%s,%s\n", words[0], words[1], words[2], words[3]);

	buf = malloc(8);
	if (!buf)
		return 1;
	strcpy(buf, "abc");
	buf = realloc(buf, 64);
	if (!buf)
		return 1;
	strcat(buf, "def");
	printf("realloc=%s len=%d\n", buf, (int)strlen(buf));
	free(buf);

	parsed = strtol("  -12345xyz", &end, 10);
	printf("strtol=%ld rest=%s\n", parsed, end);
	puts("OK static");
	return 0;
}

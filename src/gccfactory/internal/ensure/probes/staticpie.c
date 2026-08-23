/* staticpie.c -- proves rcrt1 self-relocation under -static-pie: the
   function pointer and the pointer-to-global below are fixed up by
   R_*_RELATIVE relocations before main runs, with no dynamic linker
   present to do it. A broken static-pie would load them as zero/garbage
   and crash or print a wrong, non-constant-foldable value. */
#include <stdio.h>

static int counter = 17;

static int bump(int x) {
	return x + counter;
}

typedef int (*fn)(int);

static fn dispatch = bump;
static int *global_ptr = &counter;

int main(void) {
	int r;

	*global_ptr += 5;
	r = dispatch(10);
	printf("counter=%d ptr=%d call=%d\n", counter, *global_ptr, r);
	if (r != 32) {
		puts("BAD static-pie");
		return 1;
	}
	puts("OK static-pie");
	return 0;
}

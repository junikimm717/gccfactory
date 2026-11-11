#include <setjmp.h>
#include <stdio.h>
#include <stdint.h>

int sigjmp_far(int save, int *blocked);
extern char _DYNAMIC[] __attribute__((weak));

int main(void)
{
	jmp_buf j;
	volatile int n = 0;
	if (setjmp(j) < 2) longjmp(j, ++n);
	printf("setjmp=%d\n", n);

	// A static link must place ___setjmp beyond a 16-bit branch from __sigsetjmp, or this probe tests nothing.
	intptr_t d = (char *)(uintptr_t)sigjmp_far - (char *)(uintptr_t)setjmp;
	if (!_DYNAMIC && (d < 0x10000 && d > -0x10000)) printf("layout=near\n");

	int blocked;
	int r = sigjmp_far(0, &blocked);
	printf("nosave=%d blocked=%d\n", r, blocked);
	r = sigjmp_far(1, &blocked);
	printf("save=%d blocked=%d\n", r, blocked);
	printf("OK sigsetjmp\n");
	return 0;
}

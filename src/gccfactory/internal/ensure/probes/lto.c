/* lto.c -- the caller half of the LTO probe. The helper lives in a second
   translation unit, so -flto has real cross-TU work to do: with LTO enabled
   the linker plugin must merge both units before codegen. */
#include <stdio.h>

int helper_add(int a, int b);
int helper_scale(int x);
const char *helper_tag(void);
int helper_calls(void);
int helper_secret(void);

int main(void) {
	printf("add=%d\n", helper_add(19, 23));
	printf("scale=%d\n", helper_scale(7));
	printf("tag=%s\n", helper_tag());
	printf("calls=%d\n", helper_calls());
	/* helper_secret's value is only computable after the two units are merged:
	   it folds a constant that lives in the other unit. */
	printf("secret=%d\n", helper_secret() + 1);
	puts("OK lto");
	return 0;
}

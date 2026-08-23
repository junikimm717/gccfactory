/* ltohelp.c -- the second translation unit of the LTO probe. It is also the
   unit archived into liba.a with gcc-ar, which is the path that needs the
   linker plugin. */
static int calls;

static int secret_seed(void) {
	return 41;
}

int helper_add(int a, int b) {
	calls++;
	return a + b;
}

int helper_scale(int x) {
	return helper_add(x, x) * 3;
}

const char *helper_tag(void) {
	return "lto";
}

int helper_calls(void) {
	return calls;
}

int helper_secret(void) {
	return secret_seed();
}

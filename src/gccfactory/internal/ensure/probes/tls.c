/* tls.c -- __thread storage. Exercises both a zero-initialised (.tbss) and a
   value-initialised (.tdata) TLS object, which is where broken TLS models on
   arm/mips/riscv show up first. */
#include <pthread.h>
#include <stdio.h>
#include <string.h>

#define NTHREADS 3

static __thread int slot;             /* .tbss  */
static __thread int base = 7;         /* .tdata */
static __thread char tag[8] = "none"; /* .tdata */

static int results[NTHREADS];
static char tags[NTHREADS][8];

struct arg {
	int id;
};

static void *worker(void *p) {
	struct arg *a = p;
	int i;

	if (base != 7 || strcmp(tag, "none") != 0)
		return (void *)1;

	slot = 100 + a->id * 10;
	base += a->id;
	snprintf(tag, sizeof tag, "t%d", a->id);

	/* spin a bit so the threads interleave; slot must stay private */
	for (i = 0; i < 1000; i++) {
		if (slot != 100 + a->id * 10)
			return (void *)1;
	}
	results[a->id] = slot + base;
	memcpy(tags[a->id], tag, sizeof tag);
	return NULL;
}

int main(void) {
	pthread_t th[NTHREADS];
	struct arg args[NTHREADS];
	int i;

	slot = 1;
	for (i = 0; i < NTHREADS; i++) {
		args[i].id = i;
		if (pthread_create(&th[i], NULL, worker, &args[i]) != 0) {
			puts("BAD pthread_create");
			return 1;
		}
	}
	for (i = 0; i < NTHREADS; i++) {
		void *rv = NULL;
		pthread_join(th[i], &rv);
		if (rv != NULL) {
			puts("BAD tls isolation");
			return 1;
		}
	}
	for (i = 0; i < NTHREADS; i++)
		printf("tls[%d]=%d tag=%s\n", i, results[i], tags[i]);
	printf("main slot=%d base=%d tag=%s\n", slot, base, tag);
	puts("OK tls");
	return 0;
}

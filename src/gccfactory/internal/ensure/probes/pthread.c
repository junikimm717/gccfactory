/* pthread.c -- threads, mutex, condvar. Proves libpthread (in musl: libc) and
   that the thread pointer / clone path works for this arch. */
#include <pthread.h>
#include <stdio.h>
#include <string.h>

#define NTHREADS 4
#define NITER 25000

static pthread_mutex_t mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t cv = PTHREAD_COND_INITIALIZER;
static long counter;
static int started;

static void *worker(void *arg) {
	int i;
	(void)arg;
	pthread_mutex_lock(&mu);
	started++;
	pthread_cond_broadcast(&cv);
	pthread_mutex_unlock(&mu);

	for (i = 0; i < NITER; i++) {
		pthread_mutex_lock(&mu);
		counter++;
		pthread_mutex_unlock(&mu);
	}
	return (void *)(long)0xbeef;
}

int main(void) {
	pthread_t th[NTHREADS];
	int i;

	for (i = 0; i < NTHREADS; i++) {
		if (pthread_create(&th[i], NULL, worker, NULL) != 0) {
			puts("BAD pthread_create");
			return 1;
		}
	}

	pthread_mutex_lock(&mu);
	while (started < NTHREADS)
		pthread_cond_wait(&cv, &mu);
	pthread_mutex_unlock(&mu);

	for (i = 0; i < NTHREADS; i++) {
		void *rv = NULL;
		if (pthread_join(th[i], &rv) != 0 || (long)rv != 0xbeef) {
			puts("BAD pthread_join");
			return 1;
		}
	}

	printf("started=%d\n", started);
	printf("sum=%ld\n", counter);
	puts("OK pthread");
	return 0;
}

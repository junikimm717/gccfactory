/* atomic.c -- 64-bit atomics from 4 threads. On 32-bit targets these lower to
   libatomic calls, so this probe also proves libatomic was built and is
   linkable. */
#include <pthread.h>
#include <stdio.h>

#define NTHREADS 4
#define NITER 20000

static long long counter;
static int flag;

static void *worker(void *arg) {
	int i;
	(void)arg;
	for (i = 0; i < NITER; i++)
		__atomic_fetch_add(&counter, 1LL, __ATOMIC_SEQ_CST);
	return NULL;
}

int main(void) {
	pthread_t th[NTHREADS];
	int i;
	long long expect = 0;
	long long got;
	int zero = 0;

	for (i = 0; i < NTHREADS; i++) {
		if (pthread_create(&th[i], NULL, worker, NULL) != 0) {
			puts("BAD pthread_create");
			return 1;
		}
	}
	for (i = 0; i < NTHREADS; i++)
		pthread_join(th[i], NULL);

	got = __atomic_load_n(&counter, __ATOMIC_SEQ_CST);
	printf("counter=%lld\n", got);

	expect = got;
	printf("cas_hit=%d\n", __atomic_compare_exchange_n(&counter, &expect, 1LL, 0,
	                                                   __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST));
	expect = 999;
	printf("cas_miss=%d\n", __atomic_compare_exchange_n(&counter, &expect, 2LL, 0,
	                                                    __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST));
	printf("after=%lld witness=%lld\n", __atomic_load_n(&counter, __ATOMIC_SEQ_CST), expect);

	__atomic_store_n(&flag, 5, __ATOMIC_RELEASE);
	printf("flag=%d free=%d\n", __atomic_load_n(&flag, __ATOMIC_ACQUIRE),
	       __atomic_always_lock_free(sizeof(int), &zero));
	puts("OK atomic");
	return 0;
}

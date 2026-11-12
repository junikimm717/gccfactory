#include <setjmp.h>
#include <signal.h>

static sigjmp_buf b;

int sigjmp_far(int save, int *blocked)
{
	sigset_t s, cur;
	volatile int hits = 0;
	sigemptyset(&s);
	sigaddset(&s, SIGUSR1);
	sigprocmask(SIG_UNBLOCK, &s, 0);
	int r = sigsetjmp(b, save);
	hits++;
	if (r == 0) {
		sigprocmask(SIG_BLOCK, &s, 0);
		siglongjmp(b, 7);
	}
	sigprocmask(SIG_SETMASK, 0, &cur);
	*blocked = sigismember(&cur, SIGUSR1);
	sigprocmask(SIG_UNBLOCK, &s, 0);
	return r * 10 + hits;
}

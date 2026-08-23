/* math.c -- proves libm is linkable and correct. Inputs are volatile so -O2
   cannot constant-fold the calls away. */
#include <math.h>
#include <stdio.h>

int main(void) {
	volatile double two = 2.0;
	volatile double ten = 10.0;
	volatile double three = 3.0;
	volatile double four = 4.0;
	volatile double one = 1.0;
	volatile double seven = 7.0;

	printf("sqrt=%.6f\n", sqrt(two));
	printf("pow=%.6f\n", pow(two, ten));
	printf("hypot=%.6f\n", hypot(three, four));
	printf("fmod=%.6f\n", fmod(seven, three));
	printf("exp=%.6f\n", exp(one));
	printf("floor=%.6f ceil=%.6f\n", floor(-two / three), ceil(two / three));

	if (!isnan(sqrt(-one))) {
		puts("BAD nan");
		return 1;
	}
	if (!isinf(one / (one - one))) {
		puts("BAD inf");
		return 1;
	}
	puts("OK math");
	return 0;
}

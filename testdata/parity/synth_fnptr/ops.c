/* C function-pointer dispatch fixture (step 7.5). */

typedef struct {
	int (*run)(int);
} Op;

static int add_one(int x) {
	return x + 1;
}

static int mul_two(int x) {
	return x * 2;
}

/* registrations via designated initializers */
static Op table[] = {
	{ .run = add_one },
	{ .run = mul_two },
};

/* also assignment form */
void install_hook(Op *op) {
	op->run = add_one;
}

/* dispatcher — synth should link dispatch → add_one and mul_two */
int dispatch(Op *op, int x) {
	return op->run(x);
}

int run_first(int x) {
	return dispatch(&table[0], x);
}

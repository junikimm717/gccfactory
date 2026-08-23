// except.cc -- throwing across a non-inlined function boundary proves the
// unwinder (libgcc_eh / .eh_frame) and the RTTI tables are correct for this
// target. Destructor side effects prove unwind cleanups actually run.
#include <exception>
#include <iostream>
#include <stdexcept>
#include <string>

namespace {

int destroyed = 0;

struct Tracer {
	std::string tag;
	explicit Tracer(std::string t) : tag(std::move(t)) {}
	~Tracer() { destroyed++; }
};

struct Boom : std::runtime_error {
	int code;
	Boom(const std::string &m, int c) : std::runtime_error(m), code(c) {}
};

__attribute__((noinline)) void deep(int n) {
	Tracer t("frame" + std::to_string(n));
	if (n == 0)
		throw Boom("boom at bottom", 17);
	deep(n - 1);
	std::cout << "unreachable " << t.tag << "\n";
}

__attribute__((noinline)) void rethrower() {
	try {
		deep(3);
	} catch (const std::exception &) {
		throw;
	}
}

} // namespace

int main() {
	try {
		rethrower();
	} catch (const Boom &b) {
		std::cout << "caught=" << b.what() << " code=" << b.code << "\n";
	} catch (...) {
		std::cout << "BAD wrong handler\n";
		return 1;
	}
	std::cout << "destroyed=" << destroyed << "\n";

	try {
		throw std::out_of_range("range");
	} catch (const std::logic_error &e) {
		std::cout << "hierarchy=" << e.what() << "\n";
	}

	try {
		try {
			throw 42;
		} catch (int v) {
			std::cout << "int=" << v << "\n";
			throw std::string("nested");
		}
	} catch (const std::string &s) {
		std::cout << "nested=" << s << "\n";
	}

	std::cout << "OK except" << std::endl;
	return 0;
}

// stdcxx.cc -- the wide surface of libstdc++: containers, algorithms, smart
// pointers, <regex> (a notorious link-time canary), and std::thread/std::mutex
// which require the gthreads glue to have been configured against musl.
#include <algorithm>
#include <iostream>
#include <map>
#include <memory>
#include <mutex>
#include <regex>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

namespace {

std::mutex mu;
long shared_sum = 0;

void adder(int from, int to) {
	long local = 0;
	for (int i = from; i <= to; i++)
		local += i;
	std::lock_guard<std::mutex> lk(mu);
	shared_sum += local;
}

struct Node {
	int v;
	std::unique_ptr<Node> next;
	explicit Node(int x) : v(x) {}
};

} // namespace

int main() {
	std::vector<std::string> names{"delta", "alpha", "echo", "bravo", "charlie"};
	std::sort(names.begin(), names.end());
	std::ostringstream oss;
	for (size_t i = 0; i < names.size(); i++)
		oss << (i ? "," : "") << names[i];
	std::cout << "sorted=" << oss.str() << "\n";

	std::map<std::string, int> counts;
	for (const auto &n : names)
		counts[std::string(1, n[0])] += static_cast<int>(n.size());
	std::cout << "map=" << counts.size() << " a=" << counts["a"] << " c=" << counts["c"] << "\n";

	std::vector<std::thread> ths;
	for (int i = 0; i < 4; i++)
		ths.emplace_back(adder, i * 250 + 1, (i + 1) * 250);
	for (auto &t : ths)
		t.join();
	std::cout << "threads=" << shared_sum << "\n";

	std::regex re(R"(([a-z]+)-(\d+))");
	std::smatch m;
	const std::string subject = "target: riscv-1234 end";
	std::cout << "regex=" << (std::regex_search(subject, m, re) ? m[1].str() + "/" + m[2].str()
	                                                            : std::string("nomatch"))
	          << "\n";

	auto head = std::make_unique<Node>(1);
	head->next = std::make_unique<Node>(2);
	head->next->next = std::make_unique<Node>(3);
	int chain = 0;
	for (Node *p = head.get(); p; p = p->next.get())
		chain = chain * 10 + p->v;
	std::cout << "chain=" << chain << "\n";

	auto it = std::find_if(names.begin(), names.end(),
	                       [](const std::string &s) { return s.size() == 7; });
	std::cout << "lambda=" << (it == names.end() ? "none" : *it) << "\n";
	std::cout << "OK stdcxx" << std::endl;
	return 0;
}

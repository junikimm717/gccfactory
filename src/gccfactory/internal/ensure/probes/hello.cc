// hello.cc -- libstdc++ basics: iostream (static init of the stream objects),
// std::string, std::vector, algorithms.
#include <iostream>
#include <numeric>
#include <string>
#include <vector>

namespace {

struct Widget {
	std::string name;
	int weight;
};

int total(const std::vector<Widget> &ws) {
	int sum = 0;
	for (const auto &w : ws)
		sum += w.weight;
	return sum;
}

} // namespace

int main() {
	std::vector<Widget> ws{{"alpha", 3}, {"bravo", 5}, {"charlie", 7}};
	std::string joined;

	for (const auto &w : ws) {
		if (!joined.empty())
			joined += ",";
		joined += w.name;
	}

	std::vector<int> nums(10);
	std::iota(nums.begin(), nums.end(), 1);

	std::cout << "joined=" << joined << "\n";
	std::cout << "total=" << total(ws) << "\n";
	std::cout << "accumulate=" << std::accumulate(nums.begin(), nums.end(), 0) << "\n";
	std::cout << "substr=" << joined.substr(6, 5) << " size=" << joined.size() << "\n";
	std::cout << "OK hello++" << std::endl;
	return 0;
}

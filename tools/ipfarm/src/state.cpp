#include "state.h"
#include "util.h"

#include <cstdlib>
#include <fstream>
#include <sstream>

void State::load(const std::string& path) {
    std::ifstream in(path);
    if (!in) return;
    std::string line;
    bool header = true;
    while (std::getline(in, line)) {
        line = stripCR(line);
        if (line.empty()) continue;
        if (header) {
            header = false;
            if (line.rfind("name\t", 0) == 0) continue;
        }
        std::vector<std::string> f = split(line, '\t');
        if (f.size() < 9) continue;
        StateNode n;
        n.name = f[0];
        n.proto = f[1];
        n.host = f[2];
        n.port = std::atoi(f[3].c_str());
        n.exit_ip = f[4];
        n.zen_ms = std::atol(f[5].c_str());
        n.ok = std::atol(f[6].c_str());
        n.fail = std::atol(f[7].c_str());
        n.penalized_until = std::atol(f[8].c_str());
        index[n.name] = nodes.size();
        nodes.push_back(std::move(n));
    }
}

void State::save(const std::string& path) const {
    std::ofstream out(path);
    if (!out) return;
    out << "name\tproto\thost\tport\texit_ip\tzen_ms\tok\tfail\tpenalized_until\n";
    for (const auto& n : nodes) {
        out << n.name << '\t' << n.proto << '\t' << n.host << '\t' << n.port << '\t' << n.exit_ip
            << '\t' << n.zen_ms << '\t' << n.ok << '\t' << n.fail << '\t' << n.penalized_until
            << '\n';
    }
}

StateNode* State::find(const std::string& name) {
    auto it = index.find(name);
    if (it == index.end()) return nullptr;
    return &nodes[it->second];
}

const StateNode* State::find(const std::string& name) const {
    auto it = index.find(name);
    if (it == index.end()) return nullptr;
    return &nodes[it->second];
}

StateNode& State::upsert(const std::string& name) {
    auto it = index.find(name);
    if (it != index.end()) return nodes[it->second];
    index[name] = nodes.size();
    nodes.push_back(StateNode{});
    nodes.back().name = name;
    return nodes.back();
}

double score(const StateNode& n, long now) {
    if (n.penalized_until > now) return -1.0;
    return 1000000.0 / static_cast<double>(n.zen_ms + 50) +
           200.0 * static_cast<double>(n.ok) - 400.0 * static_cast<double>(n.fail);
}
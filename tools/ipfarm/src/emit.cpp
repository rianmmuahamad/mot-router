#include "common.h"
#include "state.h"
#include "util.h"

#include <algorithm>
#include <cstdio>
#include <fstream>
#include <set>
#include <sstream>
#include <string>
#include <unordered_set>
#include <vector>

namespace {

struct ZenNode {
    std::string proto;
    std::string host;
    int port = 0;
    std::string src;
    long zen_ms = 0;
    std::string exit_ip;
};

std::vector<ZenNode> readZenOk(const std::string& path) {
    std::vector<ZenNode> out;
    std::ifstream in(path);
    if (!in) return out;
    std::string line;
    bool header = true;
    while (std::getline(in, line)) {
        line = stripCR(line);
        if (line.empty()) continue;
        if (header) {
            header = false;
            if (line.rfind("proto\t", 0) == 0) continue;
        }
        std::vector<std::string> f = split(line, '\t');
        if (f.size() < 7) continue;
        ZenNode n;
        n.proto = f[0];
        n.host = f[1];
        n.port = std::atoi(f[2].c_str());
        n.src = f[3];
        n.zen_ms = std::atol(f[5].c_str());
        n.exit_ip = f[6];
        out.push_back(std::move(n));
    }
    return out;
}

std::string nodeName(size_t shard, size_t seq, long zen_ms) {
    char buf[64];
    std::snprintf(buf, sizeof(buf), "p%02zu-%04zu-%ldms", shard, seq, zen_ms);
    return buf;
}

}  // namespace

int runEmit(const Options& opt) {
    std::string outDir = opt.work + "/out";
    std::string provDir = opt.mihomo_home + "/providers";
    if (!ensureDir(provDir)) {
        std::fprintf(stderr, "emit: cannot create %s\n", provDir.c_str());
        return 1;
    }

    std::vector<ZenNode> nodes = readZenOk(outDir + "/zen_ok.tsv");
    if (nodes.empty()) {
        std::fprintf(stderr, "emit: no zen_ok.tsv rows; run `check` first\n");
        return 1;
    }

    // Fastest first.
    std::sort(nodes.begin(), nodes.end(), [](const ZenNode& a, const ZenNode& b) {
        if (a.zen_ms != b.zen_ms) return a.zen_ms < b.zen_ms;
        return a.host < b.host;
    });

    // One representative per exit IP; empty exit_ip rows are kept individually
    // by host:port.
    std::unordered_set<std::string> seenIP;
    std::unordered_set<std::string> seenHP;
    std::vector<ZenNode> filtered;
    filtered.reserve(nodes.size());
    for (const auto& n : nodes) {
        if (!n.exit_ip.empty()) {
            if (!seenIP.insert(n.exit_ip).second) continue;
        } else {
            std::string hp = n.host + ":" + std::to_string(n.port);
            if (!seenHP.insert(hp).second) continue;
        }
        filtered.push_back(n);
        if (static_cast<int>(filtered.size()) >= opt.max_total) break;
    }

    size_t total = filtered.size();
    size_t shardCount = (total + opt.shard - 1) / opt.shard;
    if (shardCount == 0) shardCount = 1;

    std::vector<std::string> providerNames;
    size_t written = 0;
    for (size_t s = 0; s < shardCount; s++) {
        size_t begin = s * opt.shard;
        size_t end = std::min(begin + static_cast<size_t>(opt.shard), total);
        if (begin >= end) break;

        char nameBuf[32];
        std::snprintf(nameBuf, sizeof(nameBuf), "free-pool-%02zu", s);
        std::string providerName = nameBuf;
        providerNames.push_back(providerName);

        std::string path = provDir + "/" + providerName + ".yaml";
        std::ofstream out(path);
        out << "proxies:\n";
        for (size_t i = begin; i < end; i++) {
            const ZenNode& n = filtered[i];
            std::string nm = nodeName(s, i - begin + 1, n.zen_ms);
            out << "  - {name: \"" << nm << "\", type: " << n.proto << ", server: " << n.host
                << ", port: " << n.port << "}\n";
            written++;
        }
        out.close();
    }

    {
        std::ofstream idx(provDir + "/index.txt");
        for (const auto& n : providerNames) idx << n << '\n';
    }

    // Rewrite state.tsv so its names are the mihomo node names just emitted.
    // check keys rows by host:port (names do not exist yet); rotate matches
    // against mihomo, which only knows the emitted names. Counters survive.
    {
        State prior;
        prior.load(opt.work + "/state.tsv");

        std::vector<StateNode> next;
        size_t seq = 0;
        for (size_t s = 0; s < shardCount; s++) {
            size_t begin = s * opt.shard;
            size_t end = std::min(begin + static_cast<size_t>(opt.shard), total);
            if (begin >= end) break;
            for (size_t i = begin; i < end; i++) {
                const ZenNode& n = filtered[i];
                std::string nm = nodeName(s, i - begin + 1, n.zen_ms);
                StateNode node;
                node.name = nm;
                node.proto = n.proto;
                node.host = n.host;
                node.port = n.port;
                node.exit_ip = n.exit_ip;
                node.zen_ms = n.zen_ms;
                std::string hp = n.host + ":" + std::to_string(n.port);
                if (const StateNode* old = prior.find(hp)) {
                    node.ok = old->ok;
                    node.fail = old->fail;
                    node.penalized_until = old->penalized_until;
                }
                next.push_back(std::move(node));
                seq++;
            }
        }
        State out;
        out.nodes = std::move(next);
        for (size_t i = 0; i < out.nodes.size(); i++) out.index[out.nodes[i].name] = i;
        out.save(opt.work + "/state.tsv");
    }

    std::fprintf(stdout, "shards=%zu nodes=%zu\n", providerNames.size(), written);
    return 0;
}
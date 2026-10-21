#include "common.h"
#include "ctl.h"
#include "state.h"
#include "util.h"

#include <algorithm>
#include <cstdio>
#include <cstdlib>
#include <ctime>
#include <string>
#include <vector>

int runRotate(const Options& opt) {
    State state;
    state.load(opt.work + "/state.tsv");
    if (state.nodes.empty()) {
        std::fprintf(stderr, "rotate: state is empty; run `check` first\n");
        return 1;
    }

    Ctl ctl(opt.mihomo, opt.mihomo_secret, opt.router, opt.group);
    std::string cur = ctl.currentNode();

    long now = static_cast<long>(std::time(nullptr));
    std::string curIP;
    if (const StateNode* cn = state.find(cur)) curIP = cn->exit_ip;

    const StateNode* best = nullptr;
    const StateNode* bestAny = nullptr;
    double bestScore = 0, bestAnyScore = 0;
    for (const auto& n : state.nodes) {
        double s = score(n, now);
        if (s < 0) continue;
        if (!bestAny || s > bestAnyScore) {
            bestAny = &n;
            bestAnyScore = s;
        }
        if (!n.exit_ip.empty() && n.exit_ip != curIP) {
            if (!best || s > bestScore) {
                best = &n;
                bestScore = s;
            }
        }
    }

    const StateNode* pick = best ? best : bestAny;
    if (!pick) {
        std::fprintf(stderr, "rotate: no selectable node (all penalized)\n");
        return 1;
    }

    std::string err;
    if (!ctl.selectNode(pick->name, err)) {
        std::fprintf(stderr, "rotate: select failed: %s\n", err.c_str());
        return 1;
    }

    std::printf("{\"ok\":true,\"group\":\"%s\",\"from\":\"%s\",\"node\":\"%s\","
                "\"exit_ip\":\"%s\"}\n",
                opt.group.c_str(), cur.c_str(), pick->name.c_str(), pick->exit_ip.c_str());
    return 0;
}

int runStats(const Options& opt) {
    State state;
    state.load(opt.work + "/state.tsv");
    long now = static_cast<long>(std::time(nullptr));
    size_t penalized = 0;
    for (const auto& n : state.nodes) {
        if (n.penalized_until > now) penalized++;
    }
    std::printf("state: %zu node, %zu penalized (work=%s)\n", state.nodes.size(), penalized,
                opt.work.c_str());
    return 0;
}
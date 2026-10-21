#pragma once

#include "http.h"

#include <string>

// Options holds the parsed global flags shared by every subcommand.
struct Options {
    std::string work = "/tmp/ipfarm";
    std::string mihomo_home = "/tmp/mihomo-farm";
    std::string mihomo = "http://127.0.0.1:9090";
    std::string mihomo_secret;
    std::string router = "http://127.0.0.1:8787";
    std::string group = "MOT";
    int conc1 = 1000;
    int conc2 = 300;
    int max_total = 4000;
    int shard = 2000;
    int interval = 1800;
};

// Proxy is one candidate as stored in candidates.tsv and zen_ok.tsv.
struct Proxy {
    std::string proto;
    std::string host;
    int port = 0;
    std::string src;
    int prio = 1;
    long zen_ms = 0;
    std::string exit_ip;
};

// StateNode is one row of state.tsv.
struct StateNode {
    std::string name;
    std::string proto;
    std::string host;
    int port = 0;
    std::string exit_ip;
    long zen_ms = 0;
    long ok = 0;
    long fail = 0;
    long penalized_until = 0;
};

int runFetch(const Options& opt);
int runCheck(const Options& opt);
int runEmit(const Options& opt);
int runRotate(const Options& opt);
int runRun(const Options& opt);
int runStats(const Options& opt);

std::string proxyURL(const std::string& proto, const std::string& host, int port);
bool ensureDir(const std::string& path);
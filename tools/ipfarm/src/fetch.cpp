#include "common.h"
#include "util.h"

#include <algorithm>
#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <sstream>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <vector>

namespace {

struct Source {
    std::string proto;
    std::string kind;
    std::string url;
};

bool parseOctets(const std::string& ip) {
    int parts = 0;
    size_t start = 0;
    for (size_t i = 0; i <= ip.size(); i++) {
        if (i == ip.size() || ip[i] == '.') {
            if (i == start) return false;
            std::string seg = ip.substr(start, i - start);
            if (seg.size() > 3) return false;
            for (char c : seg) {
                if (c < '0' || c > '9') return false;
            }
            int v = std::atoi(seg.c_str());
            if (v > 255) return false;
            parts++;
            start = i + 1;
        }
    }
    return parts == 4 && ip != "0.0.0.0";
}

bool validHostPort(const std::string& host, const std::string& portStr, int& port) {
    if (!parseOctets(host)) return false;
    if (portStr.empty() || portStr.size() > 5) return false;
    for (char c : portStr) {
        if (c < '0' || c > '9') return false;
    }
    port = std::atoi(portStr.c_str());
    return port >= 1 && port <= 65535;
}

// mapScheme normalizes a source scheme to what mihomo can carry: http (which
// also covers CONNECT for TLS) or socks5. socks4 is dropped.
bool mapScheme(const std::string& in, std::string& out) {
    std::string s = in;
    std::transform(s.begin(), s.end(), s.begin(), [](unsigned char c) { return std::tolower(c); });
    if (s == "https" || s == "http") {
        out = "http";
        return true;
    }
    if (s == "socks5") {
        out = "socks5";
        return true;
    }
    return false;
}

struct Dedup {
    std::unordered_map<std::string, Proxy> items;
    std::unordered_map<std::string, size_t> order;
    std::vector<std::string> keys;
};

void addProxy(Dedup& d, const std::string& proto, const std::string& host, int port,
              const std::string& src, int prio) {
    std::string key = proto + "|" + host + ":" + std::to_string(port);
    auto it = d.items.find(key);
    if (it != d.items.end()) {
        if (prio < it->second.prio) it->second.prio = prio;
        return;
    }
    Proxy p;
    p.proto = proto;
    p.host = host;
    p.port = port;
    p.src = src;
    p.prio = prio;
    d.items.emplace(key, p);
    d.keys.push_back(key);
}

std::string defaultProtoForKey(const std::string& proto) {
    if (proto == "http" || proto == "socks5") return proto;
    return "http";
}

void parseLines(const Source& s, const std::string& body, Dedup& d, long& added) {
    std::istringstream in(body);
    std::string line;
    while (std::getline(in, line)) {
        line = trim(line);
        if (line.empty() || line[0] == '#') continue;

        std::string proto = defaultProtoForKey(s.proto);
        std::string work = line;
        int prio = 1;

        if (s.kind == "spys_flags") {
            if (line[0] < '0' || line[0] > '9') continue;  // header rows
            if (line.find('H') != std::string::npos || line.find('S') != std::string::npos) {
                prio = 0;
            }
            std::vector<std::string> tok = split(line, ' ');
            if (tok.empty()) continue;
            work = tok[0];
        } else if (s.kind == "hideip3") {
            std::vector<std::string> parts = split(line, ':');
            if (parts.size() >= 2) work = parts[0] + ":" + parts[1];
        }

        size_t schemeAt = work.find("://");
        if (schemeAt != std::string::npos) {
            std::string scheme = work.substr(0, schemeAt);
            std::string mapped;
            if (!mapScheme(scheme, mapped)) continue;  // socks4 and unknowns
            proto = mapped;
            work = work.substr(schemeAt + 3);
        }

        size_t colon = work.rfind(':');
        if (colon == std::string::npos) continue;
        std::string host = work.substr(0, colon);
        std::string portStr = work.substr(colon + 1);
        int port = 0;
        if (!validHostPort(host, portStr, port)) continue;
        addProxy(d, proto, host, port, s.url.empty() ? s.kind : s.url, prio);
        added++;
    }
}

}  // namespace

int runFetch(const Options& opt) {
    std::ifstream in("sources.txt");
    if (!in) {
        // Fall back to the copy shipped next to the binary's project dir.
        in.open("/tmp/mot-router/tools/ipfarm/sources.txt");
    }
    if (!in) {
        std::fprintf(stderr, "fetch: cannot open sources.txt\n");
        return 1;
    }

    std::vector<Source> sources;
    std::string line;
    while (std::getline(in, line)) {
        line = trim(line);
        if (line.empty() || line[0] == '#') continue;
        std::vector<std::string> parts;
        std::istringstream ls(line);
        std::string tok;
        while (ls >> tok) parts.push_back(tok);
        if (parts.size() < 3) continue;
        sources.push_back(Source{parts[0], parts[1], parts[2]});
    }

    std::string outDir = opt.work + "/out";
    if (!ensureDir(outDir)) {
        std::fprintf(stderr, "fetch: cannot create %s\n", outDir.c_str());
        return 1;
    }

    MultiRunner runner(32);
    std::vector<std::string> bodies(sources.size());
    for (size_t i = 0; i < sources.size(); i++) {
        Req req;
        req.url = sources[i].url;
        req.connect_timeout_ms = 10000;
        req.total_timeout_ms = 90000;
        req.headers.push_back("User-Agent: Mozilla/5.0 (compatible; ipfarm/1.0)");
        size_t idx = i;
        runner.submit(req, [&bodies, idx, &sources](Res res) {
            if (res.error.empty() && res.status >= 200 && res.status < 300) {
                bodies[idx] = std::move(res.body);
            } else if (!res.error.empty()) {
                std::fprintf(stderr, "fetch: %s failed: %s\n", sources[idx].url.c_str(),
                             res.error.c_str());
            } else {
                std::fprintf(stderr, "fetch: %s status %ld\n", sources[idx].url.c_str(),
                             res.status);
            }
        });
    }
    runner.run();

    Dedup dedup;
    for (size_t i = 0; i < sources.size(); i++) {
        if (bodies[i].empty()) continue;
        long added = 0;
        parseLines(sources[i], bodies[i], dedup, added);
        std::fprintf(stdout, "%-70s +%ld uniq=%zu\n", sources[i].url.c_str(), added,
                     dedup.keys.size());
    }

    std::string path = outDir + "/candidates.tsv";
    std::ofstream out(path);
    if (!out) {
        std::fprintf(stderr, "fetch: cannot write %s\n", path.c_str());
        return 1;
    }
    out << "proto\tip\tport\tsrc\tprio\n";
    std::unordered_map<std::string, long> perProto;
    std::unordered_set<std::string> uniqueIPs;
    for (const auto& key : dedup.keys) {
        const Proxy& p = dedup.items[key];
        out << p.proto << '\t' << p.host << '\t' << p.port << '\t' << p.src << '\t' << p.prio
            << '\n';
        perProto[p.proto]++;
        uniqueIPs.insert(p.host);
    }
    out.close();

    std::fprintf(stdout, "\ntotal uniq candidates: %zu\n", dedup.keys.size());
    for (const auto& kv : perProto) {
        std::fprintf(stdout, "  %s: %ld\n", kv.first.c_str(), kv.second);
    }
    std::fprintf(stdout, "unique IPs: %zu\n", uniqueIPs.size());
    std::fprintf(stdout, "written: %s\n", path.c_str());
    return 0;
}
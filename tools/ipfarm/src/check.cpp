#include "common.h"
#include "http.h"
#include "state.h"
#include "util.h"

#include <algorithm>
#include <atomic>
#include <cstdio>
#include <fstream>
#include <map>
#include <sstream>
#include <string>
#include <unordered_set>
#include <vector>

namespace {

// zenBody is the exact free-tier shape: five tool names (zen.RequiredTools),
// stream true, tiny output. Sent verbatim.
const char* kZenBody =
    "{\"model\":\"muse-spark-1.3-contributor-free\",\"stream\":true,\"store\":false,"
    "\"max_output_tokens\":16,"
    "\"input\":[{\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"hi\"}]}],"
    "\"tools\":["
    "{\"type\":\"function\",\"name\":\"bash\",\"description\":\"d\",\"parameters\":{\"type\":"
    "\"object\",\"properties\":{}}},"
    "{\"type\":\"function\",\"name\":\"edit\",\"description\":\"d\",\"parameters\":{\"type\":"
    "\"object\",\"properties\":{}}},"
    "{\"type\":\"function\",\"name\":\"glob\",\"description\":\"d\",\"parameters\":{\"type\":"
    "\"object\",\"properties\":{}}},"
    "{\"type\":\"function\",\"name\":\"grep\",\"description\":\"d\",\"parameters\":{\"type\":"
    "\"object\",\"properties\":{}}},"
    "{\"type\":\"function\",\"name\":\"read\",\"description\":\"d\",\"parameters\":{\"type\":"
    "\"object\",\"properties\":{}}}],"
    "\"tool_choice\":\"auto\"}";

std::vector<Proxy> readCandidates(const std::string& path) {
    std::vector<Proxy> out;
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
        if (f.size() < 5) continue;
        Proxy p;
        p.proto = f[0];
        p.host = f[1];
        p.port = std::atoi(f[2].c_str());
        p.src = f[3];
        p.prio = std::atoi(f[4].c_str());
        out.push_back(std::move(p));
    }
    return out;
}

}  // namespace

int runCheck(const Options& opt) {
    std::string outDir = opt.work + "/out";
    if (!ensureDir(outDir)) {
        std::fprintf(stderr, "check: cannot create %s\n", outDir.c_str());
        return 1;
    }

    std::vector<Proxy> candidates = readCandidates(outDir + "/candidates.tsv");
    if (candidates.empty()) {
        std::fprintf(stderr, "check: no candidates; run `fetch` first\n");
        return 1;
    }
    std::fprintf(stdout, "stage1: %zu candidates conc=%d\n", candidates.size(), opt.conc1);

    // Stage 1: cheap liveness through each proxy.
    //
    // A plain GET is used rather than CURLOPT_CONNECT_ONLY: measured on this
    // box (libcurl 7.68) a proxy that curl carries a real request through
    // reports "Timeout was reached" under CONNECT_ONLY, so the tunnel-only
    // probe produced false negatives. A GET that receives any HTTP status
    // below 500 proves TLS and the full request path work.
    std::vector<char> live(candidates.size(), 0);
    std::atomic<long> s1_ok{0}, s1_done{0};

    {
        MultiRunner runner(opt.conc1);
        for (size_t i = 0; i < candidates.size(); i++) {
            Req req;
            req.url = "https://opencode.ai/zen/v1/models";
            req.proxy = proxyURL(candidates[i].proto, candidates[i].host, candidates[i].port);
            req.connect_timeout_ms = 5000;
            req.total_timeout_ms = 12000;
            size_t idx = i;
            runner.submit(req, [&, idx](Res res) {
                bool ok = res.error.empty() && res.status > 0 && res.status < 500;
                live[idx] = ok ? 1 : 0;
                if (ok) s1_ok++;
                if (++s1_done % 10000 == 0) {
                    std::fprintf(stdout, "  stage1 %ld/%zu ok=%ld\n", s1_done.load(),
                                 candidates.size(), s1_ok.load());
                }
            });
        }
        runner.run();
    }

    std::vector<size_t> liveIdx;
    for (size_t i = 0; i < candidates.size(); i++) {
        if (live[i]) liveIdx.push_back(i);
    }
    std::fprintf(stdout, "stage1 live: %zu\n", liveIdx.size());
    if (liveIdx.empty()) {
        std::fprintf(stderr, "check: no live proxies\n");
        return 1;
    }

    // Stage 2: real Zen request through each live proxy.
    std::fprintf(stdout, "stage2: %zu live conc=%d\n", liveIdx.size(), opt.conc2);
    std::vector<Proxy> zenOk;
    std::map<long, long> histogram;
    std::atomic<long> s2_done{0};

    {
        MultiRunner runner(opt.conc2);
        for (size_t li : liveIdx) {
            const Proxy& p = candidates[li];
            Req req;
            req.method = "POST";
            req.url = "https://opencode.ai/zen/v1/responses";
            req.proxy = proxyURL(p.proto, p.host, p.port);
            req.body = kZenBody;
            req.headers.push_back("User-Agent: opencode/1.18.31");
            req.headers.push_back("Accept: text/event-stream");
            req.headers.push_back("x-opencode-client: cli");
            req.headers.push_back("x-opencode-project: global");
            // The gate shape-checks the session: `ses_` + 12 lowercase hex +
            // 14 alnum (zen.SessionID). A fixed well-formed value is enough;
            // the value only pins routing, it is not a credential.
            req.headers.push_back("x-opencode-session: ses_0123456789abcdef0123456789");
            req.connect_timeout_ms = 6000;
            req.total_timeout_ms = 15000;
            req.stop_after_bytes = 1;
            runner.submit(req, [&, li](Res res) {
                long bucket = res.status;
                if (bucket == 0 && !res.error.empty()) bucket = -1;
                histogram[bucket]++;
                if (res.status == 200 && res.first_byte) {
                    Proxy ok = candidates[li];
                    ok.zen_ms = res.ms;
                    zenOk.push_back(std::move(ok));
                }
                if (++s2_done % 500 == 0) {
                    std::fprintf(stdout, "  stage2 %ld/%zu ok=%zu\n", s2_done.load(),
                                 liveIdx.size(), zenOk.size());
                }
            });
        }
        runner.run();
    }

    std::fprintf(stdout, "stage2 histogram (status:count; -1=transport error):\n");
    for (const auto& kv : histogram) {
        std::fprintf(stdout, "  %ld: %ld\n", kv.first, kv.second);
    }
    std::fprintf(stdout, "zen_ok: %zu\n", zenOk.size());
    if (zenOk.empty()) {
        std::fprintf(stderr, "check: no proxy accepted by Zen\n");
        return 1;
    }

    // Exit IP per accepted proxy.
    std::fprintf(stdout, "exit-ip: %zu conc=500\n", zenOk.size());
    {
        MultiRunner runner(500);
        for (size_t i = 0; i < zenOk.size(); i++) {
            Req req;
            req.url = "https://api.ipify.org";
            req.proxy = proxyURL(zenOk[i].proto, zenOk[i].host, zenOk[i].port);
            req.connect_timeout_ms = 5000;
            req.total_timeout_ms = 8000;
            size_t idx = i;
            runner.submit(req, [&, idx](Res res) {
                if (res.error.empty() && res.status == 200) {
                    zenOk[idx].exit_ip = trim(res.body);
                }
            });
        }
        runner.run();
    }

    std::ofstream out(outDir + "/zen_ok.tsv");
    out << "proto\thost\tport\tsrc\tprio\tzen_ms\texit_ip\n";
    long withIP = 0;
    for (const auto& p : zenOk) {
        out << p.proto << '\t' << p.host << '\t' << p.port << '\t' << p.src << '\t' << p.prio
            << '\t' << p.zen_ms << '\t' << p.exit_ip << '\n';
        if (!p.exit_ip.empty()) withIP++;
    }
    out.close();

    // state.tsv is keyed by host:port at this stage (final node names are only
    // assigned by emit). Prior counters are carried over; a node that used to
    // be accepted but is not now increments fail, which makes emit drop it.
    std::unordered_set<std::string> accepted;
    for (const auto& p : zenOk) accepted.insert(p.host + ":" + std::to_string(p.port));

    State prior;
    prior.load(opt.work + "/state.tsv");

    std::ofstream st(opt.work + "/state.tsv");
    st << "name\tproto\thost\tport\texit_ip\tzen_ms\tok\tfail\tpenalized_until\n";
    for (const auto& p : zenOk) {
        std::string hp = p.host + ":" + std::to_string(p.port);
        long ok = 0, fail = 0, pen = 0;
        if (const StateNode* old = prior.find(hp)) {
            ok = old->ok;
            fail = old->fail;
            pen = old->penalized_until;
        }
        st << hp << '\t' << p.proto << '\t' << p.host << '\t' << p.port << '\t' << p.exit_ip
           << '\t' << p.zen_ms << '\t' << ok << '\t' << fail << '\t' << pen << '\n';
    }
    long demoted = 0;
    for (const auto& old : prior.nodes) {
        if (accepted.count(old.name)) continue;
        demoted++;
        st << old.name << '\t' << old.proto << '\t' << old.host << '\t' << old.port << '\t'
           << old.exit_ip << '\t' << old.zen_ms << '\t' << old.ok << '\t' << (old.fail + 1) << '\t'
           << old.penalized_until << '\n';
    }
    st.close();

    std::fprintf(stdout, "wrote %s (%zu rows, %ld with exit_ip)\n",
                 (outDir + "/zen_ok.tsv").c_str(), zenOk.size(), withIP);
    std::fprintf(stdout, "wrote %s (%zu accepted, %ld demoted)\n", (opt.work + "/state.tsv").c_str(),
                 zenOk.size(), demoted);
    return 0;
}
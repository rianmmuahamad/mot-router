#include "common.h"

#include <csignal>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>

namespace {

const char* envOr(const char* key, const char* fallback) {
    const char* v = std::getenv(key);
    if (v == nullptr || *v == '\0') return fallback;
    return v;
}

void usage() {
    std::fprintf(stderr,
                 "usage: ipfarm <fetch|check|emit|rotate|run|stats> [flags]\n"
                 "  --work <dir>           default /tmp/ipfarm\n"
                 "  --mihomo-home <dir>    default /tmp/mihomo-farm\n"
                 "  --mihomo <url>         default http://127.0.0.1:9090\n"
                 "  --mihomo-secret <s>    default $MIHOMO_SECRET\n"
                 "  --router <url>         default http://127.0.0.1:8787\n"
                 "  --group <name>         default MOT\n"
                 "  --conc1 <n>            default 1000\n"
                 "  --conc2 <n>            default 300\n"
                 "  --max-total <n>        default 4000\n"
                 "  --shard <n>            default 2000\n"
                 "  --interval <sec>       default 1800\n");
}

}  // namespace

int main(int argc, char** argv) {
    // Free proxies frequently reset the connection mid-request. Writes to a
    // closed socket must return an error to libcurl, not kill the process.
    std::signal(SIGPIPE, SIG_IGN);

    if (argc < 2) {
        usage();
        return 2;
    }
    std::string cmd = argv[1];
    if (cmd == "-h" || cmd == "--help") {
        usage();
        return 0;
    }

    Options opt;
    opt.mihomo_secret = envOr("MIHOMO_SECRET", "");

    for (int i = 2; i < argc; i++) {
        std::string a = argv[i];
        auto next = [&](const char* name) -> std::string {
            if (i + 1 >= argc) {
                std::fprintf(stderr, "missing value for %s\n", name);
                std::exit(2);
            }
            return argv[++i];
        };
        if (a == "--work") {
            opt.work = next("--work");
        } else if (a == "--mihomo-home") {
            opt.mihomo_home = next("--mihomo-home");
        } else if (a == "--mihomo") {
            opt.mihomo = next("--mihomo");
        } else if (a == "--mihomo-secret") {
            opt.mihomo_secret = next("--mihomo-secret");
        } else if (a == "--router") {
            opt.router = next("--router");
        } else if (a == "--group") {
            opt.group = next("--group");
        } else if (a == "--conc1") {
            opt.conc1 = std::atoi(next("--conc1").c_str());
        } else if (a == "--conc2") {
            opt.conc2 = std::atoi(next("--conc2").c_str());
        } else if (a == "--max-total") {
            opt.max_total = std::atoi(next("--max-total").c_str());
        } else if (a == "--shard") {
            opt.shard = std::atoi(next("--shard").c_str());
        } else if (a == "--interval") {
            opt.interval = std::atoi(next("--interval").c_str());
        } else {
            std::fprintf(stderr, "unknown flag: %s\n", a.c_str());
            usage();
            return 2;
        }
    }
    if (opt.conc1 < 1) opt.conc1 = 1;
    if (opt.conc2 < 1) opt.conc2 = 1;
    if (opt.shard < 1) opt.shard = 1;

    if (cmd == "fetch") return runFetch(opt);
    if (cmd == "check") return runCheck(opt);
    if (cmd == "emit") return runEmit(opt);
    if (cmd == "rotate") return runRotate(opt);
    if (cmd == "run") return runRun(opt);
    if (cmd == "stats") return runStats(opt);

    std::fprintf(stderr, "unknown subcommand: %s\n", cmd.c_str());
    usage();
    return 2;
}
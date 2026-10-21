#include "common.h"
#include "ctl.h"
#include "util.h"

#include <cstdio>
#include <fstream>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

namespace {

std::vector<std::string> readIndex(const std::string& path) {
    std::vector<std::string> names;
    std::ifstream in(path);
    std::string line;
    while (std::getline(in, line)) {
        line = trim(line);
        if (!line.empty()) names.push_back(line);
    }
    return names;
}

// writeConfig renders mihomo-config.tmpl with the provider blocks and group
// `use:` list derived from the emitted shards.
bool writeConfig(const Options& opt, const std::vector<std::string>& providers) {
    std::string tmplPath = "mihomo-config.tmpl";
    std::ifstream tmpl(tmplPath);
    if (!tmpl) {
        tmpl.open("/tmp/mot-router/tools/ipfarm/mihomo-config.tmpl");
    }
    if (!tmpl) {
        std::fprintf(stderr, "run: cannot open mihomo-config.tmpl\n");
        return false;
    }
    std::stringstream buf;
    buf << tmpl.rdbuf();
    std::string content = buf.str();

    std::string blocks;
    std::string useList;
    for (size_t i = 0; i < providers.size(); i++) {
        const std::string& name = providers[i];
        blocks += "  " + name + ":\n";
        blocks += "    type: file\n";
        blocks += "    path: ./providers/" + name + ".yaml\n";
        blocks += "    health-check:\n";
        blocks += "      enable: true\n";
        blocks += "      url: https://www.gstatic.com/generate_204\n";
        blocks += "      interval: 300\n";
        blocks += "      timeout: 3000\n";
        blocks += "      lazy: true\n";
        blocks += "      expected-status: 204\n";
        if (i > 0) useList += ", ";
        useList += name;
    }

    auto replaceAll = [](std::string s, const std::string& from, const std::string& to) {
        size_t p = 0;
        while ((p = s.find(from, p)) != std::string::npos) {
            s.replace(p, from.size(), to);
            p += to.size();
        }
        return s;
    };
    content = replaceAll(content, "{{PROVIDERS}}", blocks);
    content = replaceAll(content, "{{GROUP_USE}}", useList);

    if (!ensureDir(opt.mihomo_home)) return false;
    std::ofstream out(opt.mihomo_home + "/config.yaml");
    if (!out) {
        std::fprintf(stderr, "run: cannot write %s/config.yaml\n", opt.mihomo_home.c_str());
        return false;
    }
    out << content;
    return true;
}

}  // namespace

int runRun(const Options& opt) {
    std::fprintf(stdout, "== ipfarm run (interval=%ds) ==\n", opt.interval);
    if (runFetch(opt) != 0) return 1;
    if (runCheck(opt) != 0) return 1;
    if (runEmit(opt) != 0) return 1;

    std::vector<std::string> providers = readIndex(opt.mihomo_home + "/providers/index.txt");
    if (providers.empty()) {
        std::fprintf(stderr, "run: no providers emitted\n");
        return 1;
    }
    if (!writeConfig(opt, providers)) return 1;
    std::fprintf(stdout, "wrote %s/config.yaml (%zu providers)\n", opt.mihomo_home.c_str(),
                 providers.size());

    Ctl ctl(opt.mihomo, opt.mihomo_secret, opt.router, opt.group);
    ctl.reloadConfig(opt.mihomo_home + "/config.yaml");
    ctl.reloadProviders(providers);

    std::string err;
    long status = ctl.putRouter(
        "/api/mihomo",
        "{\"enabled\":true,\"controller\":\"" + opt.mihomo +
            "\",\"proxy\":\"http://127.0.0.1:7890\",\"group\":\"" + opt.group +
            "\",\"rotate_on_limit\":true}",
        err);
    std::fprintf(stdout, "wired mot-router /api/mihomo -> %ld%s%s\n", status,
                 err.empty() ? "" : " err=", err.c_str());

    if (opt.interval > 0) {
        std::fprintf(stdout, "sleeping %ds before next cycle\n", opt.interval);
        std::this_thread::sleep_for(std::chrono::seconds(opt.interval));
    }
    return 0;
}
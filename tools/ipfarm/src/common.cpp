#include "common.h"

#include <sys/stat.h>
#include <sys/types.h>

#include <cstdio>

std::string proxyURL(const std::string& proto, const std::string& host, int port) {
    return proto + "://" + host + ":" + std::to_string(port);
}

bool ensureDir(const std::string& path) {
    if (path.empty()) return false;
    std::string cur;
    for (size_t i = 0; i < path.size(); i++) {
        cur.push_back(path[i]);
        if (path[i] == '/' || i + 1 == path.size()) {
            if (cur == "/" || cur.empty()) continue;
            std::string dir = cur;
            if (dir.back() == '/') dir.pop_back();
            if (dir.empty()) continue;
            if (mkdir(dir.c_str(), 0755) != 0) {
                struct stat st{};
                if (stat(dir.c_str(), &st) != 0 || !S_ISDIR(st.st_mode)) return false;
            }
        }
    }
    return true;
}
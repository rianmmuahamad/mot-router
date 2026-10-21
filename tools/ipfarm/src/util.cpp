#include "util.h"

#include <cctype>

std::string trim(const std::string& s) {
    size_t b = 0, e = s.size();
    while (b < e && std::isspace(static_cast<unsigned char>(s[b]))) b++;
    while (e > b && std::isspace(static_cast<unsigned char>(s[e - 1]))) e--;
    return s.substr(b, e - b);
}

std::string stripCR(const std::string& s) {
    size_t b = 0, e = s.size();
    auto isSpaceNoTab = [](unsigned char c) { return std::isspace(c) && c != '\t'; };
    while (b < e && isSpaceNoTab(static_cast<unsigned char>(s[b]))) b++;
    while (e > b && isSpaceNoTab(static_cast<unsigned char>(s[e - 1]))) e--;
    return s.substr(b, e - b);
}

std::vector<std::string> split(const std::string& s, char delim) {
    std::vector<std::string> out;
    size_t start = 0;
    for (size_t i = 0; i <= s.size(); i++) {
        if (i == s.size() || s[i] == delim) {
            out.push_back(s.substr(start, i - start));
            start = i + 1;
        }
    }
    return out;
}

bool jsonFindString(const std::string& body, const std::string& key, std::string& out) {
    const std::string pat = "\"" + key + "\"";
    size_t p = body.find(pat);
    if (p == std::string::npos) return false;
    size_t c = body.find(':', p + pat.size());
    if (c == std::string::npos) return false;
    size_t q1 = body.find('"', c);
    if (q1 == std::string::npos) return false;
    size_t q2 = body.find('"', q1 + 1);
    if (q2 == std::string::npos) return false;
    out = body.substr(q1 + 1, q2 - q1 - 1);
    return true;
}

bool jsonFindGroupNow(const std::string& body, const std::string& group, std::string& out) {
    const std::string key = "\"" + group + "\":{";
    size_t p = body.find(key);
    if (p == std::string::npos) return false;
    size_t q = body.find("\"now\":", p);
    if (q == std::string::npos) return false;
    size_t q1 = body.find('"', q + 6);
    if (q1 == std::string::npos) return false;
    size_t q2 = body.find('"', q1 + 1);
    if (q2 == std::string::npos) return false;
    out = body.substr(q1 + 1, q2 - q1 - 1);
    return true;
}

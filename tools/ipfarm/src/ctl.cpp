#include "ctl.h"
#include "http.h"
#include "util.h"

#include <cstdio>

Ctl::Ctl(std::string mihomoBase, std::string secret, std::string routerBase, std::string group)
    : mihomo_(std::move(mihomoBase)),
      secret_(std::move(secret)),
      router_(std::move(routerBase)),
      group_(std::move(group)) {
    while (!mihomo_.empty() && mihomo_.back() == '/') mihomo_.pop_back();
    while (!router_.empty() && router_.back() == '/') router_.pop_back();
}

std::string Ctl::request(const std::string& method, const std::string& base,
                         const std::string& path, const std::string& jsonBody, long& status,
                         std::string& err) {
    MultiRunner runner(1);
    Req req;
    req.method = method;
    req.url = base + path;
    req.body = jsonBody;
    if (!jsonBody.empty()) {
        req.headers.push_back("Content-Type: application/json");
    }
    if (!secret_.empty() && base == mihomo_) {
        req.headers.push_back("Authorization: Bearer " + secret_);
    }
    req.connect_timeout_ms = 5000;
    req.total_timeout_ms = 30000;

    std::string body;
    status = 0;
    err.clear();
    runner.submit(req, [&](Res res) {
        body = std::move(res.body);
        status = res.status;
        err = res.error;
    });
    runner.run();
    return body;
}

std::string Ctl::get(const std::string& path, long& status, std::string& err) {
    return request("GET", mihomo_, path, "", status, err);
}

std::string Ctl::put(const std::string& path, const std::string& jsonBody, long& status,
                     std::string& err) {
    return request("PUT", mihomo_, path, jsonBody, status, err);
}

std::string Ctl::post(const std::string& path, const std::string& jsonBody, long& status,
                      std::string& err) {
    return request("POST", mihomo_, path, jsonBody, status, err);
}

void Ctl::reloadProviders(const std::vector<std::string>& names) {
    for (const auto& name : names) {
        long status = 0;
        std::string err;
        put("/providers/proxies/" + name, "", status, err);
        std::fprintf(stdout, "reload provider %s -> %ld%s%s\n", name.c_str(), status,
                     err.empty() ? "" : " err=", err.c_str());
        std::string ignored = get("/providers/proxies/" + name + "/healthcheck", status, err);
        std::fprintf(stdout, "healthcheck %s -> %ld\n", name.c_str(), status);
    }
}

std::string Ctl::currentNode() {
    long status = 0;
    std::string err;
    std::string body = get("/proxies", status, err);
    if (status != 200) return "";
    std::string now;
    if (!jsonFindGroupNow(body, group_, now)) return "";
    return now;
}

bool Ctl::selectNode(const std::string& name, std::string& err) {
    long status = 0;
    std::string body = put("/proxies/" + group_, "{\"name\":\"" + name + "\"}", status, err);
    (void)body;
    if (!err.empty()) return false;
    if (status != 204 && status != 200) {
        err = "unexpected status " + std::to_string(status);
        return false;
    }
    return true;
}

bool Ctl::reloadConfig(const std::string& path) {
    long status = 0;
    std::string err;
    std::string body = put("/configs?force=true", "{\"path\":\"" + path + "\"}", status, err);
    (void)body;
    std::fprintf(stdout, "reload config -> %ld%s%s\n", status, err.empty() ? "" : " err=",
                 err.c_str());
    return err.empty() && (status == 204 || status == 200);
}

long Ctl::putRouter(const std::string& path, const std::string& jsonBody, std::string& err) {
    long status = 0;
    request("PUT", router_, path, jsonBody, status, err);
    return status;
}
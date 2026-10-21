#pragma once

#include "common.h"

#include <string>
#include <vector>

// Ctl talks to the mihomo control API and the mot-router dashboard API.
class Ctl {
  public:
    Ctl(std::string mihomoBase, std::string secret, std::string routerBase, std::string group);

    // get/put/post return the response body and set status/err.
    std::string get(const std::string& path, long& status, std::string& err);
    std::string put(const std::string& path, const std::string& jsonBody, long& status,
                    std::string& err);
    std::string post(const std::string& path, const std::string& jsonBody, long& status,
                     std::string& err);

    // reloadProviders asks mihomo to re-read each named provider, then triggers
    // a health check so node delays become visible to mot-router.
    void reloadProviders(const std::vector<std::string>& names);

    // currentNode returns the node the group currently uses, or "".
    std::string currentNode();

    // selectNode switches the group's node.
    bool selectNode(const std::string& name, std::string& err);

    // reloadConfig asks mihomo to reload its configuration file.
    bool reloadConfig(const std::string& path);

    // putRouter sends a PUT to the mot-router dashboard API.
    long putRouter(const std::string& path, const std::string& jsonBody, std::string& err);

    const std::string& group() const { return group_; }
    const std::string& mihomoBase() const { return mihomo_; }

  private:
    std::string request(const std::string& method, const std::string& base,
                        const std::string& path, const std::string& jsonBody, long& status,
                        std::string& err);

    std::string mihomo_;
    std::string secret_;
    std::string router_;
    std::string group_;
};
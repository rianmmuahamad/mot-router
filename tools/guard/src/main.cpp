#include "limit.h"
#include "server.h"

#include <curl/curl.h>

#include <arpa/inet.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <sys/socket.h>
#include <unistd.h>

#include <csignal>
#include <algorithm>
#include <condition_variable>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

namespace {

const char* envOr(const char* key, const char* fallback) {
    const char* v = std::getenv(key);
    if (v == nullptr || *v == '\0') return fallback;
    return v;
}

void usage() {
    std::fprintf(stderr,
                 "usage: guard [flags]\n"
                 "  --listen <host:port>       default 127.0.0.1:8791\n"
                 "  --upstream <url>           default http://127.0.0.1:8787\n"
                 "  --master-key <mr_...>      required (env GUARD_MASTER_KEY)\n"
                 "  --max-inflight <n>         default 64\n"
                 "  --per-user-inflight <n>    default 4\n"
                 "  --global-rps <n>           default 40\n"
                 "  --user-rps <n>             default 8\n"
                 "  --queue-max <n>            default 512\n"
                 "  --queue-wait-ms <n>        per-user wait, default 60000 (0 disables)\n"
                 "  --per-user-queue <n>       waiting requests per user, default 8\n"
                 "  --retries <n>              default 2\n"
                 "  --retry-backoff-ms <n>     default 300\n");
}

}  // namespace

int main(int argc, char** argv) {
    // A client that disconnects mid-stream must not kill the process.
    std::signal(SIGPIPE, SIG_IGN);

    Options opt;
    opt.listen = envOr("GUARD_LISTEN", opt.listen.c_str());
    opt.master_key = envOr("GUARD_MASTER_KEY", "");

    for (int i = 1; i < argc; i++) {
        std::string a = argv[i];
        auto next = [&](const char* name) -> std::string {
            if (i + 1 >= argc) {
                std::fprintf(stderr, "missing value for %s\n", name);
                std::exit(2);
            }
            return argv[++i];
        };
        if (a == "--listen") opt.listen = next("--listen");
        else if (a == "--upstream") opt.upstream = next("--upstream");
        else if (a == "--master-key") opt.master_key = next("--master-key");
        else if (a == "--max-inflight") opt.max_inflight = std::atoi(next("--max-inflight").c_str());
        else if (a == "--per-user-inflight") opt.per_user_inflight = std::atoi(next("--per-user-inflight").c_str());
        else if (a == "--global-rps") opt.global_rps = std::atof(next("--global-rps").c_str());
        else if (a == "--user-rps") opt.user_rps = std::atof(next("--user-rps").c_str());
        else if (a == "--queue-max") opt.queue_max = std::atoi(next("--queue-max").c_str());
        else if (a == "--queue-wait-ms") opt.queue_wait_ms = std::atoi(next("--queue-wait-ms").c_str());
        else if (a == "--per-user-queue") opt.per_user_queue = std::atoi(next("--per-user-queue").c_str());
        else if (a == "--retries") opt.retries = std::atoi(next("--retries").c_str());
        else if (a == "--retry-backoff-ms") opt.retry_backoff_ms = std::atoi(next("--retry-backoff-ms").c_str());
        else if (a == "-h" || a == "--help") { usage(); return 0; }
        else { std::fprintf(stderr, "unknown flag: %s\n", a.c_str()); usage(); return 2; }
    }

    if (opt.master_key.empty()) {
        std::fprintf(stderr, "guard: --master-key is required (env GUARD_MASTER_KEY).\n"
                             "Create one with: curl -s -X POST http://127.0.0.1:8787/api/keys "
                             "-H 'Content-Type: application/json' -d '{\"name\":\"motgate\"}'\n");
        return 2;
    }
    if (opt.max_inflight < 1) opt.max_inflight = 1;
    if (opt.per_user_inflight < 1) opt.per_user_inflight = 1;
    opt.queue_wait_ms = std::clamp(opt.queue_wait_ms, 0, 120000);
    opt.per_user_queue = std::clamp(opt.per_user_queue, 0, 32);

    // Listen socket.
    std::string host = "127.0.0.1";
    std::string port = "8791";
    size_t colon = opt.listen.rfind(':');
    if (colon != std::string::npos) {
        host = opt.listen.substr(0, colon);
        port = opt.listen.substr(colon + 1);
    }
    if (host.empty()) host = "127.0.0.1";

    int listen_fd = ::socket(AF_INET, SOCK_STREAM, 0);
    if (listen_fd < 0) {
        std::perror("socket");
        return 1;
    }
    int one = 1;
    ::setsockopt(listen_fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));

    sockaddr_in addr{};
    addr.sin_family = AF_INET;
    addr.sin_port = htons(static_cast<uint16_t>(std::atoi(port.c_str())));
    if (::inet_pton(AF_INET, host.c_str(), &addr.sin_addr) != 1) {
        std::fprintf(stderr, "guard: invalid listen host %s\n", host.c_str());
        return 1;
    }
    if (::bind(listen_fd, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)) != 0) {
        std::perror("bind");
        return 1;
    }
    if (::listen(listen_fd, opt.queue_max) != 0) {
        std::perror("listen");
        return 1;
    }

    std::fprintf(stdout,
                 "guard listening on %s:%s upstream=%s max_inflight=%d per_user=%d "
                 "global_rps=%.1f user_rps=%.1f\n",
                 host.c_str(), port.c_str(), opt.upstream.c_str(), opt.max_inflight,
                 opt.per_user_inflight, opt.global_rps, opt.user_rps);
    std::fflush(stdout);

    curl_global_init(CURL_GLOBAL_DEFAULT);

    Stats stats;
    Limiter limiter(opt, &stats);

    // Fixed worker pool: the number of in-flight upstream requests can never
    // exceed max_inflight because each worker handles exactly one connection.
    std::vector<std::thread> workers;
    workers.reserve(static_cast<size_t>(opt.max_inflight));
    for (int i = 0; i < opt.max_inflight; i++) {
        workers.emplace_back([&]() {
            while (true) {
                sockaddr_in peer{};
                socklen_t plen = sizeof(peer);
                int fd = ::accept(listen_fd, reinterpret_cast<sockaddr*>(&peer), &plen);
                if (fd < 0) {
                    if (errno == EINTR) continue;
                    break;
                }
                int flag = 1;
                ::setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &flag, sizeof(flag));
                handleConnection(fd, opt, limiter, stats);
            }
        });
    }
    for (auto& t : workers) t.join();

    ::close(listen_fd);
    curl_global_cleanup();
    return 0;
}

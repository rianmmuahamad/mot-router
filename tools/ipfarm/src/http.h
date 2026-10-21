#pragma once

#include <curl/curl.h>

#include <chrono>
#include <cstdint>
#include <deque>
#include <functional>
#include <memory>
#include <string>
#include <utility>
#include <vector>

struct Req {
    std::string method = "GET";
    std::string url;
    std::string body;
    std::vector<std::string> headers;
    long connect_timeout_ms = 5000;
    long total_timeout_ms = 15000;
    // proxy is the outbound proxy URL, e.g. "http://1.2.3.4:8080" or
    // "socks5://1.2.3.4:1080". Empty routes directly.
    std::string proxy;
    bool connect_only = false;
    // stop_after_bytes aborts the transfer once that many body bytes arrived.
    // Used to prove a stream started without reading a whole generation.
    size_t stop_after_bytes = 0;
};

struct Res {
    long status = 0;
    long connectcode = 0;
    std::string body;
    std::string error;
    bool first_byte = false;
    int64_t ms = 0;
};

// MultiRunner drives many transfers through one libcurl multi handle with a
// fixed in-flight ceiling. submit() queues work; run() drains the queue.
class MultiRunner {
  public:
    struct Slot;

    explicit MultiRunner(int max_inflight);
    ~MultiRunner();

    MultiRunner(const MultiRunner&) = delete;
    MultiRunner& operator=(const MultiRunner&) = delete;

    void submit(const Req& req, std::function<void(Res)> done);
    void run();

  private:
    static size_t writeCb(char* ptr, size_t size, size_t nmemb, void* userp);

    void fill();
    void reap();

    CURLM* multi_ = nullptr;
    int max_inflight_ = 0;
    std::vector<std::unique_ptr<Slot>> slots_;
    std::deque<std::pair<Req, std::function<void(Res)>>> pending_;
};

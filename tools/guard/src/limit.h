#pragma once

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <deque>
#include <functional>
#include <mutex>
#include <string>
#include <unordered_map>

// Options is the parsed configuration.
struct Options {
    std::string listen = "127.0.0.1:8791";
    std::string upstream = "http://127.0.0.1:8787";
    std::string master_key;
    int max_inflight = 64;
    int per_user_inflight = 4;
    double global_rps = 40;
    double user_rps = 8;
    int queue_max = 512;
    int queue_wait_ms = 60000;
    int per_user_queue = 8;
    int retries = 2;
    int retry_backoff_ms = 300;
};

// Stats is the live admission picture, exposed by /healthz.
struct Stats {
    std::atomic<long> inflight{0};
    std::atomic<long> queued{0};
    std::atomic<long> rejected{0};
    std::atomic<long> ok{0};
    std::atomic<long> upstream_errors{0};
    std::atomic<long> waited{0};
    std::atomic<long> queue_timeouts{0};
};

// TokenBucket is a continuous-refill bucket; one instance per scope.
class TokenBucket {
  public:
    TokenBucket() = default;
    explicit TokenBucket(double rate)
        : rate_(rate), tokens_(rate), last_(std::chrono::steady_clock::now()) {}

    TokenBucket(TokenBucket&& other) noexcept {
        std::lock_guard<std::mutex> lock(other.mu_);
        rate_ = other.rate_;
        tokens_ = other.tokens_;
        last_ = other.last_;
    }
    TokenBucket& operator=(TokenBucket&& other) noexcept {
        if (this != &other) {
            std::lock_guard<std::mutex> lock(other.mu_);
            rate_ = other.rate_;
            tokens_ = other.tokens_;
            last_ = other.last_;
        }
        return *this;
    }
    TokenBucket(const TokenBucket&) = delete;
    TokenBucket& operator=(const TokenBucket&) = delete;

    // take returns true when a token was available.
    bool take() {
        std::lock_guard<std::mutex> lock(mu_);
        auto now = std::chrono::steady_clock::now();
        double elapsed = std::chrono::duration<double>(now - last_).count();
        last_ = now;
        tokens_ += elapsed * rate_;
        if (tokens_ > rate_) tokens_ = rate_;
        if (tokens_ < 1.0) return false;
        tokens_ -= 1.0;
        return true;
    }

  private:
    std::mutex mu_;
    double rate_ = 0;
    double tokens_ = 0;
    std::chrono::steady_clock::time_point last_ = std::chrono::steady_clock::now();
};

// Limiter enforces concurrency and rate limits, per user and globally.
class Limiter {
  public:
    explicit Limiter(const Options& opt, Stats* stats = nullptr)
        : per_user_(opt.per_user_inflight),
          wait_ms_(opt.queue_wait_ms),
          per_user_queue_(opt.per_user_queue),
          max_queued_(opt.max_inflight / 2),
          stats_(stats),
          user_rps_(opt.user_rps),
          global_rps_(opt.global_rps) {}

    Limiter(const Limiter&) = delete;
    Limiter& operator=(const Limiter&) = delete;

    // acquire reserves a slot for the user. Returns 0 on success, otherwise the
    // HTTP status to answer with (429) and a message.
    int acquire(const std::string& user, std::string& message, size_t body_bytes = 0,
                const std::function<bool()>& cancelled = {});
    void release(const std::string& user);

  private:
    std::mutex mu_;
    int per_user_;
    int wait_ms_;
    int per_user_queue_;
    int max_queued_;
    Stats* stats_;
    std::condition_variable ready_;
    uint64_t next_ticket_ = 0;
    int queued_ = 0;
    size_t queued_bytes_ = 0;
    static constexpr size_t max_queued_bytes_ = 16 * 1024 * 1024;
    std::unordered_map<std::string, std::deque<uint64_t>> waiting_;
    std::unordered_map<std::string, int> inflight_;
    std::unordered_map<std::string, TokenBucket> user_buckets_;
    double user_rps_;
    TokenBucket global_rps_;
};

#include "limit.h"
#include <algorithm>

int Limiter::acquire(const std::string& user, std::string& message, size_t body_bytes,
                     const std::function<bool()>& cancelled) {
    if (user.empty()) {
        message = "missing X-Motgate-User";
        return 400;
    }

    std::unique_lock<std::mutex> lock(mu_);
    auto active = [&]() {
        auto it = inflight_.find(user);
        return it == inflight_.end() ? 0 : it->second;
    };
    bool waited = false;
    if (active() >= per_user_ || waiting_.count(user)) {
        auto it = waiting_.find(user);
        size_t user_waiting = it == waiting_.end() ? 0 : it->second.size();
        if (wait_ms_ <= 0 || user_waiting >= static_cast<size_t>(per_user_queue_) ||
            queued_ >= max_queued_ || body_bytes > max_queued_bytes_ - queued_bytes_) {
            message = "Your request queue is full. Please retry shortly.";
            return 429;
        }
        const auto ticket = next_ticket_++;
        waiting_[user].push_back(ticket);
        queued_++;
        queued_bytes_ += body_bytes;
        if (stats_) stats_->queued++;
        auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(wait_ms_);
        bool admitted = false, disconnected = false;
        while (true) {
            disconnected = cancelled && cancelled();
            if (disconnected) break;
            if (active() < per_user_ && waiting_.at(user).front() == ticket) {
                admitted = true;
                break;
            }
            auto now = std::chrono::steady_clock::now();
            if (now >= deadline) break;
            ready_.wait_until(lock, std::min(deadline, now + std::chrono::milliseconds(100)));
        }
        auto& queue = waiting_.at(user);
        queue.erase(std::find(queue.begin(), queue.end(), ticket));
        if (queue.empty()) waiting_.erase(user);
        queued_--;
        queued_bytes_ -= body_bytes;
        if (stats_) stats_->queued--;
        ready_.notify_all();
        if (disconnected) return 499;
        if (!admitted) {
            if (stats_) stats_->queue_timeouts++;
            message = "Your earlier requests are still running. Please retry shortly.";
            return 429;
        }
        waited = true;
    }

    if (cancelled && cancelled()) return 499;

    // Per-user rate: create the bucket on first sight so each user is
    // independent. The bucket is left in the map; user count is bounded by the
    // admin-created set, so this cannot grow without bound.
    auto it = user_buckets_.find(user);
    if (it == user_buckets_.end()) {
        it = user_buckets_.emplace(user, TokenBucket(user_rps_)).first;
    }
    if (!it->second.take()) {
        message = "per-user rate limit";
        return 429;
    }
    if (!global_rps_.take()) {
        message = "global rate limit";
        return 429;
    }

    inflight_[user]++;
    if (waited && stats_) stats_->waited++;
    return 0;
}

void Limiter::release(const std::string& user) {
    std::lock_guard<std::mutex> lock(mu_);
    auto it = inflight_.find(user);
    if (it == inflight_.end()) return;
    if (--it->second <= 0) inflight_.erase(it);
    ready_.notify_all();
}

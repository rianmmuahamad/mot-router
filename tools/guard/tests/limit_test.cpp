#include "../src/limit.h"
#include <cassert>
#include <future>
#include <thread>
#include <iostream>
using namespace std::chrono_literals;

static Options options() {
    Options o;
    o.max_inflight = 8;
    o.per_user_inflight = 1;
    o.per_user_queue = 2;
    o.queue_wait_ms = 500;
    o.user_rps = 1000;
    o.global_rps = 1000;
    return o;
}
static void waitQueued(Stats& s, long count) {
    auto until = std::chrono::steady_clock::now() + 1s;
    while (s.queued.load() != count && std::chrono::steady_clock::now() < until)
        std::this_thread::sleep_for(1ms);
    assert(s.queued.load() == count);
}
int main() {
    {
        auto o = options(); Stats stats; Limiter l(o, &stats); std::string msg;
        assert(l.acquire("a", msg) == 0);
        auto first = std::async(std::launch::async, [&] {std::string m; return l.acquire("a",m);});
        waitQueued(stats,1);
        auto second = std::async(std::launch::async, [&] {std::string m; return l.acquire("a",m);});
        waitQueued(stats,2);
        // Another user can enter while this user's FIFO queue waits.
        assert(l.acquire("b",msg) == 0); l.release("b");
        assert(l.acquire("a",msg) == 429); // bounded per-user queue
        l.release("a");
        assert(first.get() == 0);
        assert(second.wait_for(20ms) == std::future_status::timeout);
        l.release("a");
        assert(second.get() == 0); l.release("a");
        assert(stats.queued == 0 && stats.waited == 2);
    }
    {
        auto o = options(); o.queue_wait_ms = 25;
        Stats stats; Limiter l(o, &stats); std::string msg;
        assert(l.acquire("a",msg) == 0);
        assert(l.acquire("a",msg) == 429);
        assert(stats.queued == 0 && stats.queue_timeouts == 1);
        l.release("a"); assert(l.acquire("a",msg) == 0); l.release("a");
    }
    {
        auto o = options(); Stats stats; Limiter l(o, &stats); std::string msg;
        assert(l.acquire("a",msg) == 0);
        // Large queued request bodies cannot accumulate beyond 16 MiB.
        assert(l.acquire("a",msg,17*1024*1024) == 429);
        std::atomic<bool> cancel{false};
        auto pending = std::async(std::launch::async,[&]{std::string m;return l.acquire("a",m,1,[&]{return cancel.load();});});
        waitQueued(stats,1);cancel=true;
        assert(pending.get() == 499);assert(stats.queued == 0);
        l.release("a");assert(l.acquire("a",msg) == 0);l.release("a");
    }
    {
        auto o = options(); o.max_inflight = 4; Stats stats; Limiter l(o,&stats);std::string msg;
        assert(l.acquire("a",msg) == 0); assert(l.acquire("b",msg) == 0);
        auto a=std::async(std::launch::async,[&]{std::string m;return l.acquire("a",m);});
        auto b=std::async(std::launch::async,[&]{std::string m;return l.acquire("b",m);});
        waitQueued(stats,2);
        assert(l.acquire("a",msg) == 429); // global waiting bound
        l.release("a");l.release("b");assert(a.get()==0 && b.get()==0);
        l.release("a");l.release("b");assert(stats.queued==0);
    }
    {
        auto o=options(); o.queue_wait_ms=0; Limiter l(o); std::string msg;
        assert(l.acquire("a",msg)==0);assert(l.acquire("a",msg)==429);l.release("a");
        assert(l.acquire("",msg)==400);
        o=options();o.user_rps=1;Limiter rate(o);
        assert(rate.acquire("a",msg)==0);rate.release("a");
        assert(rate.acquire("a",msg)==429);assert(msg=="per-user rate limit");
    }
    std::cout << "queue FIFO, isolation, bounds, timeout, cancellation, release and rate limits passed\n";
}

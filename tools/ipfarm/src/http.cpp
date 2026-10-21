#include "http.h"

#include <algorithm>
#include <cstring>

struct MultiRunner::Slot {
    CURL* easy = nullptr;
    Req req;
    Res res;
    std::function<void(Res)> done;
    struct curl_slist* hdrs = nullptr;
    size_t received = 0;
    bool aborting = false;
    std::chrono::steady_clock::time_point started;
    long connectcode = 0;
};

size_t MultiRunner::writeCb(char* ptr, size_t size, size_t nmemb, void* userp) {
    auto* slot = static_cast<Slot*>(userp);
    size_t n = size * nmemb;
    slot->res.first_byte = true;
    if (slot->req.stop_after_bytes > 0) {
        slot->received += n;
        if (slot->received >= slot->req.stop_after_bytes) {
            // Abort the transfer. Returning a value different from n makes
            // libcurl stop; the status line and headers are already parsed.
            slot->aborting = true;
            return 0;
        }
        return n;
    }
    slot->res.body.append(ptr, n);
    return n;
}

MultiRunner::MultiRunner(int max_inflight) : max_inflight_(max_inflight) {
    curl_global_init(CURL_GLOBAL_DEFAULT);
    multi_ = curl_multi_init();
    slots_.resize(max_inflight);
    for (auto& ptr : slots_) {
        ptr = std::make_unique<Slot>();
    }
}

MultiRunner::~MultiRunner() {
    for (auto& slot : slots_) {
        if (slot->easy) {
            curl_multi_remove_handle(multi_, slot->easy);
            curl_easy_cleanup(slot->easy);
        }
        if (slot->hdrs) curl_slist_free_all(slot->hdrs);
    }
    if (multi_) curl_multi_cleanup(multi_);
    curl_global_cleanup();
}

void MultiRunner::submit(const Req& req, std::function<void(Res)> done) {
    pending_.emplace_back(req, std::move(done));
}

void MultiRunner::fill() {
    while (!pending_.empty()) {
        Slot* free_slot = nullptr;
        for (auto& slot : slots_) {
            if (slot->easy == nullptr) {
                free_slot = slot.get();
                break;
            }
        }
        if (!free_slot) return;

        auto [req, done] = std::move(pending_.front());
        pending_.pop_front();

        free_slot->req = std::move(req);
        free_slot->done = std::move(done);
        free_slot->res = Res{};
        free_slot->received = 0;
        free_slot->aborting = false;
        free_slot->started = std::chrono::steady_clock::now();

        CURL* e = curl_easy_init();
        free_slot->easy = e;

        curl_easy_setopt(e, CURLOPT_URL, free_slot->req.url.c_str());
        curl_easy_setopt(e, CURLOPT_NOSIGNAL, 1L);
        curl_easy_setopt(e, CURLOPT_FOLLOWLOCATION, 0L);
        curl_easy_setopt(e, CURLOPT_SSL_VERIFYPEER, 1L);
        curl_easy_setopt(e, CURLOPT_SSL_VERIFYHOST, 2L);
        curl_easy_setopt(e, CURLOPT_ACCEPT_ENCODING, "gzip,deflate");
        curl_easy_setopt(e, CURLOPT_CONNECTTIMEOUT_MS, free_slot->req.connect_timeout_ms);
        curl_easy_setopt(e, CURLOPT_TIMEOUT_MS, free_slot->req.total_timeout_ms);
        curl_easy_setopt(e, CURLOPT_WRITEFUNCTION, &MultiRunner::writeCb);
        curl_easy_setopt(e, CURLOPT_WRITEDATA, free_slot);
        curl_easy_setopt(e, CURLOPT_HEADERFUNCTION, nullptr);

        if (!free_slot->req.proxy.empty()) {
            curl_easy_setopt(e, CURLOPT_PROXY, free_slot->req.proxy.c_str());
        }
        if (free_slot->req.connect_only) {
            curl_easy_setopt(e, CURLOPT_CONNECT_ONLY, 1L);
        }
        if (free_slot->req.method == "POST") {
            curl_easy_setopt(e, CURLOPT_POST, 1L);
            curl_easy_setopt(e, CURLOPT_POSTFIELDS, free_slot->req.body.c_str());
            curl_easy_setopt(e, CURLOPT_POSTFIELDSIZE,
                             static_cast<long>(free_slot->req.body.size()));
        } else if (free_slot->req.method != "GET") {
            curl_easy_setopt(e, CURLOPT_CUSTOMREQUEST, free_slot->req.method.c_str());
            if (!free_slot->req.body.empty()) {
                curl_easy_setopt(e, CURLOPT_POSTFIELDS, free_slot->req.body.c_str());
                curl_easy_setopt(e, CURLOPT_POSTFIELDSIZE,
                                 static_cast<long>(free_slot->req.body.size()));
            }
        }
        if (!free_slot->req.body.empty()) {
            free_slot->hdrs = curl_slist_append(free_slot->hdrs, "Content-Type: application/json");
        }
        for (const auto& h : free_slot->req.headers) {
            free_slot->hdrs = curl_slist_append(free_slot->hdrs, h.c_str());
        }
        if (free_slot->hdrs) {
            curl_easy_setopt(e, CURLOPT_HTTPHEADER, free_slot->hdrs);
        }

        curl_multi_add_handle(multi_, e);
    }
}

void MultiRunner::reap() {
    int msgs = 0;
    CURLMsg* msg = nullptr;
    while ((msg = curl_multi_info_read(multi_, &msgs)) != nullptr) {
        if (msg->msg != CURLMSG_DONE) continue;
        CURL* e = msg->easy_handle;

        Slot* slot = nullptr;
        for (auto& s : slots_) {
            if (s->easy == e) {
                slot = s.get();
                break;
            }
        }
        if (!slot) continue;

        long code = 0;
        curl_easy_getinfo(e, CURLINFO_RESPONSE_CODE, &code);
        slot->res.status = code;

        long cc = 0;
        curl_easy_getinfo(e, CURLINFO_HTTP_CONNECTCODE, &cc);
        slot->res.connectcode = cc;

        if (msg->data.result != CURLE_OK &&
            !(slot->aborting && msg->data.result == CURLE_WRITE_ERROR)) {
            slot->res.error = curl_easy_strerror(msg->data.result);
        }
        auto end = std::chrono::steady_clock::now();
        slot->res.ms = std::chrono::duration_cast<std::chrono::milliseconds>(end - slot->started)
                           .count();

        curl_multi_remove_handle(multi_, e);
        curl_easy_cleanup(e);
        if (slot->hdrs) {
            curl_slist_free_all(slot->hdrs);
            slot->hdrs = nullptr;
        }
        slot->easy = nullptr;

        auto done = slot->done;
        Res res = std::move(slot->res);
        slot->done = nullptr;
        if (done) done(std::move(res));
    }
}

void MultiRunner::run() {
    fill();
    int running = 0;
    while (true) {
        CURLMcode code = curl_multi_perform(multi_, &running);
        if (code != CURLM_OK) break;
        reap();
        fill();
        if (running == 0 && pending_.empty()) break;
        if (running == 0 && !pending_.empty()) {
            fill();
            continue;
        }
        int numfds = 0;
        curl_multi_poll(multi_, nullptr, 0, 50, &numfds);
    }
    reap();
}
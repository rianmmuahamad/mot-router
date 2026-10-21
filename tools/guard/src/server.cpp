#include "server.h"

#include <curl/curl.h>

#include <arpa/inet.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <sys/socket.h>
#include <unistd.h>

#include <cctype>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <sstream>
#include <thread>
#include <vector>

namespace {
constexpr size_t kMaxHeaderBytes = 64 * 1024;
constexpr size_t kMaxBodyBytes = 64 * 1024 * 1024;

std::string lower(std::string s) {
    for (char& c : s) c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
    return s;
}

std::string trim(std::string s) {
    size_t b = 0, e = s.size();
    while (b < e && (s[b] == ' ' || s[b] == '\t' || s[b] == '\r' || s[b] == '\n')) b++;
    while (e > b && (s[e - 1] == ' ' || s[e - 1] == '\t' || s[e - 1] == '\r' || s[e - 1] == '\n')) e--;
    return s.substr(b, e - b);
}

// sendAll writes every byte or reports failure.
bool sendAll(int fd, const char* data, size_t n) {
    size_t sent = 0;
    while (sent < n) {
        ssize_t w = ::send(fd, data + sent, n - sent, MSG_NOSIGNAL);
        if (w <= 0) return false;
        sent += static_cast<size_t>(w);
    }
    return true;
}
}  // namespace

bool readRequest(int fd, Request& out) {
    std::string buf;
    buf.reserve(8192);
    char tmp[8192];
    size_t header_end = std::string::npos;

    while (header_end == std::string::npos) {
        ssize_t n = ::recv(fd, tmp, sizeof(tmp), 0);
        if (n <= 0) return false;
        buf.append(tmp, static_cast<size_t>(n));
        header_end = buf.find("\r\n\r\n");
        if (buf.size() > kMaxHeaderBytes && header_end == std::string::npos) return false;
    }

    std::string head = buf.substr(0, header_end);
    std::istringstream hs(head);
    std::string requestLine;
    if (!std::getline(hs, requestLine)) return false;
    if (!requestLine.empty() && requestLine.back() == '\r') requestLine.pop_back();

    std::istringstream rl(requestLine);
    if (!(rl >> out.method >> out.path >> out.version)) return false;

    std::string line;
    while (std::getline(hs, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (line.empty()) continue;
        size_t colon = line.find(':');
        if (colon == std::string::npos) continue;
        out.headers[lower(trim(line.substr(0, colon)))] = trim(line.substr(colon + 1));
    }

    size_t content_length = 0;
    auto cl = out.headers.find("content-length");
    if (cl != out.headers.end()) {
        content_length = static_cast<size_t>(std::strtoull(cl->second.c_str(), nullptr, 10));
        if (content_length > kMaxBodyBytes) return false;
    }

    std::string rest = buf.substr(header_end + 4);
    // ReverseProxy clients wait for an explicit 100 response before sending a
    // large request body when Expect: 100-continue is present. This server
    // reads the body itself, so acknowledge the expectation before recv() or
    // both sides can wait until the client timeout.
    auto expect = out.headers.find("expect");
    if (expect != out.headers.end() && content_length > rest.size() &&
        lower(expect->second).find("100-continue") != std::string::npos) {
        static constexpr char kContinue[] = "HTTP/1.1 100 Continue\r\n\r\n";
        if (!sendAll(fd, kContinue, sizeof(kContinue) - 1)) return false;
    }
    if (content_length > 0) {
        while (rest.size() < content_length) {
            ssize_t n = ::recv(fd, tmp, sizeof(tmp), 0);
            if (n <= 0) return false;
            rest.append(tmp, static_cast<size_t>(n));
        }
        rest.resize(content_length);
    }
    out.body = rest;

    auto it = out.headers.find("x-motgate-user");
    if (it != out.headers.end()) out.user = it->second;
    return true;
}

void writeSimple(int fd, int status, const std::string& status_text, const std::string& body) {
    std::string resp = "HTTP/1.1 " + std::to_string(status) + " " + status_text + "\r\n";
    resp += "Content-Type: application/json\r\n";
    resp += "Content-Length: " + std::to_string(body.size()) + "\r\n";
    if (status == 429) resp += "Retry-After: 1\r\n";
    resp += "Connection: close\r\n\r\n";
    resp += body;
    sendAll(fd, resp.data(), resp.size());
}

namespace {

// StreamState bridges libcurl callbacks to the client socket.
//
// The response head is withheld until the header block ends, so a retryable
// upstream status can be discarded before anything reaches the client. Once the
// head is sent, the body is relayed as HTTP/1.1 chunked.
struct StreamState {
    int fd = -1;
    long status = 0;
    std::string content_type;
    bool head_sent = false;
    bool body_started = false;
    bool interim_headers = false;
    bool abort_for_retry = false;
    bool client_gone = false;
};

bool headIsRetryable(long status) {
    return status == 429 || status == 502 || status == 503;
}

void sendHead(StreamState& st) {
    if (st.head_sent) return;
    st.head_sent = true;
    std::string ctype = st.content_type.empty() ? "text/event-stream" : st.content_type;
    std::string head = "HTTP/1.1 " + std::to_string(st.status) + " \r\n";
    head += "Content-Type: " + ctype + "\r\n";
    head += "Cache-Control: no-cache\r\n";
    head += "Transfer-Encoding: chunked\r\n";
    head += "Connection: close\r\n\r\n";
    if (!sendAll(st.fd, head.data(), head.size())) st.client_gone = true;
}

}  // namespace

int forward(int fd, const Request& req, const Options& opt, int attempt) {
    CURL* e = curl_easy_init();
    if (!e) return 0;

    StreamState st;
    st.fd = fd;

    std::string url = opt.upstream;
    while (!url.empty() && url.back() == '/') url.pop_back();
    url += req.path;

    struct curl_slist* hdrs = nullptr;
    auto ct = req.headers.find("content-type");
    if (ct != req.headers.end() && !ct->second.empty()) {
        hdrs = curl_slist_append(hdrs, ("Content-Type: " + ct->second).c_str());
    }
    auto acc = req.headers.find("accept");
    if (acc != req.headers.end() && !acc->second.empty()) {
        hdrs = curl_slist_append(hdrs, ("Accept: " + acc->second).c_str());
    } else {
        hdrs = curl_slist_append(hdrs, "Accept: text/event-stream");
    }
    auto usr = req.headers.find("x-motgate-user");
    if (usr != req.headers.end() && !usr->second.empty()) {
        hdrs = curl_slist_append(hdrs, ("X-Motgate-User: " + usr->second).c_str());
    }
    std::string auth = "Authorization: Bearer " + opt.master_key;
    hdrs = curl_slist_append(hdrs, auth.c_str());

    curl_easy_setopt(e, CURLOPT_URL, url.c_str());
    curl_easy_setopt(e, CURLOPT_NOSIGNAL, 1L);
    curl_easy_setopt(e, CURLOPT_HTTPHEADER, hdrs);
    curl_easy_setopt(e, CURLOPT_HEADERFUNCTION,
                     +[](char* buffer, size_t size, size_t nitems, void* userp) -> size_t {
                         auto* s = static_cast<StreamState*>(userp);
                         size_t n = size * nitems;
                         std::string line(buffer, n);

                         if (line.rfind("HTTP/", 0) == 0) {
                             size_t sp = line.find(' ');
                             if (sp != std::string::npos) {
                                 long status = std::strtol(line.c_str() + sp + 1, nullptr, 10);
                                 // libcurl reports interim response blocks
                                 // (notably 100 Continue) through the same
                                 // callbacks as the final response. Do not
                                 // send an interim status as the downstream
                                 // response head: a Go client will then expect
                                 // a second status line and mistake the first
                                 // chunk length (for example "2a0") for it.
                                 s->interim_headers = status >= 100 && status < 200 && status != 101;
                                 if (!s->interim_headers) s->status = status;
                             }
                             return n;
                         }
                         if (line == "\r\n" || line == "\n") {
                             if (s->interim_headers) {
                                 s->interim_headers = false;
                                 return n;
                             }
                             if (headIsRetryable(s->status) && !s->head_sent) {
                                 // Abort so the caller can retry without having
                                 // written anything to the client.
                                 s->abort_for_retry = true;
                                 return 0;
                             }
                             sendHead(*s);
                             return n;
                         }
                         if (s->interim_headers) return n;
                         std::string low = lower(line);
                         if (low.rfind("content-type:", 0) == 0) {
                             s->content_type = trim(line.substr(line.find(':') + 1));
                         }
                         return n;
                     });
    curl_easy_setopt(e, CURLOPT_HEADERDATA, &st);
    curl_easy_setopt(e, CURLOPT_WRITEFUNCTION,
                     +[](char* ptr, size_t size, size_t nmemb, void* userp) -> size_t {
                         auto* s = static_cast<StreamState*>(userp);
                         size_t n = size * nmemb;
                         s->body_started = true;
                         if (!s->head_sent) sendHead(*s);
                         if (s->client_gone) return 0;

                         char prefix[32];
                         int plen = std::snprintf(prefix, sizeof(prefix), "%zx\r\n", n);
                         if (!sendAll(s->fd, prefix, static_cast<size_t>(plen))) {
                             s->client_gone = true;
                             return 0;
                         }
                         if (!sendAll(s->fd, ptr, n)) {
                             s->client_gone = true;
                             return 0;
                         }
                         if (!sendAll(s->fd, "\r\n", 2)) {
                             s->client_gone = true;
                             return 0;
                         }
                         return n;
                     });
    curl_easy_setopt(e, CURLOPT_WRITEDATA, &st);
    curl_easy_setopt(e, CURLOPT_CONNECTTIMEOUT_MS, 5000L);
    if (req.method == "GET" || req.method == "HEAD" || req.method == "DELETE") {
        if (req.method == "HEAD") {
            curl_easy_setopt(e, CURLOPT_NOBODY, 1L);
        } else {
            curl_easy_setopt(e, CURLOPT_HTTPGET, 1L);
        }
        if (req.method == "DELETE") {
            curl_easy_setopt(e, CURLOPT_CUSTOMREQUEST, "DELETE");
        }
    } else {
        curl_easy_setopt(e, CURLOPT_POST, 1L);
        curl_easy_setopt(e, CURLOPT_POSTFIELDS, req.body.data());
        curl_easy_setopt(e, CURLOPT_POSTFIELDSIZE, static_cast<long>(req.body.size()));
    }

    curl_easy_perform(e);
    long http_status = 0;
    curl_easy_getinfo(e, CURLINFO_RESPONSE_CODE, &http_status);
    curl_slist_free_all(hdrs);
    curl_easy_cleanup(e);

    if (st.abort_for_retry) return -1;  // retryable, nothing sent

    if (!st.head_sent) {
        // Upstream never produced headers: answer with the raw status (or 502).
        StreamState fallback;
        fallback.fd = fd;
        fallback.status = http_status ? http_status : 502;
        fallback.content_type = "application/json";
        sendHead(fallback);
        sendAll(fd, "0\r\n\r\n", 5);
    } else {
        sendAll(fd, "0\r\n\r\n", 5);
    }
    (void)attempt;
    return static_cast<int>(http_status);
}

void handleConnection(int fd, const Options& opt, Limiter& limiter, Stats& stats) {
    Request req;
    if (!readRequest(fd, req)) {
        ::close(fd);
        return;
    }

    if (req.path == "/healthz") {
        char body[512];
        std::snprintf(body, sizeof(body),
                      "{\"ok\":true,\"inflight\":%ld,\"queued\":%ld,\"rejected\":%ld,"
                      "\"upstream_errors\":%ld,\"max_inflight\":%d,\"per_user_inflight\":%d,"
                      "\"queue_wait_ms\":%d,\"waited\":%ld,\"queue_timeouts\":%ld}",
                      stats.inflight.load(), stats.queued.load(), stats.rejected.load(),
                      stats.upstream_errors.load(), opt.max_inflight, opt.per_user_inflight,
                      opt.queue_wait_ms, stats.waited.load(), stats.queue_timeouts.load());
        writeSimple(fd, 200, "OK", body);
        ::close(fd);
        return;
    }

    if (req.method == "OPTIONS") {
        writeSimple(fd, 204, "No Content", "");
        ::close(fd);
        return;
    }

    std::string message;
    int code = limiter.acquire(req.user, message, req.body.size(), [fd]() {
        char byte;
        ssize_t n = ::recv(fd, &byte, 1, MSG_PEEK | MSG_DONTWAIT);
        return n == 0 || (n < 0 && errno != EAGAIN && errno != EWOULDBLOCK && errno != EINTR);
    });
    if (code == 499) {
        ::close(fd);
        return;
    }
    if (code != 0) {
        stats.rejected++;
        std::string body = "{\"type\":\"error\",\"error\":{\"type\":\"" +
                           std::string(code == 400 ? "invalid_request_error" : "rate_limit_error") +
                           "\",\"message\":\"" + message + "\"}}";
        writeSimple(fd, code, code == 400 ? "Bad Request" : "Too Many Requests", body);
        ::close(fd);
        return;
    }

    stats.inflight++;
    int status = 0;
    for (int attempt = 0;; attempt++) {
        int rc = forward(fd, req, opt, attempt);
        if (rc != -1) {
            status = rc;
            break;
        }
        if (attempt >= opt.retries) {
            // Retries exhausted: surface the retryable status honestly.
            StreamState st;
            st.fd = fd;
            st.status = 503;
            st.content_type = "application/json";
            sendHead(st);
            sendAll(fd, "0\r\n\r\n", 5);
            status = 503;
            break;
        }
        int backoff = opt.retry_backoff_ms;
        for (int i = 0; i < attempt; i++) backoff *= 3;
        std::this_thread::sleep_for(std::chrono::milliseconds(backoff));
    }
    stats.inflight--;
    limiter.release(req.user);
    if (status == 0) stats.upstream_errors++;
    if (status >= 200 && status < 300) stats.ok++;

    ::close(fd);
}

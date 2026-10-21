#pragma once

#include "limit.h"

#include <atomic>
#include <string>
#include <unordered_map>

// Request is the parsed client request handed to the upstream forwarder.
struct Request {
    std::string method;
    std::string path;
    std::string version;
    std::unordered_map<std::string, std::string> headers;  // lowercase keys
    std::string body;
    std::string user;
};

// readRequest parses one HTTP/1.1 request from fd. Returns false when the peer
// closed or the request is unusable.
bool readRequest(int fd, Request& out);

// writeSimple writes a status with an optional JSON body and closes.
void writeSimple(int fd, int status, const std::string& status_text, const std::string& body);

// forward streams the request to the upstream and relays the response.
// Returns the upstream status (0 when no answer arrived), or -1 when the
// upstream answered with a retryable status before any byte was sent.
int forward(int fd, const Request& req, const Options& opt, int attempt);

// handleConnection runs the full per-connection flow.
void handleConnection(int fd, const Options& opt, Limiter& limiter, Stats& stats);

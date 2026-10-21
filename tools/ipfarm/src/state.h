#pragma once

#include "common.h"

#include <string>
#include <unordered_map>
#include <vector>

// State is the persisted node table (state.tsv). Counters accumulate across
// runs so the scorer can prefer nodes with a track record.
struct State {
    std::vector<StateNode> nodes;
    std::unordered_map<std::string, size_t> index;  // name -> position in nodes

    void load(const std::string& path);
    void save(const std::string& path) const;

    StateNode* find(const std::string& name);
    const StateNode* find(const std::string& name) const;

    // upsert inserts or updates a node keyed by name.
    StateNode& upsert(const std::string& name);
};

// score implements the fixed ranking formula. A penalized node is never picked.
double score(const StateNode& n, long now);
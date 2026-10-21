#pragma once

#include <string>
#include <vector>

// stripCR removes a trailing carriage return and surrounding spaces. It
// deliberately does NOT strip tabs: TSV rows may legitimately end in an empty
// field, and trim() would eat that separator.
std::string stripCR(const std::string& s);

std::string trim(const std::string& s);
std::vector<std::string> split(const std::string& s, char delim);

// jsonFindString returns the first string value bound to "key" in body, using a
// linear scan rather than a JSON parser. Body shapes read here are fixed
// (mihomo's group object), and no JSON parser exists on this box.
bool jsonFindString(const std::string& body, const std::string& key, std::string& out);

// jsonFindGroupNow reads the "now" field of a named proxy group out of a
// mihomo /proxies response, scoping the search to that group's object.
bool jsonFindGroupNow(const std::string& body, const std::string& group, std::string& out);

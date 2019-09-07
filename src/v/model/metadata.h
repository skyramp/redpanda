#pragma once

#include "model/fundamental.h"
#include "seastarx.h"

#include <seastar/core/sstring.hh>

#include <optional>
#include <unordered_set>
#include <vector>

namespace model {
struct node_id {
    using type = int32_t;
    static constexpr const type min = std::numeric_limits<type>::min();
    node_id() noexcept = default;
    constexpr explicit node_id(type id) noexcept
      : value(id) {
    }
    const type value = min;
};

class broker {
public:
    broker(
      node_id id,
      sstring host,
      int32_t port,
      std::optional<sstring> rack) noexcept
      : _id(id)
      , _host(std::move(host))
      , _port(port)
      , _rack(rack) {
    }

    node_id id() const {
        return _id;
    }

    const sstring& host() const {
        return _host;
    }

    int32_t port() const {
        return _port;
    }

    const std::optional<sstring>& rack() const {
        return _rack;
    }

private:
    node_id _id;
    sstring _host;
    int32_t _port;
    std::optional<sstring> _rack;
};

struct partition_metadata {
    partition_metadata() noexcept = default;
    partition_metadata(partition p) noexcept
      : id(std::move(p)) {
    }
    partition id;
};

struct topic_metadata {
    topic_metadata(topic_view v) noexcept
      : topic(std::move(v)) {
    }
    topic_view topic;
    std::vector<partition_metadata> partitions;
};

namespace internal {
struct hash_by_topic_name {
    size_t operator()(const topic_metadata& tm) const {
        return std::hash<std::string_view>()(tm.topic.name());
    }
};

struct equals_by_topic_name {
    bool
    operator()(const topic_metadata& tm1, const topic_metadata& tm2) const {
        return tm1.topic.name() == tm2.topic.name();
    }
};

} // namespace internal

using topic_metadata_map = std::unordered_set<
  topic_metadata,
  internal::hash_by_topic_name,
  internal::equals_by_topic_name>;

} // namespace model

namespace std {

template<>
struct hash<model::topic_view> {
    size_t operator()(model::topic_view v) const {
        return hash<std::string_view>()(v.name());
    }
};

template<>
struct hash<model::topic> {
    size_t operator()(model::topic t) const {
        return hash<seastar::sstring>()(t.name);
    }
};

} // namespace std

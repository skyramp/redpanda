/*
 * Copyright 2020 Vectorized, Inc.
 *
 * Use of this software is governed by the Business Source License
 * included in the file licenses/BSL.md
 *
 * As of the Change Date specified in that file, in accordance with
 * the Business Source License, use of this software will be governed
 * by the Apache License, Version 2.0
 */

#pragma once
#include "cluster/controller.h"
#include "cluster/members_table.h"
#include "cluster/metadata_cache.h"
#include "cluster/partition_leaders_table.h"
#include "cluster/partition_manager.h"
#include "cluster/shard_table.h"
#include "cluster/topics_frontend.h"
#include "cluster/types.h"
#include "kafka/client/transport.h"
#include "kafka/protocol/fetch.h"
#include "kafka/server/handlers/topics/topic_utils.h"
#include "model/metadata.h"
#include "model/namespace.h"
#include "model/timeout_clock.h"
#include "pandaproxy/rest/configuration.h"
#include "pandaproxy/schema_registry/configuration.h"
#include "redpanda/application.h"
#include "resource_mgmt/cpu_scheduling.h"
#include "rpc/dns.h"
#include "storage/directories.h"
#include "storage/tests/utils/disk_log_builder.h"
#include "storage/tests/utils/random_batch.h"
#include "test_utils/async.h"
#include "test_utils/fixture.h"
#include "test_utils/logs.h"
#include "utils/unresolved_address.h"

#include <seastar/util/log.hh>

#include <fmt/format.h>

#include <filesystem>

class redpanda_thread_fixture {
public:
    static constexpr const char* rack_name = "i-am-rack";

    redpanda_thread_fixture(
      model::node_id node_id,
      int32_t kafka_port,
      int32_t rpc_port,
      int32_t proxy_port,
      int32_t schema_reg_port,
      int32_t coproc_supervisor_port,
      std::vector<config::seed_server> seed_servers,
      ss::sstring base_dir,
      std::optional<scheduling_groups> sch_groups,
      bool remove_on_shutdown)
      : app(ssx::sformat("redpanda-{}", node_id()))
      , proxy_port(proxy_port)
      , schema_reg_port(schema_reg_port)
      , data_dir(std::move(base_dir))
      , remove_on_shutdown(remove_on_shutdown) {
        configure(
          node_id,
          kafka_port,
          rpc_port,
          coproc_supervisor_port,
          std::move(seed_servers));
        app.initialize(
          proxy_config(proxy_port),
          proxy_client_config(kafka_port),
          schema_reg_config(schema_reg_port),
          proxy_client_config(kafka_port),
          sch_groups);
        app.check_environment();
        app.configure_admin_server();
        app.wire_up_services();
        app.start();

        // used by request context builder
        proto = std::make_unique<kafka::protocol>(
          app.smp_service_groups.kafka_smp_sg(),
          app.metadata_cache,
          app.controller->get_topics_frontend(),
          app.quota_mgr,
          app.group_router,
          app.shard_table,
          app.partition_manager,
          app.coordinator_ntp_mapper,
          app.fetch_session_cache,
          app.id_allocator_frontend,
          app.controller->get_credential_store(),
          app.controller->get_authorizer(),
          app.controller->get_security_frontend(),
          app.controller->get_api(),
          app.tx_gateway_frontend,
          app.data_policies,
          std::nullopt);
    }

    // creates single node with default configuration
    redpanda_thread_fixture()
      : redpanda_thread_fixture(
        model::node_id(1),
        9092,
        33145,
        8082,
        8081,
        43189,
        {},
        ssx::sformat("test.dir_{}", time(0)),
        std::nullopt,
        true) {}

    // Restart the fixture with an existing data directory
    explicit redpanda_thread_fixture(std::filesystem::path existing_data_dir)
      : redpanda_thread_fixture(
        model::node_id(1),
        9092,
        33145,
        8082,
        8081,
        43189,
        {},
        existing_data_dir.string(),
        std::nullopt,
        true) {}

    ~redpanda_thread_fixture() {
        app.shutdown();
        if (remove_on_shutdown) {
            std::filesystem::remove_all(data_dir);
        }
    }

    config::configuration& lconf() { return config::shard_local_cfg(); }

    void configure(
      model::node_id node_id,
      int32_t kafka_port,
      int32_t rpc_port,
      int32_t coproc_supervisor_port,
      std::vector<config::seed_server> seed_servers) {
        auto base_path = std::filesystem::path(data_dir);
        ss::smp::invoke_on_all([node_id,
                                kafka_port,
                                rpc_port,
                                coproc_supervisor_port,
                                seed_servers = std::move(seed_servers),
                                base_path]() mutable {
            auto& config = config::shard_local_cfg();
            config.get("node_id").set_value(node_id);

            config.get("rpc_server")
              .set_value(unresolved_address("127.0.0.1", rpc_port));
            config.get("kafka_api")
              .set_value(
                std::vector<model::broker_endpoint>{model::broker_endpoint(
                  unresolved_address("127.0.0.1", kafka_port))});
            config.get("seed_servers").set_value(seed_servers);
            config.get("enable_pid_file").set_value(false);
            config.get("developer_mode").set_value(true);
            config.get("enable_admin_api").set_value(false);
            config.get("enable_coproc").set_value(true);
            config.get("join_retry_timeout_ms").set_value(100ms);
            config.get("members_backend_retry_ms").set_value(1000ms);
            config.get("coproc_supervisor_server")
              .set_value(
                unresolved_address("127.0.0.1", coproc_supervisor_port));
            config.get("rack").set_value(std::optional<ss::sstring>(rack_name));
            config.get("disable_metrics").set_value(true);
            config.get("data_directory")
              .set_value(config::data_directory_path{.path = base_path});
        }).get0();
    }

    YAML::Node proxy_config(uint16_t proxy_port = 8082) {
        pandaproxy::rest::configuration cfg;
        cfg.get("pandaproxy_api")
          .set_value(std::vector<model::broker_endpoint>{model::broker_endpoint(
            unresolved_address("127.0.0.1", proxy_port))});
        return to_yaml(cfg);
    }

    YAML::Node proxy_client_config(
      uint16_t kafka_api_port
      = config::shard_local_cfg().kafka_api()[0].address.port()) {
        kafka::client::configuration cfg;
        unresolved_address kafka_api{
          config::shard_local_cfg().kafka_api()[0].address.host(),
          kafka_api_port};
        cfg.brokers.set_value(std::vector<unresolved_address>({kafka_api}));
        return to_yaml(cfg);
    }

    YAML::Node schema_reg_config(uint16_t listen_port = 8081) {
        pandaproxy::schema_registry::configuration cfg;
        cfg.get("schema_registry_api")
          .set_value(std::vector<model::broker_endpoint>{model::broker_endpoint(
            unresolved_address("127.0.0.1", listen_port))});
        cfg.get("schema_registry_replication_factor")
          .set_value(std::make_optional<int16_t>(1));
        return to_yaml(cfg);
    }

    ss::future<> wait_for_controller_leadership() {
        auto tout = ss::lowres_clock::now() + std::chrono::seconds(10);
        auto id = co_await app.controller->get_partition_leaders()
                    .local()
                    .wait_for_leader(model::controller_ntp, tout, {});

        co_await tests::cooperative_spin_wait_with_timeout(10s, [this, id] {
            auto& members = app.controller->get_members_table();
            return members.local().contains(id);
        });
    }

    ss::future<kafka::client::transport> make_kafka_client() {
        return ss::make_ready_future<kafka::client::transport>(
          rpc::base_transport::configuration{
            .server_addr = config::shard_local_cfg().kafka_api()[0].address,
          });
    }

    model::ntp
    make_default_ntp(model::topic topic, model::partition_id partition) {
        return model::ntp(model::kafka_namespace, std::move(topic), partition);
    }

    storage::log_config make_default_config() {
        return storage::log_config(
          storage::log_config::storage_type::disk,
          data_dir.string(),
          1_GiB,
          storage::debug_sanitize_files::yes);
    }

    ss::future<> wait_for_topics(std::vector<cluster::topic_result> results) {
        return tests::cooperative_spin_wait_with_timeout(
          2s, [this, results = std::move(results)] {
              return std::all_of(
                results.begin(),
                results.end(),
                [this](const cluster::topic_result& r) {
                    auto md = app.metadata_cache.local().get_topic_metadata(
                      r.tp_ns);
                    return md
                           && std::all_of(
                             md->partitions.begin(),
                             md->partitions.end(),
                             [this, &r](const model::partition_metadata& p) {
                                 return app.shard_table.local().shard_for(
                                   model::ntp(r.tp_ns.ns, r.tp_ns.tp, p.id));
                             });
                });
          });
    }

    ss::future<>
    add_topic(model::topic_namespace_view tp_ns, int partitions = 1) {
        std::vector<cluster::topic_configuration> cfgs{
          cluster::topic_configuration(tp_ns.ns, tp_ns.tp, partitions, 1)};
        return app.controller->get_topics_frontend()
          .local()
          .create_topics(std::move(cfgs), model::no_timeout)
          .then([this](std::vector<cluster::topic_result> results) {
              return wait_for_topics(std::move(results));
          });
    }

    ss::future<> add_non_replicable_topic(
      model::topic_namespace tp_ns_src, model::topic_namespace tp_ns) {
        cluster::non_replicable_topic nrt{
          .source = std::move(tp_ns_src), .name = std::move(tp_ns)};
        return app.controller->get_topics_frontend()
          .local()
          .create_non_replicable_topics({std::move(nrt)}, model::no_timeout)
          .then([this](std::vector<cluster::topic_result> results) {
              return wait_for_topics(std::move(results));
          });
    }

    ss::future<> wait_for_partition_offset(
      model::ntp ntp,
      model::offset o,
      model::timeout_clock::duration tout = 3s) {
        return tests::cooperative_spin_wait_with_timeout(
          tout, [this, ntp = std::move(ntp), o]() mutable {
              auto shard = app.shard_table.local().shard_for(ntp);
              if (!shard) {
                  return ss::make_ready_future<bool>(false);
              }
              return app.partition_manager.invoke_on(
                *shard, [ntp, o](cluster::partition_manager& mgr) {
                    auto partition = mgr.get(ntp);
                    return partition && partition->committed_offset() >= o;
                });
          });
    }

    model::ntp make_data(model::revision_id rev) {
        auto topic_name = ssx::sformat("my_topic_{}", 0);
        model::ntp ntp(
          model::kafka_namespace,
          model::topic(topic_name),
          model::partition_id(0));

        storage::ntp_config ntp_cfg(
          ntp, lconf().data_directory().as_sstring(), nullptr, rev);

        storage::disk_log_builder builder(make_default_config());
        using namespace storage; // NOLINT
        builder | start(std::move(ntp_cfg)) | add_segment(model::offset(0))
          | add_random_batches(
            model::offset(0), 20, maybe_compress_batches::yes)
          | stop();

        add_topic(model::topic_namespace_view(ntp)).get();

        return ntp;
    }

    kafka::request_context make_request_context() {
        security::sasl_server sasl(security::sasl_server::sasl_state::complete);
        auto conn = ss::make_lw_shared<kafka::connection_context>(
          *proto,
          rpc::server::resources(nullptr, nullptr),
          std::move(sasl),
          false);

        kafka::request_header header;
        auto encoder_context = kafka::request_context(
          conn, std::move(header), iobuf(), std::chrono::milliseconds(0));

        iobuf buf;
        kafka::fetch_request request;
        // do not use incremental fetch requests
        request.data.max_wait_ms = std::chrono::milliseconds::zero();
        kafka::response_writer writer(buf);
        request.encode(writer, encoder_context.header().version);

        return kafka::request_context(
          conn,
          std::move(header),
          std::move(buf),
          std::chrono::milliseconds(0));
    }

    application app;
    uint16_t proxy_port;
    uint16_t schema_reg_port;
    std::filesystem::path data_dir;
    std::unique_ptr<kafka::protocol> proto;
    bool remove_on_shutdown;
};

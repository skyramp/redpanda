# Copyright 2020 Vectorized, Inc.
#
# Use of this software is governed by the Business Source License
# included in the file licenses/BSL.md
#
# As of the Change Date specified in that file, in accordance with
# the Business Source License, use of this software will be governed
# by the Apache License, Version 2.0
import ducktape.errors
from ducktape.utils.util import wait_until
from ducktape.mark.resource import cluster

from rptest.tests.redpanda_test import RedpandaTest
from rptest.services.admin import Admin

import time
import threading
import random


class LogLevelTest(RedpandaTest):
    @cluster(num_nodes=3)
    def test_log_level_control(self):
        admin = Admin(self.redpanda)
        node = self.redpanda.nodes[0]

        # This test assumes the default log level while testing is trace
        default_log_level = "trace"

        # set to warn level. message seen at trace
        with self.redpanda.monitor_log(node) as mon:
            admin.set_log_level("admin_api_server", "warn")
            mon.wait_until(
                f"Set log level for {{admin_api_server}}: {default_log_level} -> warn",
                timeout_sec=5,
                backoff_sec=1,
                err_msg="Never saw message")

        # set to debug. log level at warn, so shouldn't see it
        try:
            with self.redpanda.monitor_log(node) as mon:
                admin.set_log_level("admin_api_server", "debug")
                mon.wait_until(
                    "Set log level for {admin_api_server}: warn -> debug",
                    timeout_sec=10,
                    backoff_sec=1,
                    err_msg="Never saw message")
            assert False, "Should not have seen message"
        except ducktape.errors.TimeoutError:
            pass

        # should now see it again
        with self.redpanda.monitor_log(node) as mon:
            admin.set_log_level("admin_api_server", "info")
            mon.wait_until(
                "Set log level for {admin_api_server}: debug -> info",
                timeout_sec=5,
                backoff_sec=1,
                err_msg="Never saw message")

        with self.redpanda.monitor_log(node) as mon:
            admin.set_log_level("admin_api_server", "debug", expires=5)
            mon.wait_until(
                f"Expiring log level for {{admin_api_server}} to {default_log_level}",
                timeout_sec=10,
                backoff_sec=1,
                err_msg="Never saw message")

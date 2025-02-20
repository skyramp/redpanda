# Licensed to the Apache Software Foundation (ASF) under one or more
# contributor license agreements.  See the NOTICE file distributed with
# this work for additional information regarding copyright ownership.
# The ASF licenses this file to You under the Apache License, Version 2.0
# (the "License"); you may not use this file except in compliance with
# the License.  You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
FROM ubuntu:groovy-20210325

ENV TZ="UTC" \
    DEBIAN_FRONTEND=noninteractive

# - install openssh, jvm and kafka cli tools
# - allow user env variables in ssh sessions
RUN apt update && \
    apt install -y \
      build-essential \
      curl \
      default-jdk \
      cmake \
      git \
      iptables \
      libatomic1 \
      libyajl-dev \
      libsasl2-dev \
      libssl-dev \
      maven \
      nodejs \
      npm \
      openssh-server \
      python3-pip && \
    rm -rf /var/lib/apt/lists/* && \
    echo 'PermitUserEnvironment yes' >> /etc/ssh/sshd_config

# install go
RUN mkdir -p /usr/local/go/ && \
    bash -c 'if [[ $(uname -m) = "aarch64" ]]; then ARCHID="arm64"; else export ARCHID="amd64"; fi && \
    curl -sSLf --retry 3 --retry-connrefused --retry-delay 2 https://golang.org/dl/go1.17.linux-${ARCHID}.tar.gz | tar -xz -C /usr/local/go/ --strip 1'
ENV PATH="${PATH}:/usr/local/go/bin"

# install kafka binary dependencies, librdkafka dev, kcat and kaf tools
ENV KAFKA_MIRROR="https://s3-us-west-2.amazonaws.com/kafka-packages"
RUN mkdir -p "/opt/kafka-2.3.1" && chmod a+rw /opt/kafka-2.3.1 && curl -s "$KAFKA_MIRROR/kafka_2.12-2.3.1.tgz" | tar xz --strip-components=1 -C "/opt/kafka-2.3.1" && \
    mkdir -p "/opt/kafka-2.4.1" && chmod a+rw /opt/kafka-2.4.1 && curl -s "$KAFKA_MIRROR/kafka_2.12-2.4.1.tgz" | tar xz --strip-components=1 -C "/opt/kafka-2.4.1" && \
    mkdir -p "/opt/kafka-2.5.0" && chmod a+rw /opt/kafka-2.5.0 && curl -s "$KAFKA_MIRROR/kafka_2.12-2.5.0.tgz" | tar xz --strip-components=1 -C "/opt/kafka-2.5.0" && \
    mkdir -p "/opt/kafka-2.7.0" && chmod a+rw /opt/kafka-2.7.0 && curl -s "$KAFKA_MIRROR/kafka_2.12-2.7.0.tgz" | tar xz --strip-components=1 -C "/opt/kafka-2.7.0" && \
    mkdir /opt/librdkafka && \
    curl -SL "https://github.com/edenhill/librdkafka/archive/v1.8.0.tar.gz" | tar -xz --strip-components=1 -C /opt/librdkafka && \
    cd /opt/librdkafka && \
    ./configure && \
    make -j$(nproc) && \
    make install && \
    cd /opt/librdkafka/tests && \
    make build -j$(nproc) && \
    go get github.com/birdayz/kaf/cmd/kaf && \
    mv /root/go/bin/kaf /usr/local/bin/ && \
    mkdir /tmp/kcat && \
    curl -SL "https://github.com/edenhill/kcat/archive/1.7.0.tar.gz" | tar -xz --strip-components=1 -C /tmp/kcat && \
    cd /tmp/kcat && \
    ./configure && \
    make -j$(nproc) && \
    make install && \
    ldconfig

# Install dependencies for kafka clients such as sarama and franz-go
RUN git -C /opt clone https://github.com/Shopify/sarama.git && \
    cd /opt/sarama/examples/interceptors && go mod tidy && go build && \
    cd /opt/sarama/examples/http_server && go mod tidy && go build && \
    cd /opt/sarama/examples/consumergroup && go mod tidy && go build && \
    cd /opt/sarama/examples/sasl_scram_client && go mod tidy && go build && \
    git -C /opt clone https://github.com/twmb/franz-go.git && cd /opt/franz-go && \
    cd /opt/franz-go/examples/bench && go mod tidy && go build

RUN go install github.com/twmb/kcl@latest && \
    mv /root/go/bin/kcl /usr/local/bin/

# Expose port 8080 for any http examples within clients
EXPOSE 8080

# copy source of test (java) programs
COPY --chown=0:0 tests/java /tmp/java
RUN mvn clean package --batch-mode --file /tmp/java/kafka-verifier --define buildDir=/opt/kafka-verifier/ && \
    mvn clean package --batch-mode --file /tmp/java/compacted-log-verifier --define buildDir=/opt/compacted-log-verifier && \
    mvn clean package --batch-mode --file /tmp/java/tx-verifier --define buildDir=/opt/tx-verifier

# copy ssh keys
COPY --chown=0:0 tests/docker/ssh /root/.ssh

# install python dependencies and rptest package.
# rptest package installed in editable mode so it can be overridden.
# passes --force so system pip packages can be updated
COPY --chown=0:0 tests/setup.py /root/tests/
RUN python3 -m pip install --upgrade --force pip && \
    python3 -m pip install --force --no-cache-dir -e /root/tests/

CMD service ssh start && tail -f /dev/null

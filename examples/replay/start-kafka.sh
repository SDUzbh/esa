#!/bin/sh
# Operator-controlled baseline: immutable binaries, mutable per-ticket state.
set -eu
mkdir -p /workspace/config /workspace/logs /workspace/zookeeper /workspace/broker
export LOG_DIR=/workspace/logs
export KAFKA_HEAP_OPTS='-Xms256m -Xmx256m'
export KAFKA_LOG4J_OPTS='-Dlog4j.configuration=file:/input/product/config/log4j.properties'
cat > /workspace/config/zookeeper.properties <<'EOF'
dataDir=/workspace/zookeeper
clientPort=2181
tickTime=2000
maxClientCnxns=0
EOF
cat > /workspace/config/server.properties <<'EOF'
broker.id=0
listeners=PLAINTEXT://127.0.0.1:9092
advertised.listeners=PLAINTEXT://127.0.0.1:9092
log.dirs=/workspace/broker
zookeeper.connect=127.0.0.1:2181
offsets.topic.replication.factor=1
EOF
sh /input/product/bin/zookeeper-server-start.sh /workspace/config/zookeeper.properties > /workspace/logs/zookeeper.out 2>&1 &
sh /input/product/bin/kafka-server-start.sh /workspace/config/server.properties > /workspace/logs/kafka.out 2>&1 &
attempt=0
until grep -Eq 'Kafka Server.*started' /workspace/logs/kafka.out; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 60 ]; then cat /workspace/logs/zookeeper.out /workspace/logs/kafka.out; exit 1; fi
  sleep 1
done
sh /input/product/bin/kafka-topics.sh --zookeeper 127.0.0.1:2181 --create --topic esa-replay-control --partitions 1 --replication-factor 1
printf 'KAFKA_SERVER_READY=PASS\n'

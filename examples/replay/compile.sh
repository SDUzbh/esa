#!/bin/sh
set -eu
classes=/input/classes
if [ ! -f "$classes/.complete" ]; then classes=/workspace/classes; fi
if [ ! -f "$classes/.complete" ]; then
  mkdir -p "$classes"
  find /input/source/clients/src/main/java -name '*.java' > /tmp/sources.txt
  printf '%s\n' /input/source/clients/src/test/java/org/apache/kafka/clients/MockClient.java /input/MetadataDisconnectReplay.java >> /tmp/sources.txt
  javac -source 7 -target 7 -cp '/input/deps/*' -d "$classes" @/tmp/sources.txt
  touch "$classes/.complete"
fi
printf 'CLIENT_MODULE_COMPILATION=PASS\n'
java -cp "$classes:/input/deps/*" MetadataDisconnectReplay

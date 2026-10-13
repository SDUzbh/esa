# Performance investigation · version 2

Fix workload, target runtime, measurement units and baseline controls before interpreting latency, CPU, allocation or locking. Request runtime/debugger or approved temporary tools with a named hypothesis. Do not assert that a sampler, JFR or profiler is installed because this Profile exists.

Compare repeatable observations with unchanged inputs. Distinguish warmup, environment limits and workload variance from implementation costs. Preserve product code/classes; auxiliary tools must use a separate tool path and classpath. Reuse unchanged builds and controls. Finish with bounded evidence, concrete source findings when justified, advice and measurement steps. Missing workload or sampler support is a limitation, not a verified product regression.

The preset image provides the tool collection in `toolset.txt`. Use the host-supplied readiness inventory for exact installed versions and paths. Target Java remains under `/opt/java/openjdk`; modern JFR/analysis tools use `/opt/esa/tool-jdk` explicitly. Do not substitute the tool JVM for the registered product runtime. Kernel restrictions may limit native sampling or tracing even when a tool is installed.

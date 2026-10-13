# Java defect analysis · version 2

Inspect relevant historical source and request runtime/debugger capabilities for an explicit hypothesis. The registered runtime supplies its target JDK; do not substitute modern libraries for historical dependencies. Use JDK debugging tools only if the readiness observation confirms they exist.

Read numbered source with `read_code`; use `source_search` to locate exact text. Run the registered reproducer or a bounded auxiliary script after a runtime grant. Preserve baseline controls and separate an environment failure from a product failure. Reuse existing successful controls when hashes, inputs and tool versions match; record a reason for a rerun.

Product code, dependencies and classes are immutable. Scratch scripts and pinned tools stay in `/workspace`. Tool/classpath substitution must not change product behavior. Ask through `tool_install` or propose an environment change when policy denies a needed tool. Finish with inspected source lines, cause hypotheses, correction advice, verification steps and uncertainty. A reproduced symptom does not alone prove the causal fault site.

The preset image provides the tool collection in `toolset.txt`. Use the host-supplied readiness inventory for exact installed versions and paths. Target Java remains under `/opt/java/openjdk`; modern JFR/analysis tools use `/opt/esa/tool-jdk` explicitly. Do not substitute the tool JVM for the registered product runtime. Kernel restrictions may limit native sampling or tracing even when a tool is installed.

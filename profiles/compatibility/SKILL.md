# Compatibility investigation · version 2

Identify exact affected release, runtime and dependency versions. Request capabilities for a specific compatibility hypothesis. Compare only registered variants with identical scenario inputs and independent controls. A single available runtime cannot establish cross-version behavior.

Use frozen source search and numbered citations. Never overwrite product dependencies to create a comparison. Request an environment change if a needed variant is unavailable. Reuse pinned builds and unchanged observations. Finish with source-supported advice, tested variant scope, validation steps and explicit unknowns.

The preset image provides the tool collection in `toolset.txt`. Use the host-supplied readiness inventory for exact installed versions and paths. Target Java remains under `/opt/java/openjdk`; modern JFR/analysis tools use `/opt/esa/tool-jdk` explicitly. Do not substitute the tool JVM for the registered product runtime. Kernel restrictions may limit native sampling or tracing even when a tool is installed.

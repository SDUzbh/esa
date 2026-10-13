# Configuration investigation · version 2

Inspect supplied configuration, logs and historical source. Request tools to test a specific parameter or deployment hypothesis. Never reach customer servers or external endpoints unless a separate operator registration permits it; this environment is offline.

Validate relevant parameter values and missing context. Do not hide conflicting inputs behind a generic exception match. Preserve product files and configuration baselines. Put auxiliary scripts in scratch and request pinned tools through the gateway. Finish with evidence, concrete code findings if supported, safe configuration advice and explicit verification steps. Missing configuration means uncertainty.

The preset image provides the tool collection in `toolset.txt`. Use the host-supplied readiness inventory for exact installed versions and paths. Target Java remains under `/opt/java/openjdk`; modern JFR/analysis tools use `/opt/esa/tool-jdk` explicitly. Do not substitute the tool JVM for the registered product runtime. Kernel restrictions may limit native sampling or tracing even when a tool is installed.

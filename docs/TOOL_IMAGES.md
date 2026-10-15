# Preset tool images

Each Profile has a declarative `profiles/<profile>/toolset.txt`. One inspected `deploy/tools/toolset.release.txt` records the shared installed versions, image ID, upstream hashes and executable paths. Profile version 2 includes these declarations in its approved manifest hash. Do not execute TXT files as shell scripts.

One immutable `esa-toolkit` image contains the union of approved tools. Profiles select Skills, capability policy and recommended tool subsets; they do not duplicate operating systems or package inventories. Distinct customer runtimes still require registered scenario/environment inputs.

| Profile | Prepared tools |
| --- | --- |
| General | Python, ripgrep, jq, Git, curl, file and binary inspection |
| Java debugging | JDK 8 java/javac/jdb/jcmd/jstack/jmap/javap/jdeps, Soot, SootUp, GDB, strace, lsof |
| Performance | async-profiler, sysstat, vmstat, pidstat, iostat, time, JFR tools |
| Compatibility | javap/jdeps, diff, readelf/objdump, unzip, separate analysis JVM |
| Configuration | ip/ss, dig, tcpdump, netcat, curl/jq |

The default registered Java fixture uses the JDK 8 runtime. JDK 21 analysis/JFR tools are separately installed at `/opt/esa/tool-jdk`; they never replace the default `JAVA_HOME` or PATH's product Java. Customer source, registered product dependencies and frozen baseline classes are not upgraded. This preset is a current analysis environment, not proof of an exact historical JVM patch level. An explicitly supplied historical image cannot be overwritten by a preset selection.

The shared image includes the classic [Soot command-line tool](https://github.com/soot-oss/soot) and its bundled dependencies at `/opt/esa/soot/soot.jar`. The `soot` launcher uses the analysis JVM with a 1 GiB heap; its default analysis classpath contains frozen product classes, registered dependencies and target JDK 8 libraries. After a runtime capability grant, the Agent uses the existing `run` tool, for example:

```sh
soot -f jimple -keep-line-number -d /workspace/diagnostics/soot org.apache.kafka.clients.NetworkClient
```

Output stays in scratch. Do not replace product classes or run generated analysis as the product.

[SootUp](https://github.com/soot-oss/SootUp) is installed alongside classic Soot. Its Java bytecode frontend, core/interceptor dependencies and callgraph SDK are in `/opt/esa/sootup/lib/*`. The ESA `sootup` launcher uses the separate analysis JVM with a 1 GiB heap and requires no per-ticket dependency download or build:

```sh
sootup --version
sootup jimple org.apache.kafka.clients.NetworkClient handleCompletedReceives > /workspace/diagnostics/network-client.jimple
sootup cfg org.apache.kafka.clients.NetworkClient handleCompletedReceives > /workspace/diagnostics/network-client.cfg
```

The default input classpath reads `/input/classes`, `/input/deps/*.jar` and the target JDK 8 libraries, not the tool JVM's runtime libraries. Create the scratch output directory first. Use `--classpath PATHS` to supply a complete alternative analysis classpath. The optional method filter selects a name (all overloads) or a full Soot signature; unknown classes/methods fail. Jimple and CFG output go to stdout. CFG output includes statement IDs plus normal and exceptional edges; IDs are not source line numbers.

Engineering may write custom SootUp analysis classes under `/workspace`, compile them using `/opt/esa/tool-jdk/bin/javac --release 17 -cp '/opt/esa/sootup/lib/*'`, and run them with `/opt/esa/tool-jdk/bin/java -Xmx1024m -cp '/workspace/diagnostics/classes:/opt/esa/sootup/lib/*' Analysis`. The SDK includes CHA/RTA callgraph APIs; define explicit entry methods and mark product inputs as Application and dependencies/JDK libraries as Library. See `profiles/java-debug/SKILL.md`. This uses the existing runtime-granted `run` capability; it adds no service, agent role or permissions. Static results remain diagnostic evidence, not a runtime reproduction or verified source mapping.

## Check and update

```sh
python3 scripts/update_tool_images.py --context colima-redmine
```

The default command checks official releases and builds only when release/configuration fingerprints change. It:

1. Pulls the official Eclipse Temurin GA JDK 8 and 21 images for Ubuntu Noble; fixes their repository digests. Major JVM versions and the OS family remain pinned.
2. Resolves declared packages, dependency installation plans and base security updates from the image's signed stable Ubuntu repositories in an automatically removed resolver container.
3. Selects the highest numeric non-draft, non-prerelease async-profiler release from its official GitHub repository. Nightly/RC tags are excluded. Requires the publisher's asset SHA256 and verifies downloads.
4. Resolves the highest numeric stable `org.soot-oss:soot` release from Maven Central; downloads its bundled JAR and verifies the publisher's SHA256. Snapshot/RC artifacts are excluded.
5. Resolves the latest numeric stable SootUp release from Maven Central. An official digest-pinned Maven builder resolves runtime dependencies with a Central-only mirror, no host mounts, and automatic container removal. Each JAR is checked against the strongest published Central checksum (SHA256 or SHA1); a measured SHA256 for every JAR and the builder image digest are retained for locked rebuilds. Transitive JARs are cached; Maven is not installed in the engineering runtime or on the host.
6. Builds one shared toolkit image, compiles the small ESA SootUp launcher, and checks commands and JVM/profiler/Soot/SootUp versions offline as a nonroot user.
7. Publishes `.esa/tool-images/current.json` only after inspection passes, and writes the shared `deploy/tools/toolset.release.txt`. Running tasks retain their immutable image IDs. Host packages and live Redmine containers are unchanged.

For a check without building or activating a new registry:

```sh
python3 scripts/update_tool_images.py --context colima-redmine --check
```

For inspection/rebuilding with the last verified release lock, without selecting new upstream releases:

```sh
python3 scripts/update_tool_images.py --context colima-redmine --locked
```

An unchanged update still verifies the images and returns `current`. Release locks retain architecture and package versions; unavailable locked packages fail rather than silently choosing substitutes. An older lock without Soot or SootUp initializes that newly declared tool from the official stable repository once, retaining the other existing locks. SootUp dependency checksums and selected versions are listed in the shared release TXT; future locked builds reuse those exact JARs. Transient metadata/checksum connection failures have bounded retries. Ubuntu's stable repository candidates may contain distribution-specific version suffixes; they are distribution releases rather than promises of the newest upstream source release. Network/update/build failures leave the previous active registry in place. Old image layers are retained for existing runs; removal is an explicit operator retention task, not automatic pruning.

Official provenance: [Temurin container images](https://adoptium.net/installation/containers/), [Docker official Temurin manifest](https://github.com/docker-library/official-images/blob/master/library/eclipse-temurin), [async-profiler releases](https://github.com/async-profiler/async-profiler/releases).

## Use and lifecycle

The Agent selects an approved Profile before the registered runtime is created; source-only analysis needs no container. All prepared Profiles currently use the shared toolkit. `scripts/replay_session.py --profile <name>` or `Environment(profile=<name>)` selects another verified image. Image IDs, release/config hashes and actual tools enter readiness/audit. The Agent gets a compact readiness list; complete package inventory stays in the private environment report and TXT release inspection.

Runtime containers use `--rm`, an offline network, read-only root/product mounts, UID 1000, dropped capabilities and CPU/memory/process limits. Writable tools/scripts remain in scratch `tmpfs`. Containers remove automatically on exit; owned named data volumes and crash recovery still have explicit cleanup. Nothing uninstalls individual tools or modifies a running image.

Tool installation is not permission to use privileged kernel facilities. CPU sampling, debugger attach or packet capture can be restricted; use permitted modes or return an environment request. Preinstalled binaries under `/opt` can execute without relying on scratch mount execution permissions. Extra shell tools in scratch use an explicit interpreter.

## Validate

```sh
python3 -B scripts/test_tool_images.py
python3 scripts/verify_tool_images.py
```

The first command checks release filtering, checksum requirements, archive bounds and Docker-context isolation without network/Docker. The second verifies the shared real image for all Profiles, generates Jimple/CFG from frozen fixture bytecode, compiles and runs a custom SootUp CHA analysis offline, checks product source/class immutability and confirms removal of the runtime/writer/data volume. It uses no engineering model calls.

`python3 -B scripts/verify_profile_runtime.py --evidence /absolute/registered/evidence.json` also compiles and runs an auxiliary Java program, then uses the Agent tool gateway to generate Jimple with Soot and Jimple/CFG with SootUp from the frozen Kafka `NetworkClient` class, and checks that its bytecode remains unchanged.

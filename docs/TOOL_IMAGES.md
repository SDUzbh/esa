# Preset tool images

Each Profile has a declarative `profiles/<profile>/toolset.txt` and an inspected `toolset.release.txt` containing installed versions, image IDs, upstream hashes and executable paths. Profile version 2 includes these declarations in its approved manifest hash. Do not execute TXT files as shell scripts.

Five images (`esa-tools-general`, `esa-tools-java-debug`, `esa-tools-performance`, `esa-tools-compatibility`, `esa-tools-configuration`) share one immutable toolkit layer. Each image contains the union of the declared tools, with its own Profile label and bundled configuration. Profile TXT files describe the recommended subset; capability policy remains separate from package presence. Sharing the installed layer allows the same Agent session to request another approved capability without recreating its environment.

| Profile | Prepared tools |
| --- | --- |
| General | Python, ripgrep, jq, Git, curl, file and binary inspection |
| Java debugging | JDK 8 java/javac/jdb/jcmd/jstack/jmap/javap/jdeps, GDB, strace, lsof |
| Performance | async-profiler, sysstat, vmstat, pidstat, iostat, time, JFR tools |
| Compatibility | javap/jdeps, diff, readelf/objdump, unzip, separate analysis JVM |
| Configuration | ip/ss, dig, tcpdump, netcat, curl/jq |

The default registered Java fixture uses the JDK 8 runtime. JDK 21 analysis/JFR tools are separately installed at `/opt/esa/tool-jdk`; they never replace the default `JAVA_HOME` or PATH's product Java. Customer source, registered product dependencies and frozen baseline classes are not upgraded. This preset is a current analysis environment, not proof of an exact historical JVM patch level. An explicitly supplied historical image cannot be overwritten by a preset selection.

## Check and update

```sh
python3 scripts/update_tool_images.py --context colima-redmine
```

The default command checks official releases and builds only when release/configuration fingerprints change. It:

1. Pulls the official Eclipse Temurin GA JDK 8 and 21 images for Ubuntu Noble; fixes their repository digests. Major JVM versions and the OS family remain pinned.
2. Resolves declared packages, dependency installation plans and base security updates from the image's signed stable Ubuntu repositories in an automatically removed resolver container.
3. Selects the highest numeric non-draft, non-prerelease async-profiler release from its official GitHub repository. Nightly/RC tags are excluded. Requires the publisher's asset SHA256 and verifies downloads.
4. Builds a shared toolkit and five thin Profile images; checks commands and JVM/profiler versions offline as a nonroot user.
5. Publishes `.esa/tool-images/current.json` only after every image passes, and writes each Profile's inspected `toolset.release.txt`. Running tasks retain their immutable image IDs. Host packages and live Redmine containers are unchanged.

For a check without building or activating a new registry:

```sh
python3 scripts/update_tool_images.py --context colima-redmine --check
```

For inspection/rebuilding with the last verified release lock, without selecting new upstream releases:

```sh
python3 scripts/update_tool_images.py --context colima-redmine --locked
```

An unchanged update still verifies the images and returns `current`. Release locks retain architecture and package versions; unavailable locked packages fail rather than silently choosing substitutes. Ubuntu's stable repository candidates may contain distribution-specific version suffixes; they are distribution releases rather than promises of the newest upstream source release. Network/update/build failures leave the previous active registry in place. Old image layers are retained for existing runs; removal is an explicit operator retention task, not automatic pruning.

Official provenance: [Temurin container images](https://adoptium.net/installation/containers/), [Docker official Temurin manifest](https://github.com/docker-library/official-images/blob/master/library/eclipse-temurin), [async-profiler releases](https://github.com/async-profiler/async-profiler/releases).

## Use and lifecycle

Managed engineering environments now select the prepared `java-debug` image for the registered Java fixture. `scripts/replay_session.py --profile <name>` or `Environment(profile=<name>)` selects another verified image. Image IDs, release/config hashes and actual tools enter readiness/audit. The Agent gets a compact readiness list; complete package inventory stays in the private environment report and TXT release inspection.

Runtime containers use `--rm`, an offline network, read-only root/product mounts, UID 1000, dropped capabilities and CPU/memory/process limits. Writable tools/scripts remain in scratch `tmpfs`. Containers remove automatically on exit; owned named data volumes and crash recovery still have explicit cleanup. Nothing uninstalls individual tools or modifies a running image.

Tool installation is not permission to use privileged kernel facilities. CPU sampling, debugger attach or packet capture can be restricted; use permitted modes or return an environment request. Preinstalled binaries under `/opt` can execute without relying on scratch mount execution permissions. Extra shell tools in scratch use an explicit interpreter.

## Validate

```sh
python3 -B scripts/test_tool_images.py
python3 scripts/verify_tool_images.py
```

The first command checks release filtering, checksum requirements, archive bounds and Docker-context isolation without network/Docker. The second verifies all five real images, runs tools in an owned offline environment, checks product source immutability and confirms removal of the runtime/writer/data volume. It uses no engineering model calls.

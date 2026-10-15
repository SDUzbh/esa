ARG TARGET_IMAGE
ARG TOOL_JDK_IMAGE
FROM ${TOOL_JDK_IMAGE} AS tool_jdk
FROM ${TARGET_IMAGE} AS toolkit
ARG TOOLSET_HASH
ARG RELEASE_HASH
COPY packages.txt /opt/esa/packages.txt
RUN apt-get update && xargs -r apt-get install -y --no-install-recommends < /opt/esa/packages.txt && rm -rf /var/lib/apt/lists/*
COPY --from=tool_jdk /opt/java/openjdk /opt/esa/tool-jdk
COPY async-profiler/ /opt/esa/async-profiler/
COPY soot.jar /opt/esa/soot/soot.jar
COPY soot /usr/local/bin/soot
COPY sootup/ /opt/esa/sootup/
RUN mkdir /opt/esa/sootup/classes && /opt/esa/tool-jdk/bin/javac --release 17 -cp '/opt/esa/sootup/lib/*' -d /opt/esa/sootup/classes /opt/esa/sootup/SootUp.java && cp /opt/esa/sootup/sootup /usr/local/bin/sootup
COPY profiles/ /opt/esa/profiles/
COPY inspect_tools.py /opt/esa/inspect_tools.py
ENV ESA_TOOL_JAVA_HOME=/opt/esa/tool-jdk
ENV PATH="/opt/esa/async-profiler/bin:${PATH}"
RUN chmod -R a+rX /opt/esa && chmod 0555 /usr/local/bin/soot /usr/local/bin/sootup && python3 /opt/esa/inspect_tools.py --write /opt/esa/inventory.json
LABEL esa.toolkit="release" esa.toolset.hash="${TOOLSET_HASH}" esa.release.hash="${RELEASE_HASH}"
USER 1000:1000

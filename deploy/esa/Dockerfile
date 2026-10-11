FROM redmine:7.0.2-bookworm AS certificates
FROM scratch
COPY --from=certificates /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --chown=1000:1000 deploy/esa/seed/ /var/lib/esa/
COPY bin/esa /esa
COPY config/rules.json /etc/esa/rules.json
USER 1000:1000
ENTRYPOINT ["/esa", "-listen", "0.0.0.0:8090", "-store", "/var/lib/esa/state.json", "-rules", "/etc/esa/rules.json"]

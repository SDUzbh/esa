#!/bin/sh
# Starts a disposable Postgres for ESA's tests, which need one: ESA keeps its
# state only in Postgres. The server keeps its data in memory (tmpfs) and
# listens on 127.0.0.1 only. The connection URL goes to a private env file,
# never to the terminal:
#
#   sh scripts/test_postgres.sh && . .tools/test-postgres/env
#   sh scripts/test_postgres.sh stop
#
# DOCKER_CONTEXT selects the Docker daemon (for example colima-redmine).
# scripts/shutdown.py also removes the server, by its esa.test.database label.
set -eu
cd "$(dirname "$0")/.."
name=${ESA_TEST_POSTGRES_CONTAINER:-esa-test-postgres}
port=${ESA_TEST_POSTGRES_PORT:-55432}
dir=.tools/test-postgres
# Same image as the CI service.
image=postgres:17-alpine@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24

if [ "${1:-start}" = stop ]; then
    docker rm -f "$name" >/dev/null 2>&1 || true
    rm -rf "$dir"
    echo "Stopped $name."
    exit 0
fi

# A server whose password file is gone is unreachable; it is disposable, so
# replace it rather than pair it with a new password.
if [ ! -s "$dir/password" ] && docker inspect "$name" >/dev/null 2>&1; then
    docker rm -f "$name" >/dev/null
fi
mkdir -p "$dir"
chmod 700 "$dir"
if [ ! -s "$dir/password" ]; then
    (umask 077 && openssl rand -hex 24 >"$dir/password")
fi
password=$(cat "$dir/password")
if ! docker inspect "$name" >/dev/null 2>&1; then
    docker run -d --name "$name" --label esa.test.database=1 --tmpfs /var/lib/postgresql/data \
        -e POSTGRES_USER=esa -e POSTGRES_DB=esa_test -e POSTGRES_PASSWORD="$password" \
        -p "127.0.0.1:$port:5432" "$image" >/dev/null
fi
tries=0
until docker exec "$name" pg_isready -q -U esa -d esa_test; do
    tries=$((tries + 1))
    if [ "$tries" -gt 60 ]; then
        echo "Postgres did not become ready; see: docker logs $name" >&2
        exit 1
    fi
    sleep 1
done
(umask 077 && printf "export ESA_TEST_DATABASE_URL='postgres://esa:%s@127.0.0.1:%s/esa_test?sslmode=disable'\n" "$password" "$port" >"$dir/env")
echo "Postgres is ready in container $name. Load the test URL with: . $dir/env"

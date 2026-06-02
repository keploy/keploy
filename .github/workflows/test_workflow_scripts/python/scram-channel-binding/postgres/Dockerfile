FROM postgres:16

COPY server.crt /etc/ssl/private/server.crt
COPY server.key /etc/ssl/private/server.key
COPY pg_hba.conf /etc/postgresql/pg_hba.conf
COPY init.sql /docker-entrypoint-initdb.d/00-init.sql

RUN chown postgres:postgres \
        /etc/ssl/private/server.key \
        /etc/ssl/private/server.crt \
        /etc/postgresql/pg_hba.conf && \
    chmod 600 /etc/ssl/private/server.key && \
    chmod 644 /etc/ssl/private/server.crt /etc/postgresql/pg_hba.conf

# password_encryption=scram-sha-256 is the default in PG 14+ but we
# pin it explicitly so the e2e harness doesn't depend on the image's
# default ever changing.
CMD ["postgres", \
     "-c", "ssl=on", \
     "-c", "ssl_cert_file=/etc/ssl/private/server.crt", \
     "-c", "ssl_key_file=/etc/ssl/private/server.key", \
     "-c", "hba_file=/etc/postgresql/pg_hba.conf", \
     "-c", "password_encryption=scram-sha-256", \
     "-c", "log_connections=on"]

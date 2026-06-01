FROM postgres:16

COPY server.crt /etc/ssl/private/server.crt
COPY server.key /etc/ssl/private/server.key
COPY pg_hba.conf /etc/postgresql/pg_hba.conf

RUN chown postgres:postgres /etc/ssl/private/server.key /etc/ssl/private/server.crt /etc/postgresql/pg_hba.conf && \
    chmod 600 /etc/ssl/private/server.key && \
    chmod 644 /etc/ssl/private/server.crt /etc/postgresql/pg_hba.conf

CMD ["postgres", \
     "-c", "ssl=on", \
     "-c", "ssl_cert_file=/etc/ssl/private/server.crt", \
     "-c", "ssl_key_file=/etc/ssl/private/server.key", \
     "-c", "hba_file=/etc/postgresql/pg_hba.conf", \
     "-c", "password_encryption=scram-sha-256", \
     "-c", "log_connections=on", \
     "-c", "log_min_messages=info"]

CREATE SEQUENCE IF NOT EXISTS idemkit_token_seq AS BIGINT INCREMENT BY 1 START WITH 1 NO CYCLE;

CREATE TABLE IF NOT EXISTS idemkit_keys (
    key              TEXT        PRIMARY KEY,
    body_hash        BYTEA       NOT NULL,
    state            SMALLINT    NOT NULL,
    response_code    INT,
    response_headers JSONB,
    response_body    BYTEA,
    locked_at        TIMESTAMPTZ NOT NULL,
    completed_at     TIMESTAMPTZ,
    expires_at       TIMESTAMPTZ NOT NULL,
    token            BIGINT      NOT NULL
);

CREATE INDEX IF NOT EXISTS idemkit_keys_expires_at ON idemkit_keys (expires_at);

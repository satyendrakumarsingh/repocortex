CREATE TABLE transactions (
    id          VARCHAR(64) PRIMARY KEY,
    merchant_id VARCHAR(64) NOT NULL,
    status      VARCHAR(32) NOT NULL,
    amount      BIGINT      NOT NULL,
    created_at  TIMESTAMP   NOT NULL DEFAULT now(),
    updated_at  TIMESTAMP
);

CREATE INDEX idx_transactions_merchant ON transactions (merchant_id);
CREATE INDEX idx_transactions_status   ON transactions (status);

CREATE TABLE transaction_events (
    id             BIGSERIAL PRIMARY KEY,
    transaction_id VARCHAR(64) NOT NULL REFERENCES transactions(id),
    event_type     VARCHAR(32) NOT NULL,
    created_at     TIMESTAMP   NOT NULL DEFAULT now()
);

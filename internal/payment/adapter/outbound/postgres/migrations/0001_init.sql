-- One row per bill code handed out. Codes are reused over time, but at most
-- one live (pending/processing) intent may hold a code, and each POS bill has
-- at most one live intent.
CREATE TABLE payment_intents (
    id                  UUID PRIMARY KEY,
    bill_id             TEXT        NOT NULL,
    code                CHAR(6)     NOT NULL,
    created_by          TEXT        NOT NULL,
    restaurant_name     TEXT        NOT NULL,
    table_label         TEXT        NOT NULL DEFAULT '',
    merchant_wallet_id  TEXT        NOT NULL,
    items               JSONB       NOT NULL,
    total               BIGINT      NOT NULL CHECK (total > 0),
    status              TEXT        NOT NULL CHECK (status IN ('pending', 'processing', 'paid', 'expired')),
    payer_user_id       TEXT,
    payer_wallet_id     TEXT,
    pay_idempotency_key TEXT,
    transaction_id      TEXT,
    balance_after       BIGINT,
    paid_at             TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    CHECK (status <> 'paid' OR transaction_id IS NOT NULL)
);

CREATE UNIQUE INDEX payment_intents_live_code_key
    ON payment_intents (code) WHERE status IN ('pending', 'processing');
CREATE UNIQUE INDEX payment_intents_live_bill_key
    ON payment_intents (merchant_wallet_id, bill_id) WHERE status IN ('pending', 'processing');
CREATE INDEX payment_intents_code_idx ON payment_intents (code, created_at DESC);
CREATE INDEX payment_intents_bill_idx ON payment_intents (merchant_wallet_id, bill_id, created_at DESC);

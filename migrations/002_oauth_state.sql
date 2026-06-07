-- +goose Up
-- MCP OAuth CSRF state. Binds a single-use authorization state to the user who
-- started the flow (the provider callback can't carry the JWT). Short-lived;
-- the PKCE verifier is stored encrypted. Multi-DB compatible.

CREATE TABLE oauth_state (
    state        VARCHAR(128) PRIMARY KEY,
    user_id      VARCHAR(128) NOT NULL,
    app_id       VARCHAR(128) NOT NULL,
    provider     VARCHAR(128) NOT NULL,
    server_id    VARCHAR(128) NOT NULL,
    verifier     TEXT,
    nonce        VARCHAR(128),
    redirect_uri VARCHAR(512),
    expires_at   TIMESTAMP,
    created_at   TIMESTAMP
);

CREATE INDEX idx_oauth_state_user ON oauth_state(user_id);
CREATE INDEX idx_oauth_state_expires ON oauth_state(expires_at);

-- +goose Down
DROP TABLE oauth_state;

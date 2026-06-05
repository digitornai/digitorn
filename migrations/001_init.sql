-- +goose Up
-- Initial Digitorn schema. Multi-DB compatible (Postgres / MySQL / SQLite / MSSQL / Oracle).

CREATE TABLE app_deployments (
    id            CHAR(36) PRIMARY KEY,
    app_id        VARCHAR(128) NOT NULL,
    version       VARCHAR(64)  NOT NULL,
    yaml_compiled TEXT,
    deployed_by   VARCHAR(128),
    deployed_at   TIMESTAMP NOT NULL
);

CREATE INDEX idx_app_version ON app_deployments(app_id, version);

CREATE TABLE sessions (
    id             CHAR(36) PRIMARY KEY,
    app_id         VARCHAR(128) NOT NULL,
    agent_id       VARCHAR(128) NOT NULL,
    user_id        VARCHAR(128) NOT NULL,
    status         VARCHAR(32)  NOT NULL,
    message_count  INT NOT NULL DEFAULT 0,
    metadata       TEXT,
    created_at     TIMESTAMP NOT NULL,
    last_activity  TIMESTAMP
);

CREATE INDEX idx_session_user ON sessions(app_id, user_id, created_at);

CREATE TABLE messages (
    id            CHAR(36) PRIMARY KEY,
    session_id    CHAR(36) NOT NULL,
    role          VARCHAR(32) NOT NULL,
    content       TEXT,
    tool_calls    TEXT,
    tool_call_id  VARCHAR(128),
    turn_number   INT NOT NULL,
    tokens_in     INT DEFAULT 0,
    tokens_out    INT DEFAULT 0,
    created_at    TIMESTAMP NOT NULL
);

CREATE INDEX idx_message_session ON messages(session_id, created_at);

CREATE TABLE tool_calls (
    id           CHAR(36) PRIMARY KEY,
    session_id   CHAR(36) NOT NULL,
    turn_number  INT NOT NULL,
    module_id    VARCHAR(128) NOT NULL,
    tool_name    VARCHAR(128) NOT NULL,
    params       TEXT,
    success      BOOLEAN NOT NULL,
    result_data  TEXT,
    result_err   TEXT,
    duration_ms  BIGINT,
    executed_at  TIMESTAMP NOT NULL
);

CREATE INDEX idx_toolcall_session ON tool_calls(session_id, turn_number);

CREATE TABLE credentials (
    id          CHAR(36) PRIMARY KEY,
    user_id     VARCHAR(128) NOT NULL,
    provider    VARCHAR(128) NOT NULL,
    fields      TEXT,
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP
);

CREATE UNIQUE INDEX idx_cred_user_provider ON credentials(user_id, provider);

CREATE TABLE audit_log (
    id         BIGINT PRIMARY KEY,
    timestamp  TIMESTAMP NOT NULL,
    user_id    VARCHAR(128),
    action     VARCHAR(128) NOT NULL,
    module     VARCHAR(128),
    result     VARCHAR(32) NOT NULL,
    metadata   TEXT
);

CREATE INDEX idx_audit_user_ts ON audit_log(user_id, timestamp);

CREATE TABLE module_state (
    id          BIGINT PRIMARY KEY,
    module_id   VARCHAR(128) NOT NULL,
    app_id      VARCHAR(128) NOT NULL,
    state       VARCHAR(32) NOT NULL,
    config      TEXT,
    last_update TIMESTAMP
);

CREATE UNIQUE INDEX idx_modstate_module_app ON module_state(module_id, app_id);

-- +goose Down
DROP TABLE module_state;
DROP TABLE audit_log;
DROP TABLE credentials;
DROP TABLE tool_calls;
DROP TABLE messages;
DROP TABLE sessions;
DROP TABLE app_deployments;

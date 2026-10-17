CREATE TABLE operators (
    id text PRIMARY KEY,
    email text NOT NULL UNIQUE CHECK (email = lower(email) AND length(email) BETWEEN 3 AND 254),
    password_hash text NOT NULL,
    role text NOT NULL CHECK (role IN ('admin', 'viewer')),
    status text NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE TABLE operator_sessions (
    id text PRIMARY KEY,
    operator_id text NOT NULL REFERENCES operators(id),
    secret_hash bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    revoked_at timestamptz
);
CREATE INDEX operator_sessions_operator ON operator_sessions(operator_id);
CREATE INDEX operator_sessions_expiry ON operator_sessions(expires_at);
CREATE TABLE projects (
    id text PRIMARY KEY CHECK (id ~ '^[a-zA-Z0-9._:-]{1,128}$'),
    name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    status text NOT NULL CHECK (status IN ('enabled', 'disabled')),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX projects_status ON projects(status, id);
CREATE TABLE project_credentials (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects(id),
    key_identifier text NOT NULL UNIQUE,
    secret_hash bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
    status text NOT NULL CHECK (status IN ('active', 'revoked')),
    created_at timestamptz NOT NULL,
    expires_at timestamptz CHECK (expires_at IS NULL OR expires_at > created_at),
    revoked_at timestamptz,
    CHECK ((status = 'revoked') = (revoked_at IS NOT NULL))
);
CREATE INDEX project_credentials_project ON project_credentials(project_id, id);
CREATE TABLE audit_logs (
    id text PRIMARY KEY,
    actor_id text REFERENCES operators(id),
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text NOT NULL,
    timestamp timestamptz NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 2048)
);
CREATE INDEX audit_logs_actor ON audit_logs(actor_id, id);
CREATE INDEX audit_logs_action ON audit_logs(action, id);

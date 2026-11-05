-- Vehicle registry. Rows are upserted from telemetry on first sight; in production
-- this would be fed from the manufacturing or provisioning system.
CREATE TABLE vehicles (
    vehicle_id        TEXT PRIMARY KEY,
    vin               TEXT NOT NULL,
    model             TEXT NOT NULL,
    region            TEXT NOT NULL,
    pack_capacity_kwh DOUBLE PRECISION NOT NULL,
    software_version  TEXT NOT NULL,
    first_seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Diagnostics double as a transactional outbox. The engine writes rows and the alert
-- service consumes them where processed_at IS NULL, so no message broker is needed
-- between the two services and no diagnostic can be lost between them.
CREATE TABLE diagnostics (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    subject             TEXT NOT NULL,
    category            TEXT NOT NULL,
    transition          TEXT NOT NULL CHECK (transition IN ('opened', 'updated', 'cleared')),
    severity            TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    confidence          DOUBLE PRECISION NOT NULL,
    deviation_score     DOUBLE PRECISION NOT NULL,
    reason              TEXT NOT NULL,
    likely_cause        TEXT NOT NULL,
    evidence            JSONB NOT NULL,
    recommended_actions JSONB NOT NULL,
    software_version    TEXT NOT NULL DEFAULT '',
    observed_at         TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at        TIMESTAMPTZ
);
CREATE INDEX diagnostics_unprocessed ON diagnostics (id) WHERE processed_at IS NULL;
CREATE INDEX diagnostics_subject ON diagnostics (subject, id DESC);
CREATE INDEX diagnostics_recent ON diagnostics (id DESC);

CREATE TABLE alerts (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    subject             TEXT NOT NULL,
    category            TEXT NOT NULL,
    title               TEXT NOT NULL,
    level               TEXT NOT NULL CHECK (level IN ('info', 'warning', 'critical')),
    status              TEXT NOT NULL CHECK (status IN ('active', 'acknowledged', 'resolved')),
    reason              TEXT NOT NULL,
    likely_cause        TEXT NOT NULL,
    confidence          DOUBLE PRECISION NOT NULL,
    recommended_actions JSONB NOT NULL,
    occurrences         INTEGER NOT NULL DEFAULT 1,
    first_diagnostic_id BIGINT NOT NULL REFERENCES diagnostics (id),
    last_diagnostic_id  BIGINT NOT NULL REFERENCES diagnostics (id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    acknowledged_at     TIMESTAMPTZ,
    acknowledged_by     TEXT,
    resolved_at         TIMESTAMPTZ,
    resolved_by         TEXT
);
-- One open alert per problem, enforced by the database rather than by application
-- logic, so concurrent writers cannot create duplicates.
CREATE UNIQUE INDEX alerts_one_open ON alerts (subject, category) WHERE status <> 'resolved';
CREATE INDEX alerts_status_time ON alerts (status, created_at DESC);
CREATE INDEX alerts_subject ON alerts (subject, created_at DESC);
CREATE INDEX alerts_created ON alerts (created_at DESC);

-- Append-only audit trail of everything that happened to an alert.
CREATE TABLE alert_events (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    alert_id   BIGINT NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    actor      TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX alert_events_alert ON alert_events (alert_id, id);

CREATE TABLE fleet_statistics (
    captured_at TIMESTAMPTZ NOT NULL,
    metric      TEXT NOT NULL,
    cohort      TEXT NOT NULL,
    median      DOUBLE PRECISION NOT NULL,
    mad         DOUBLE PRECISION NOT NULL,
    samples     BIGINT NOT NULL,
    PRIMARY KEY (captured_at, metric, cohort)
);

-- One-minute per-vehicle rollups. Raw telemetry deliberately stays out of Postgres:
-- at 10k vehicles it would be billions of rows a month for little diagnostic value.
-- Daily partitions make retention a cheap DROP TABLE instead of a large DELETE.
CREATE TABLE telemetry_rollups (
    vehicle_id       TEXT NOT NULL,
    bucket           TIMESTAMPTZ NOT NULL,
    samples          INTEGER NOT NULL,
    battery_temp_c   DOUBLE PRECISION NOT NULL,
    motor_temp_c     DOUBLE PRECISION NOT NULL,
    ambient_temp_c   DOUBLE PRECISION NOT NULL,
    soc_pct          DOUBLE PRECISION NOT NULL,
    speed_kph        DOUBLE PRECISION NOT NULL,
    pack_power_kw    DOUBLE PRECISION NOT NULL,
    battery_residual DOUBLE PRECISION,
    PRIMARY KEY (vehicle_id, bucket)
) PARTITION BY RANGE (bucket);

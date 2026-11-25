-- Canonical teams. team_id comes from a global identity that never accepts
-- an explicit value and is never reused; team_name is unique ignoring case
-- across active and archived teams.
CREATE TABLE {{schema}}.teams (
    team_id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team_name     text        NOT NULL,
    default_image text        NOT NULL,
    ingested_at   timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    archived_at   timestamptz
);

CREATE UNIQUE INDEX teams_team_name_ci ON {{schema}}.teams (lower(team_name));

-- One sequence for every stream message team-registry publishes.
CREATE SEQUENCE {{schema}}.publisher_seq AS bigint MINVALUE 1;

-- Committed stream messages; the relay publishes them in publisher_seq order
-- and sets sent_at only after the JetStream acknowledgement.
CREATE TABLE {{schema}}.outbox (
    publisher_seq bigint      PRIMARY KEY,
    subject       text        NOT NULL,
    headers       jsonb       NOT NULL,
    payload       bytea       NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    sent_at       timestamptz
);

CREATE INDEX outbox_pending ON {{schema}}.outbox (publisher_seq) WHERE sent_at IS NULL;

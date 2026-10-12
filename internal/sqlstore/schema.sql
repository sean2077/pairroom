-- sqlstore schema version 1 (see docs/design/enterprise-service.md §13).
-- One schema belongs to exactly one immutable deployment/org; every Store
-- connection pins search_path to this schema.

CREATE TABLE schema_meta (
  id      boolean PRIMARY KEY DEFAULT true CHECK (id),
  version integer NOT NULL
);

CREATE TABLE deployment (
  id            boolean PRIMARY KEY DEFAULT true CHECK (id),
  deployment_id text NOT NULL UNIQUE CHECK (deployment_id <> ''),
  org_id        text NOT NULL CHECK (org_id <> '')
);

CREATE TABLE ownership (
  id                  boolean PRIMARY KEY DEFAULT true CHECK (id),
  instance_id         text NOT NULL CHECK (instance_id <> ''),
  epoch               bigint NOT NULL CHECK (epoch > 0),
  lease_pid           integer NOT NULL,
  lease_backend_start timestamptz NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
  user_id  text PRIMARY KEY CHECK (user_id <> ''),
  disabled boolean NOT NULL DEFAULT false
);

CREATE TABLE rooms (
  room_id     text PRIMARY KEY CHECK (room_id <> ''),
  host_mode   text NOT NULL DEFAULT 'native' CHECK (host_mode = 'native'),
  current_seq bigint NOT NULL DEFAULT 0 CHECK (current_seq >= 0),
  revision    bigint NOT NULL DEFAULT 0 CHECK (revision >= 0)
);

CREATE TABLE room_members (
  room_id  text NOT NULL REFERENCES rooms,
  user_id  text NOT NULL REFERENCES users,
  role     text NOT NULL CHECK (role IN ('owner','member','viewer')),
  disabled boolean NOT NULL DEFAULT false,
  PRIMARY KEY (room_id, user_id)
);

CREATE TABLE room_events (
  room_id        text NOT NULL REFERENCES rooms,
  seq            bigint NOT NULL CHECK (seq > 0),
  event_id       text NOT NULL CHECK (event_id <> ''),
  fact_version   integer NOT NULL,
  event_encoding bytea NOT NULL CHECK (octet_length(event_encoding) > 0 AND octet_length(event_encoding) <= 2097152),
  kind           text NOT NULL CHECK (kind <> ''),
  actor          text NOT NULL CHECK (actor <> ''),
  created_at     timestamptz NOT NULL,      -- derived index projection (microsecond precision); authoritative ns value lives in event_encoding
  origin_user    text NOT NULL REFERENCES users(user_id),
  PRIMARY KEY (room_id, seq),
  UNIQUE (room_id, event_id)
);

CREATE TABLE operations (
  op_namespace      text NOT NULL CHECK (op_namespace = 'human'),
  deployment_id     text NOT NULL REFERENCES deployment(deployment_id),
  room_id           text NOT NULL,
  binding_id        text NOT NULL DEFAULT '' CHECK (binding_id = ''),
  generation        bigint NOT NULL DEFAULT 0 CHECK (generation = 0),
  op_kind           text NOT NULL CHECK (op_kind IN ('append_fact', 'human_send')),
  identifier        text NOT NULL CHECK (identifier <> ''),
  principal         text NOT NULL CHECK (principal <> ''),
  intent_hash       bytea NOT NULL CHECK (octet_length(intent_hash) = 32),
  expected_revision bigint NOT NULL CHECK (expected_revision >= 0),
  result_seq        bigint NOT NULL,
  result_revision   bigint NOT NULL CHECK (result_revision >= 0),
  committed_at      timestamptz NOT NULL,
  FOREIGN KEY (room_id, result_seq) REFERENCES room_events(room_id, seq),
  PRIMARY KEY (op_namespace, deployment_id, room_id, binding_id, generation, op_kind, identifier, principal)
);

CREATE TABLE audit (
  audit_seq     bigserial PRIMARY KEY,
  deployment_id text NOT NULL REFERENCES deployment(deployment_id),
  room_id       text REFERENCES rooms,
  user_id       text NOT NULL REFERENCES users(user_id),
  kind          text NOT NULL CHECK (kind <> ''),
  outcome       text NOT NULL CHECK (outcome <> ''),
  detail        text NOT NULL DEFAULT '',
  at            timestamptz NOT NULL DEFAULT now()
);
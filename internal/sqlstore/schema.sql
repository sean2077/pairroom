-- sqlstore schema version 2 (see docs/design/enterprise-service.md §13).
-- One schema belongs to exactly one immutable deployment/org; every Store
-- connection pins search_path to this schema.
--
-- Version 1 is not readable by this build: the enterprise control model below
-- is additive on the event/operation tables and the old version is refused
-- before any state is opened (no real deployment exists yet).
--
-- Constraint, index and trigger names are stable on purpose: evidence cites
-- the exact sqlstate and constraint name. Every trigger function is owned by
-- this schema and runs with `search_path = pg_catalog`; it reaches
-- application tables only through TG_TABLE_SCHEMA-qualified dynamic SQL, so a
-- caller's search_path can never redirect a check to a different table.

CREATE TABLE schema_meta (
  id      boolean PRIMARY KEY DEFAULT true CHECK (id),
  version integer NOT NULL
);

-- The organization catalog. A row here is a directory entry and authorizes
-- nothing by itself.
CREATE TABLE organizations (
  org_id     text PRIMARY KEY CHECK (org_id <> ''),
  created_at timestamptz NOT NULL DEFAULT now(),
  disabled   boolean NOT NULL DEFAULT false
);

-- The single configured organization anchor: this deployment serves exactly
-- one organization, and every resource row references this one row, so a
-- self-consistent graph for another organization cannot be inserted at all.
CREATE TABLE configured_org (
  id            boolean PRIMARY KEY DEFAULT true CHECK (id),
  org_id        text NOT NULL UNIQUE REFERENCES organizations(org_id),
  configured_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE deployment (
  id            boolean PRIMARY KEY DEFAULT true CHECK (id),
  deployment_id text NOT NULL UNIQUE CHECK (deployment_id <> ''),
  org_id        text NOT NULL UNIQUE REFERENCES configured_org(org_id)
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
  user_id      text PRIMARY KEY CHECK (user_id <> ''),
  org_id       text NOT NULL,
  CONSTRAINT users_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  disabled     boolean NOT NULL DEFAULT false,
  tombstoned_at timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT users_org_key UNIQUE (org_id, user_id),
  -- A tombstoned user is always disabled, so the existing kernel, which only
  -- consults `disabled`, stays fail-closed for a tombstone.
  CONSTRAINT users_tombstone_check CHECK (tombstoned_at IS NULL OR disabled)
);

-- Trust connections: an issuer is part of the connection identity. Changing
-- an issuer requires a new connection; existing identities are never silently
-- repointed.
CREATE TABLE trust_connections (
  connection_id text PRIMARY KEY CHECK (connection_id <> ''),
  org_id        text NOT NULL,
  CONSTRAINT trust_connections_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  issuer        text NOT NULL CHECK (issuer <> ''),
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT trust_connections_triple_key UNIQUE (org_id, connection_id, issuer)
);

-- Exact identity mapping: a configured connection, the exact issuer it names
-- and the exact subject. Email addresses are not identities and appear in no
-- uniqueness rule. The whole key, including its User, is immutable and the
-- mapping is retired, never repointed or reclaimed.
CREATE TABLE external_identities (
  identity_id   text PRIMARY KEY CHECK (identity_id <> ''),
  org_id        text NOT NULL,
  CONSTRAINT external_identities_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  connection_id text NOT NULL,
  issuer        text NOT NULL CHECK (issuer <> ''),
  subject       text NOT NULL CHECK (subject <> ''),
  user_id       text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  retired_at    timestamptz,
  retired_reason text,
  CONSTRAINT external_identities_subject_key UNIQUE (org_id, connection_id, issuer, subject),
  CONSTRAINT external_identities_connection_fkey FOREIGN KEY (org_id, connection_id, issuer)
    REFERENCES trust_connections(org_id, connection_id, issuer),
  CONSTRAINT external_identities_user_fkey FOREIGN KEY (org_id, user_id) REFERENCES users(org_id, user_id)
);

CREATE TABLE devices (
  device_id  text PRIMARY KEY CHECK (device_id <> ''),
  org_id     text NOT NULL,
  CONSTRAINT devices_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  user_id    text NOT NULL,
  disabled   boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  retired_at timestamptz,
  retired_reason text,
  CONSTRAINT devices_org_key UNIQUE (org_id, device_id),
  CONSTRAINT devices_user_fkey FOREIGN KEY (org_id, user_id) REFERENCES users(org_id, user_id)
);

-- One canonical native agent per (device, opaque stable source key). The key
-- is chosen by the endpoint and reveals no raw vendor session or path; the
-- Runtime kind is recorded at join time and authorizes nothing. Two devices
-- may carry the same opaque key.
CREATE TABLE agents (
  agent_id          text PRIMARY KEY CHECK (agent_id <> ''),
  org_id            text NOT NULL,
  CONSTRAINT agents_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  device_id         text NOT NULL,
  stable_source_key text NOT NULL CHECK (stable_source_key <> ''),
  runtime_kind      text,
  runtime_recorded_at timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  retired_at        timestamptz,
  retired_reason    text,
  CONSTRAINT agents_source_key UNIQUE (org_id, device_id, stable_source_key),
  CONSTRAINT agents_org_key UNIQUE (org_id, agent_id),
  CONSTRAINT agents_device_fkey FOREIGN KEY (org_id, device_id) REFERENCES devices(org_id, device_id)
);

-- Logical Projects are optional Room scopes: a Room names at most one Project
-- and several Rooms may share one. Knowledge of a Project ID authorizes
-- nothing.
CREATE TABLE projects (
  project_id  text PRIMARY KEY CHECK (project_id <> ''),
  org_id      text NOT NULL,
  CONSTRAINT projects_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  archived_at timestamptz,
  CONSTRAINT projects_org_key UNIQUE (org_id, project_id)
);

CREATE TABLE rooms (
  room_id       text PRIMARY KEY CHECK (room_id <> ''),
  org_id        text NOT NULL,
  CONSTRAINT rooms_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  host_mode     text NOT NULL DEFAULT 'native' CHECK (host_mode = 'native'),
  owner_user_id text NOT NULL,
  project_id    text,
  current_seq   bigint NOT NULL DEFAULT 0 CHECK (current_seq >= 0),
  revision      bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
  created_at    timestamptz NOT NULL DEFAULT now(),
  archived_at   timestamptz,
  CONSTRAINT rooms_org_key UNIQUE (org_id, room_id),
  CONSTRAINT rooms_owner_fkey FOREIGN KEY (org_id, owner_user_id) REFERENCES users(org_id, user_id),
  CONSTRAINT rooms_project_fkey FOREIGN KEY (org_id, project_id) REFERENCES projects(org_id, project_id)
);

CREATE TABLE room_members (
  room_id  text NOT NULL,
  user_id  text NOT NULL,
  org_id   text NOT NULL,
  CONSTRAINT room_members_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  role     text NOT NULL CHECK (role IN ('owner','member','viewer')),
  disabled boolean NOT NULL DEFAULT false,
  PRIMARY KEY (room_id, user_id),
  CONSTRAINT room_members_room_fkey FOREIGN KEY (org_id, room_id) REFERENCES rooms(org_id, room_id),
  CONSTRAINT room_members_user_fkey FOREIGN KEY (org_id, user_id) REFERENCES users(org_id, user_id)
);

-- At most one live owner-role membership per Room; the durable owner record in
-- `rooms` stays the authority and this row may only carry the role for it.
CREATE UNIQUE INDEX room_members_owner_key ON room_members (room_id) WHERE role = 'owner' AND NOT disabled;

-- Immutable generations: a replacement appends generation + 1; a retired
-- generation row is never updated or deleted, so the historical binding stays
-- referable.
CREATE TABLE slot_generations (
  org_id     text NOT NULL,
  CONSTRAINT slot_generations_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  room_id    text NOT NULL,
  slot       text NOT NULL CHECK (slot IN ('slot1','slot2')),
  generation bigint NOT NULL CHECK (generation > 0),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (room_id, slot, generation),
  CONSTRAINT slot_generations_org_key UNIQUE (org_id, room_id, slot, generation),
  CONSTRAINT slot_generations_room_fkey FOREIGN KEY (org_id, room_id) REFERENCES rooms(org_id, room_id)
);

-- The stable slot head. It starts at the first generation and only ever holds
-- or advances by one; it points at an immutable generation row and never at a
-- row a binding could advance.
CREATE TABLE room_slots (
  room_id            text NOT NULL,
  slot               text NOT NULL CHECK (slot IN ('slot1','slot2')),
  current_generation bigint NOT NULL CHECK (current_generation > 0),
  PRIMARY KEY (room_id, slot),
  CONSTRAINT room_slots_head_fkey FOREIGN KEY (room_id, slot, current_generation)
    REFERENCES slot_generations(room_id, slot, generation) DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE bindings (
  binding_id  text PRIMARY KEY CHECK (binding_id <> ''),
  org_id      text NOT NULL,
  CONSTRAINT bindings_org_anchor_fkey FOREIGN KEY (org_id) REFERENCES configured_org(org_id),
  room_id     text NOT NULL,
  slot        text NOT NULL CHECK (slot IN ('slot1','slot2')),
  generation  bigint NOT NULL CHECK (generation > 0),
  agent_id    text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  retired_at  timestamptz,
  retired_reason text,
  -- A binding references the immutable historical generation, never the
  -- advancing head, so a retired binding never blocks a legal replacement.
  CONSTRAINT bindings_generation_fkey FOREIGN KEY (room_id, slot, generation)
    REFERENCES slot_generations(room_id, slot, generation),
  -- One binding per generation per slot: a new binding ID can never reuse a
  -- retired generation.
  CONSTRAINT bindings_generation_key UNIQUE (org_id, room_id, slot, generation),
  CONSTRAINT bindings_agent_fkey FOREIGN KEY (org_id, agent_id) REFERENCES agents(org_id, agent_id)
);

-- One live binding per slot and one live reservation per agent, across every
-- Room including archived ones: archiving does not retire a binding, and only
-- an explicit retire writes retired_at.
CREATE UNIQUE INDEX bindings_active_slot_key ON bindings (room_id, slot) WHERE retired_at IS NULL;
CREATE UNIQUE INDEX bindings_active_agent_key ON bindings (agent_id) WHERE retired_at IS NULL;

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

-- ---------------------------------------------------------------------------
-- Guard functions. Each is owned by this schema and pins search_path so an
-- application table is never resolved through a caller's search_path.

-- Fully immutable rows: reject UPDATE and DELETE outright.
CREATE FUNCTION control_reject_change() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $fn$
BEGIN
  RAISE EXCEPTION USING
    ERRCODE = '23514',
    CONSTRAINT = TG_TABLE_NAME || '_immutable',
    MESSAGE = format('%s rows are immutable', TG_TABLE_NAME),
    DETAIL = format('%s on %s is not a supported state transition', TG_OP, TG_TABLE_NAME);
END
$fn$;

-- Rows that may never be deleted, but may change in supported ways.
CREATE FUNCTION control_reject_delete() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $fn$
BEGIN
  RAISE EXCEPTION USING
    ERRCODE = '23514',
    CONSTRAINT = TG_TABLE_NAME || '_no_delete',
    MESSAGE = format('%s rows are retained, never deleted', TG_TABLE_NAME),
    DETAIL = 'retire or disable the row instead of deleting it';
END
$fn$;

-- Identity columns that must never be relabeled in place. TG_ARGV[0] is the
-- constraint name, the remaining arguments are the guarded columns.
CREATE FUNCTION control_guard_columns() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $fn$
DECLARE
  idx integer;
  col text;
BEGIN
  IF TG_NARGS < 2 THEN
    RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'control_guard_columns needs a constraint name and at least one column';
  END IF;
  FOR idx IN 1..(TG_NARGS - 1) LOOP
    col := TG_ARGV[idx];
    IF (to_jsonb(OLD) ->> col) IS DISTINCT FROM (to_jsonb(NEW) ->> col) THEN
      RAISE EXCEPTION USING
        ERRCODE = '23514',
        CONSTRAINT = TG_ARGV[0],
        MESSAGE = format('%s.%s is immutable', TG_TABLE_NAME, col),
        DETAIL = format('%s cannot be relabeled in place', col);
    END IF;
  END LOOP;
  RETURN NEW;
END
$fn$;

-- Retirement is write-once and named: retired_at may go from absent to set,
-- never back, and a retirement must state a non-blank reason. The guard runs
-- on every entry point, so a row inserted already-retired cannot skip it.
CREATE FUNCTION control_guard_retired() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $fn$
DECLARE
  was text;
  now text := to_jsonb(NEW) ->> 'retired_at';
  reason text := to_jsonb(NEW) ->> 'retired_reason';
BEGIN
  IF TG_OP = 'UPDATE' THEN
    was := to_jsonb(OLD) ->> 'retired_at';
  END IF;
  IF was IS NOT NULL AND now IS DISTINCT FROM was THEN
    RAISE EXCEPTION USING
      ERRCODE = '23514',
      CONSTRAINT = TG_TABLE_NAME || '_retired_once',
      MESSAGE = format('%s.retired_at is write-once', TG_TABLE_NAME);
  END IF;
  -- Use the Unicode White_Space set explicitly: default btrim removes only
  -- ASCII spaces and a locale-dependent character class can omit NBSP/NEL.
  IF now IS NOT NULL AND (reason IS NULL OR btrim(reason, U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000') = '') THEN
    RAISE EXCEPTION USING
      ERRCODE = '23514',
      CONSTRAINT = TG_TABLE_NAME || '_retired_reason',
      MESSAGE = format('%s retirement must state a non-blank reason', TG_TABLE_NAME);
  END IF;
  RETURN NEW;
END
$fn$;

-- A slot head is created at generation one; only the update guard advances it.
CREATE FUNCTION room_slots_insert_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $fn$
BEGIN
  IF NEW.current_generation <> 1 THEN
    RAISE EXCEPTION USING
      ERRCODE = '23514',
      CONSTRAINT = 'room_slots_head_check',
      MESSAGE = format('slot %s was created at generation %s', NEW.slot, NEW.current_generation),
      DETAIL = 'a slot head starts at generation one and only advances by one';
  END IF;
  RETURN NEW;
END
$fn$;

-- The slot head only ever holds or advances by exactly one generation.
CREATE FUNCTION room_slots_head_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $fn$
BEGIN
  IF NEW.current_generation IS DISTINCT FROM OLD.current_generation
     AND NEW.current_generation <> OLD.current_generation + 1 THEN
    RAISE EXCEPTION USING
      ERRCODE = '23514',
      CONSTRAINT = 'room_slots_head_check',
      MESSAGE = format('slot head %s advanced from %s to %s', NEW.slot, OLD.current_generation, NEW.current_generation),
      DETAIL = 'a slot head holds or advances by exactly one generation';
  END IF;
  RETURN NEW;
END
$fn$;

-- Deferred invariant: every live binding's generation equals its slot head.
-- The check takes the slot head row FOR UPDATE first and then reads the
-- committed-in-this-transaction state, so two concurrent writers cannot
-- produce a write skew and a legal single-transaction replacement that
-- retires, appends a generation, advances the head and inserts the new
-- binding is never rejected for its intermediate state.
CREATE FUNCTION bindings_active_generation_check() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $fn$
DECLARE
  room_id text := COALESCE(NEW.room_id, OLD.room_id);
  slot text := COALESCE(NEW.slot, OLD.slot);
  head bigint;
  stale bigint;
BEGIN
  -- The head row is locked first so two concurrent writers cannot skew.
  EXECUTE format('SELECT current_generation FROM %I.room_slots WHERE room_id = $1 AND slot = $2 FOR UPDATE', TG_TABLE_SCHEMA)
    INTO head USING room_id, slot;
  IF head IS NULL THEN
    RAISE EXCEPTION USING
      ERRCODE = '23514',
      CONSTRAINT = 'bindings_active_generation_check',
      MESSAGE = format('no slot head exists for room %s slot %s', room_id, slot);
  END IF;
  EXECUTE format('SELECT generation FROM %I.bindings WHERE room_id = $1 AND slot = $2 AND retired_at IS NULL AND generation <> $3 LIMIT 1', TG_TABLE_SCHEMA)
    INTO stale USING room_id, slot, head;
  IF stale IS NOT NULL THEN
    RAISE EXCEPTION USING
      ERRCODE = '23514',
      CONSTRAINT = 'bindings_active_generation_check',
      MESSAGE = format('live binding generation %s does not match slot head %s for room %s slot %s', stale, head, room_id, slot);
  END IF;
  RETURN NULL;
END
$fn$;

-- Deferred invariant, owned by both ends of the relation: the durable owner
-- record is authoritative and a membership row may carry the owner role only
-- for that user. An owner without a live owner-role membership keeps the
-- record.
CREATE FUNCTION owner_record_consistency_check() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $fn$
DECLARE
  room_id text := COALESCE(NEW.room_id, OLD.room_id);
  owner text;
  stray bigint;
BEGIN
  EXECUTE format('SELECT owner_user_id FROM %I.rooms WHERE room_id = $1 FOR UPDATE', TG_TABLE_SCHEMA)
    INTO owner USING room_id;
  IF owner IS NULL THEN
    RETURN NULL;
  END IF;
  EXECUTE format('SELECT count(*) FROM %I.room_members WHERE room_id = $1 AND role = $2 AND user_id <> $3', TG_TABLE_SCHEMA)
    INTO stray USING room_id, 'owner', owner;
  IF stray <> 0 THEN
    RAISE EXCEPTION USING
      ERRCODE = '23514',
      CONSTRAINT = 'owner_record_consistency_check',
      MESSAGE = format('room %s has an owner-role member that is not the durable owner', room_id);
  END IF;
  RETURN NULL;
END
$fn$;

-- ---------------------------------------------------------------------------
-- Guards.

CREATE TRIGGER configured_org_immutable BEFORE UPDATE OR DELETE ON configured_org
  FOR EACH ROW EXECUTE FUNCTION control_reject_change();
CREATE TRIGGER deployment_immutable BEFORE UPDATE OR DELETE ON deployment
  FOR EACH ROW EXECUTE FUNCTION control_reject_change();
CREATE TRIGGER slot_generations_immutable BEFORE UPDATE OR DELETE ON slot_generations
  FOR EACH ROW EXECUTE FUNCTION control_reject_change();
CREATE TRIGGER trust_connections_immutable BEFORE UPDATE OR DELETE ON trust_connections
  FOR EACH ROW EXECUTE FUNCTION control_reject_change();

CREATE TRIGGER users_no_delete BEFORE DELETE ON users
  FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER external_identities_no_delete BEFORE DELETE ON external_identities
  FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER devices_no_delete BEFORE DELETE ON devices
  FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER agents_no_delete BEFORE DELETE ON agents
  FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
-- The binding history and the slot heads are retained too: deleting either
-- would bypass the explicit retirement, the generation uniqueness and the
-- live-binding-equals-head invariant.
CREATE TRIGGER bindings_no_delete BEFORE DELETE ON bindings
  FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER room_slots_no_delete BEFORE DELETE ON room_slots
  FOR EACH ROW EXECUTE FUNCTION control_reject_delete();

CREATE TRIGGER users_identity_immutable BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('users_identity_immutable', 'user_id', 'org_id', 'created_at');
CREATE TRIGGER external_identities_identity_immutable BEFORE UPDATE ON external_identities
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('external_identities_identity_immutable', 'identity_id', 'org_id', 'connection_id', 'issuer', 'subject', 'user_id', 'created_at');
CREATE TRIGGER devices_identity_immutable BEFORE UPDATE ON devices
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('devices_identity_immutable', 'device_id', 'org_id', 'user_id', 'created_at');
CREATE TRIGGER agents_identity_immutable BEFORE UPDATE ON agents
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('agents_identity_immutable', 'agent_id', 'org_id', 'device_id', 'stable_source_key', 'created_at');
CREATE TRIGGER rooms_identity_immutable BEFORE UPDATE ON rooms
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('rooms_identity_immutable', 'room_id', 'org_id', 'host_mode', 'created_at');
CREATE TRIGGER room_slots_identity_immutable BEFORE UPDATE ON room_slots
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('room_slots_identity_immutable', 'room_id', 'slot');

CREATE TRIGGER external_identities_retired_once BEFORE INSERT OR UPDATE ON external_identities
  FOR EACH ROW EXECUTE FUNCTION control_guard_retired();
CREATE TRIGGER devices_retired_once BEFORE INSERT OR UPDATE ON devices
  FOR EACH ROW EXECUTE FUNCTION control_guard_retired();
CREATE TRIGGER agents_retired_once BEFORE INSERT OR UPDATE ON agents
  FOR EACH ROW EXECUTE FUNCTION control_guard_retired();
CREATE TRIGGER bindings_retired_once BEFORE INSERT OR UPDATE ON bindings
  FOR EACH ROW EXECUTE FUNCTION control_guard_retired();
CREATE TRIGGER bindings_identity_immutable BEFORE UPDATE ON bindings
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('bindings_identity_immutable', 'binding_id', 'org_id', 'room_id', 'slot', 'generation', 'agent_id', 'created_at');

CREATE TRIGGER room_slots_head_insert BEFORE INSERT ON room_slots
  FOR EACH ROW EXECUTE FUNCTION room_slots_insert_guard();
CREATE TRIGGER room_slots_head_check BEFORE UPDATE ON room_slots
  FOR EACH ROW EXECUTE FUNCTION room_slots_head_guard();

CREATE CONSTRAINT TRIGGER bindings_active_generation_check AFTER INSERT OR UPDATE ON bindings
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION bindings_active_generation_check();
CREATE CONSTRAINT TRIGGER room_slots_active_generation_check AFTER UPDATE ON room_slots
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION bindings_active_generation_check();
CREATE CONSTRAINT TRIGGER room_members_owner_record_check AFTER INSERT OR UPDATE OR DELETE ON room_members
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION owner_record_consistency_check();
CREATE CONSTRAINT TRIGGER rooms_owner_record_check AFTER UPDATE ON rooms
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION owner_record_consistency_check();

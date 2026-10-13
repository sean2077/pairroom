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
  version integer NOT NULL,
  fact_codec_version integer NOT NULL DEFAULT 2 CHECK(fact_codec_version=2),
  object_schema_version integer NOT NULL DEFAULT 1 CHECK(object_schema_version=1)
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
  CONSTRAINT trust_connections_triple_key UNIQUE (org_id, connection_id, issuer),
  CONSTRAINT trust_connections_issuer_key UNIQUE (connection_id, issuer)
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
  origin_kind    text NOT NULL CHECK(origin_kind IN ('user','operator')),
  origin_user    text REFERENCES users(user_id),
  CONSTRAINT room_events_origin_check CHECK((origin_kind='user' AND origin_user IS NOT NULL) OR (origin_kind='operator' AND origin_user IS NULL AND actor='system')),
  PRIMARY KEY (room_id, seq),
  UNIQUE (room_id, event_id)
);

CREATE TABLE operations (
  op_namespace      text NOT NULL CHECK (op_namespace = 'human'),
  deployment_id     text NOT NULL REFERENCES deployment(deployment_id),
  room_id           text NOT NULL,
  binding_id        text NOT NULL DEFAULT '' CHECK (binding_id = ''),
  generation        bigint NOT NULL DEFAULT 0 CHECK (generation = 0),
  op_kind           text NOT NULL CONSTRAINT operations_op_kind_check CHECK (op_kind IN ('append_fact', 'human_send', 'room_create', 'room_membership')),
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
  user_id       text REFERENCES users(user_id),
  actor_kind    text NOT NULL DEFAULT 'user' CHECK (actor_kind IN ('user', 'operator', 'client')),
  CONSTRAINT audit_actor_check CHECK ((actor_kind = 'user' AND user_id IS NOT NULL) OR (actor_kind IN ('operator', 'client') AND user_id IS NULL)),
  kind          text NOT NULL CHECK (kind <> ''),
  outcome       text NOT NULL CHECK (outcome <> ''),
  detail        text NOT NULL DEFAULT '',
  at            timestamptz NOT NULL DEFAULT now(),
  target_id text CHECK(target_id IS NULL OR target_id<>''),
  operation_id text CHECK(operation_id IS NULL OR operation_id ~ '^operation-[a-f0-9]{64}$'),
  invalidated_approval_count bigint CHECK(invalidated_approval_count IS NULL OR invalidated_approval_count>=0)
);

-- Deployment control has its own sequence and closed receipts. It never
-- manufactures a Room or puts authentication credentials in Room history.
CREATE TABLE identity_policy (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  config_hash bytea NOT NULL CHECK (octet_length(config_hash) = 32),
  bootstrap_connection text NOT NULL,
  bootstrap_issuer text NOT NULL,
  bootstrap_subject text NOT NULL,
  bootstrap_consumed_by text REFERENCES users(user_id),
  restore_epoch text NOT NULL CHECK (restore_epoch <> ''),
  revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
  FOREIGN KEY (bootstrap_connection, bootstrap_issuer) REFERENCES trust_connections(connection_id, issuer)
);

CREATE TABLE identity_admissions (
  connection_id text NOT NULL,
  issuer text NOT NULL,
  subject text NOT NULL CHECK (subject <> ''),
  enabled boolean NOT NULL DEFAULT true,
  PRIMARY KEY (connection_id, issuer, subject),
  FOREIGN KEY (connection_id, issuer) REFERENCES trust_connections(connection_id, issuer)
);

CREATE TABLE organization_admins (
  user_id text PRIMARY KEY REFERENCES users(user_id),
  enabled boolean NOT NULL DEFAULT true
);

CREATE TABLE browser_sessions (
  session_id text PRIMARY KEY CHECK (session_id <> ''),
  user_id text NOT NULL REFERENCES users(user_id),
  token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  csrf_hash bytea NOT NULL CHECK (octet_length(csrf_hash) = 32),
  restore_epoch text NOT NULL CHECK (restore_epoch <> ''),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz,
  CHECK (expires_at > created_at)
);
CREATE INDEX browser_sessions_user_key ON browser_sessions (user_id);

CREATE TABLE control_events (
  seq bigint PRIMARY KEY CHECK (seq > 0),
  event_id text NOT NULL UNIQUE CHECK (event_id <> ''),
  codec_version integer NOT NULL,
  kind text NOT NULL CHECK (kind <> ''),
  actor_user_id text REFERENCES users(user_id),
  receipt_encoding bytea NOT NULL CHECK (octet_length(receipt_encoding) > 0 AND octet_length(receipt_encoding) <= 16384),
  created_at timestamptz NOT NULL
);

CREATE TABLE control_operations (
  op_namespace text NOT NULL CHECK (op_namespace IN ('operator', 'oidc', 'human', 'device')),
  deployment_id text NOT NULL REFERENCES deployment(deployment_id),
  op_kind text NOT NULL CHECK (op_kind IN ('identity.configure', 'identity.login', 'session.revoke', 'identity.admission', 'user.status', 'administrator.set', 'device.enrollment.begin', 'device.enrollment.decide', 'device.enrollment.complete', 'device.enrollment.headless.decide', 'device.enrollment.headless.complete', 'device.rotate', 'device.recover', 'device.revoke', 'agent.register')),
  identifier text NOT NULL CHECK (identifier <> ''),
  principal text NOT NULL CHECK (principal <> ''),
  intent_hash bytea NOT NULL CHECK (octet_length(intent_hash) = 32),
  result_hash bytea NOT NULL CHECK (octet_length(result_hash) = 32),
  result_seq bigint NOT NULL REFERENCES control_events(seq),
  committed_at timestamptz NOT NULL,
  PRIMARY KEY (op_namespace, deployment_id, op_kind, identifier, principal)
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

CREATE FUNCTION browser_session_revoke_once() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $fn$
BEGIN
  IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
    RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'browser_sessions_revoked_once', MESSAGE = 'a revoked browser credential cannot be restored';
  END IF;
  RETURN NEW;
END
$fn$;

CREATE FUNCTION identity_bootstrap_once() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $fn$
BEGIN
  IF OLD.bootstrap_consumed_by IS NOT NULL AND NEW.bootstrap_consumed_by IS DISTINCT FROM OLD.bootstrap_consumed_by THEN
    RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'identity_bootstrap_consumed_once', MESSAGE = 'bootstrap consumption cannot be reset or repointed';
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
CREATE TRIGGER identity_policy_no_delete BEFORE DELETE ON identity_policy
  FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER identity_policy_configuration_immutable BEFORE UPDATE ON identity_policy
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('identity_policy_configuration_immutable', 'config_hash', 'bootstrap_connection', 'bootstrap_issuer', 'bootstrap_subject');
CREATE TRIGGER identity_bootstrap_once BEFORE UPDATE ON identity_policy
  FOR EACH ROW EXECUTE FUNCTION identity_bootstrap_once();
CREATE TRIGGER browser_sessions_no_delete BEFORE DELETE ON browser_sessions
  FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER browser_sessions_identity_immutable BEFORE UPDATE ON browser_sessions
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('browser_sessions_identity_immutable', 'session_id', 'user_id', 'token_hash', 'csrf_hash', 'restore_epoch', 'expires_at', 'created_at');
CREATE TRIGGER browser_sessions_revoked_once BEFORE UPDATE ON browser_sessions
  FOR EACH ROW EXECUTE FUNCTION browser_session_revoke_once();
CREATE TRIGGER identity_admissions_key_immutable BEFORE UPDATE ON identity_admissions
  FOR EACH ROW EXECUTE FUNCTION control_guard_columns('identity_admissions_key_immutable', 'connection_id', 'issuer', 'subject');

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

-- Batch Relay receipts retain the accepted cursor even when no Room fact was
-- emitted. Room history remains the only durable fact log.
CREATE TABLE relay_operations (
  op_namespace text NOT NULL CHECK (op_namespace IN ('human','binding')),
  deployment_id text NOT NULL REFERENCES deployment(deployment_id),
  room_id text NOT NULL REFERENCES rooms(room_id),
  binding_id text NOT NULL,
  generation bigint NOT NULL,
  op_kind text NOT NULL CHECK (op_kind <> ''),
  identifier text NOT NULL CHECK (identifier <> ''),
  principal text NOT NULL CHECK (principal <> ''),
  intent_hash bytea NOT NULL CHECK (octet_length(intent_hash)=32),
  result_hash bytea NOT NULL CHECK (octet_length(result_hash)=32),
  expected_revision bigint NOT NULL CHECK (expected_revision>=0),
  result_revision bigint NOT NULL CHECK (result_revision=expected_revision+1),
  result_seq bigint NOT NULL CHECK (result_seq>=0),
  result_version integer NOT NULL CHECK (result_version=1),
  result_encoding bytea NOT NULL CHECK (octet_length(result_encoding) BETWEEN 1 AND 2097152),
  fact_first bigint,
  fact_last bigint,
  committed_at timestamptz NOT NULL,
  CHECK ((op_namespace='human' AND binding_id='' AND generation=0) OR (op_namespace='binding' AND binding_id<>'' AND generation>0)),
  CHECK ((fact_first IS NULL AND fact_last IS NULL) OR (fact_first IS NOT NULL AND fact_last IS NOT NULL AND fact_first>0 AND fact_last>=fact_first AND result_seq=fact_last)),
  FOREIGN KEY(room_id,fact_first) REFERENCES room_events(room_id,seq),
  FOREIGN KEY(room_id,fact_last) REFERENCES room_events(room_id,seq),
  PRIMARY KEY(op_namespace,deployment_id,room_id,binding_id,generation,op_kind,identifier,principal)
);
CREATE TRIGGER relay_operations_immutable BEFORE UPDATE OR DELETE ON relay_operations FOR EACH ROW EXECUTE FUNCTION control_reject_change();

CREATE TABLE device_enrollments (
  transport_mode text NOT NULL CHECK(transport_mode IN ('loopback','headless')),
  confirmation_hash bytea,
  confirmation_failures integer NOT NULL DEFAULT 0 CHECK(confirmation_failures BETWEEN 0 AND 5),
  next_poll_at timestamptz,
  poll_sequence bigint NOT NULL DEFAULT 0 CHECK(poll_sequence BETWEEN 0 AND 120),
  poll_violations integer NOT NULL DEFAULT 0 CHECK(poll_violations BETWEEN 0 AND 5),
  last_poll_intent_hash bytea,
  last_poll_status text,
  last_poll_next_at timestamptz,
  request_id text PRIMARY KEY CHECK(request_id<>''),
  device_id text NOT NULL CHECK(device_id<>''),
  label text NOT NULL CHECK(length(label) BETWEEN 1 AND 128),
  origin text NOT NULL CHECK(origin<>''),
  callback_uri text NOT NULL,
  client_state text NOT NULL,
  proof_hash bytea NOT NULL CHECK(octet_length(proof_hash)=32),
  grant_id text NOT NULL UNIQUE CHECK(grant_id<>''),
  access_hash bytea NOT NULL UNIQUE CHECK(octet_length(access_hash)=32),
  refresh_hash bytea NOT NULL UNIQUE CHECK(octet_length(refresh_hash)=32),
  status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','denied','completed','invalidated')),
  approved_user_id text REFERENCES users(user_id),
  code_hash bytea CHECK(code_hash IS NULL OR octet_length(code_hash)=32),
  approval_count integer NOT NULL DEFAULT 0 CHECK(approval_count BETWEEN 0 AND 3),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  CHECK(expires_at>created_at AND expires_at<=created_at+interval '10 minutes'),
  CHECK(status NOT IN ('approved','completed') OR (approved_user_id IS NOT NULL AND code_hash IS NOT NULL AND approval_count>0)),
  CONSTRAINT device_enrollments_transport_shape CHECK((transport_mode='loopback' AND callback_uri<>'' AND client_state<>'' AND confirmation_hash IS NULL AND confirmation_failures=0 AND next_poll_at IS NULL AND poll_sequence=0 AND poll_violations=0 AND last_poll_intent_hash IS NULL AND last_poll_status IS NULL AND last_poll_next_at IS NULL) OR (transport_mode='headless' AND callback_uri='' AND client_state='' AND octet_length(confirmation_hash)=32 AND confirmation_hash IS NOT NULL AND next_poll_at IS NOT NULL AND next_poll_at<=expires_at AND approval_count<=1 AND (status NOT IN ('approved','completed') OR code_hash=confirmation_hash))),
  CONSTRAINT device_enrollments_poll_shape CHECK((poll_sequence=0 AND last_poll_intent_hash IS NULL AND last_poll_status IS NULL AND last_poll_next_at IS NULL) OR (poll_sequence>0 AND octet_length(last_poll_intent_hash)=32 AND last_poll_intent_hash IS NOT NULL AND last_poll_status IN ('pending','approved','denied','completed','invalidated','expired','slow_down','polling_exhausted') AND last_poll_status IS NOT NULL AND last_poll_next_at IS NOT NULL AND last_poll_next_at<=expires_at))
);
CREATE TRIGGER device_enrollments_no_delete BEFORE DELETE ON device_enrollments FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER device_enrollments_identity_immutable BEFORE UPDATE ON device_enrollments FOR EACH ROW EXECUTE FUNCTION control_guard_columns('device_enrollments_identity_immutable','transport_mode','confirmation_hash','request_id','device_id','label','origin','callback_uri','client_state','proof_hash','grant_id','access_hash','refresh_hash','created_at','expires_at');

CREATE FUNCTION device_enrollments_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.confirmation_failures<OLD.confirmation_failures OR NEW.poll_violations<OLD.poll_violations OR NEW.poll_sequence<OLD.poll_sequence OR NEW.poll_sequence>OLD.poll_sequence+1 OR NEW.next_poll_at<OLD.next_poll_at THEN RAISE EXCEPTION 'enrollment counters are monotonic' USING ERRCODE='23514'; END IF;
 IF OLD.status IN ('denied','invalidated','completed') AND NEW.status<>OLD.status THEN RAISE EXCEPTION 'enrollment terminal state is permanent' USING ERRCODE='23514'; END IF;
 IF NEW.confirmation_failures=5 AND NEW.status<>'invalidated' OR NEW.poll_violations=5 AND NEW.status<>'invalidated' THEN RAISE EXCEPTION 'enrollment exhausted' USING ERRCODE='23514'; END IF;
 IF OLD.transport_mode='headless' AND OLD.status='approved' AND (NEW.approved_user_id IS DISTINCT FROM OLD.approved_user_id OR NEW.code_hash IS DISTINCT FROM OLD.code_hash OR NEW.approval_count<>OLD.approval_count) THEN RAISE EXCEPTION 'headless consent immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER device_enrollments_state_guard BEFORE UPDATE ON device_enrollments FOR EACH ROW EXECUTE FUNCTION device_enrollments_guard();

CREATE TABLE device_families (
  family_id text PRIMARY KEY REFERENCES device_enrollments(request_id),
  device_id text NOT NULL REFERENCES devices(device_id),
  user_id text NOT NULL REFERENCES users(user_id),
  origin text NOT NULL,
  proof_hash bytea NOT NULL CHECK(octet_length(proof_hash)=32),
  restore_epoch text NOT NULL,
  current_grant_id text NOT NULL,
  previous_grant_id text,
  recovery_rotation_id text,
  recovery_hash bytea CHECK(recovery_hash IS NULL OR octet_length(recovery_hash)=32),
  recovery_until timestamptz,
  recovery_used_at timestamptz,
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK(expires_at>created_at),
  revoked_at timestamptz,
  CHECK((previous_grant_id IS NULL AND recovery_rotation_id IS NULL AND recovery_hash IS NULL AND recovery_until IS NULL AND recovery_used_at IS NULL) OR (previous_grant_id IS NOT NULL AND recovery_rotation_id IS NOT NULL AND recovery_hash IS NOT NULL AND recovery_until IS NOT NULL))
);
CREATE INDEX device_families_device_key ON device_families(device_id);
CREATE TRIGGER device_families_no_delete BEFORE DELETE ON device_families FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER device_families_identity_immutable BEFORE UPDATE ON device_families FOR EACH ROW EXECUTE FUNCTION control_guard_columns('device_families_identity_immutable','family_id','device_id','user_id','origin','proof_hash','restore_epoch','created_at','expires_at');
CREATE TRIGGER device_families_revoked_once BEFORE UPDATE ON device_families FOR EACH ROW EXECUTE FUNCTION browser_session_revoke_once();

CREATE TABLE device_grants (
  grant_id text PRIMARY KEY CHECK(grant_id<>''),
  family_id text NOT NULL REFERENCES device_families(family_id),
  access_hash bytea NOT NULL UNIQUE CHECK(octet_length(access_hash)=32),
  refresh_hash bytea NOT NULL UNIQUE CHECK(octet_length(refresh_hash)=32),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK(expires_at>created_at),
  revoked_at timestamptz
);
ALTER TABLE device_families ADD CONSTRAINT device_families_current_fkey FOREIGN KEY(current_grant_id) REFERENCES device_grants(grant_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE device_families ADD CONSTRAINT device_families_previous_fkey FOREIGN KEY(previous_grant_id) REFERENCES device_grants(grant_id) DEFERRABLE INITIALLY DEFERRED;
CREATE TRIGGER device_grants_no_delete BEFORE DELETE ON device_grants FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER device_grants_identity_immutable BEFORE UPDATE ON device_grants FOR EACH ROW EXECUTE FUNCTION control_guard_columns('device_grants_identity_immutable','grant_id','family_id','access_hash','refresh_hash','created_at','expires_at');
CREATE TRIGGER device_grants_revoked_once BEFORE UPDATE ON device_grants FOR EACH ROW EXECUTE FUNCTION browser_session_revoke_once();

CREATE TABLE binding_grants (
  grant_id text PRIMARY KEY CHECK(grant_id<>''),
  binding_id text NOT NULL REFERENCES bindings(binding_id),
  family_id text NOT NULL REFERENCES device_families(family_id),
  token_hash bytea NOT NULL UNIQUE CHECK(octet_length(token_hash)=32),
  restore_epoch text NOT NULL,
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK(expires_at>created_at),
  revoked_at timestamptz
);
CREATE INDEX binding_grants_binding_key ON binding_grants(binding_id);
CREATE TRIGGER binding_grants_no_delete BEFORE DELETE ON binding_grants FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER binding_grants_identity_immutable BEFORE UPDATE ON binding_grants FOR EACH ROW EXECUTE FUNCTION control_guard_columns('binding_grants_identity_immutable','grant_id','binding_id','family_id','token_hash','restore_epoch','created_at','expires_at');
CREATE TRIGGER binding_grants_revoked_once BEFORE UPDATE ON binding_grants FOR EACH ROW EXECUTE FUNCTION browser_session_revoke_once();

CREATE TABLE governance_actions (
  action_id text PRIMARY KEY CHECK(action_id<>''),
  actor_user_id text NOT NULL REFERENCES users(user_id),
  session_id text NOT NULL REFERENCES browser_sessions(session_id),
  room_id text NOT NULL REFERENCES rooms(room_id),
  target_user_id text NOT NULL REFERENCES users(user_id),
  expected_owner_user_id text NOT NULL REFERENCES users(user_id),
  expected_revision bigint NOT NULL CHECK(expected_revision>=0),
  reason text NOT NULL CONSTRAINT governance_reason_valid CHECK(octet_length(reason) BETWEEN 1 AND 1024 AND btrim(reason, U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000')<>'' AND reason !~ U&'[\0001-\0008\000B\000C\000E-\001F\007F-\009F]'),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  ready_at timestamptz,
  consumed_at timestamptz,
  invalidated_at timestamptz,
  CHECK(expires_at>created_at AND expires_at<=created_at+interval '10 minutes'),
  CHECK(consumed_at IS NULL OR ready_at IS NOT NULL)
);
CREATE FUNCTION control_columns_once() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog AS $fn$
DECLARE col text;
BEGIN
  FOREACH col IN ARRAY TG_ARGV LOOP
    IF (to_jsonb(OLD)->>col) IS NOT NULL AND (to_jsonb(NEW)->>col) IS DISTINCT FROM (to_jsonb(OLD)->>col) THEN
      RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT=TG_TABLE_NAME || '_' || col || '_once', MESSAGE='write-once control state cannot be restored';
    END IF;
  END LOOP;
  RETURN NEW;
END
$fn$;
CREATE TRIGGER governance_actions_no_delete BEFORE DELETE ON governance_actions FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER governance_actions_identity_immutable BEFORE UPDATE ON governance_actions FOR EACH ROW EXECUTE FUNCTION control_guard_columns('governance_actions_identity_immutable','action_id','actor_user_id','session_id','room_id','target_user_id','expected_owner_user_id','expected_revision','reason','created_at','expires_at');
CREATE TRIGGER governance_actions_once BEFORE UPDATE ON governance_actions FOR EACH ROW EXECUTE FUNCTION control_columns_once('ready_at','consumed_at','invalidated_at');
CREATE TABLE governance_operations (
  deployment_id text NOT NULL REFERENCES deployment(deployment_id),
  actor_user_id text NOT NULL REFERENCES users(user_id),
  op_kind text NOT NULL CHECK(op_kind IN ('prepare','finish','transfer')),
  identifier text NOT NULL CHECK(identifier<>''),
  intent_hash bytea NOT NULL CHECK(octet_length(intent_hash)=32),
  result_hash bytea NOT NULL CHECK(octet_length(result_hash)=32),
  result_version integer NOT NULL CHECK(result_version=1),
  result_encoding bytea NOT NULL CHECK(octet_length(result_encoding) BETWEEN 1 AND 262144),
  committed_at timestamptz NOT NULL,
  PRIMARY KEY(deployment_id,actor_user_id,op_kind,identifier)
);
CREATE TRIGGER governance_operations_immutable BEFORE UPDATE OR DELETE ON governance_operations FOR EACH ROW EXECUTE FUNCTION control_reject_change();
CREATE TRIGGER agents_runtime_once BEFORE UPDATE ON agents FOR EACH ROW EXECUTE FUNCTION control_columns_once('runtime_kind','runtime_recorded_at');

-- A Device request and a live Room Owner decision are independent authorities.
-- The request freezes its native proof and candidate Binding grant; the final
-- typed Room transaction consumes it together with the Binding fact.
CREATE TABLE binding_requests (
  request_id text PRIMARY KEY CHECK(request_id<>''),
  org_id text NOT NULL REFERENCES configured_org(org_id),
  room_id text NOT NULL,
  slot text NOT NULL CHECK(slot IN ('slot1','slot2')),
  observed_generation bigint NOT NULL CHECK(observed_generation>0),
  replacement boolean NOT NULL,
  agent_id text NOT NULL,
  binding_id text NOT NULL UNIQUE CHECK(binding_id<>''),
  grant_id text NOT NULL UNIQUE CHECK(grant_id<>''),
  token_hash bytea NOT NULL UNIQUE CHECK(octet_length(token_hash)=32),
  proof_hash bytea NOT NULL CHECK(octet_length(proof_hash)=32),
  park_enabled boolean NOT NULL,
  user_id text NOT NULL,
  device_id text NOT NULL,
  family_id text NOT NULL REFERENCES device_families(family_id),
  restore_epoch text NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','approved','denied','consumed','invalidated')),
  requested_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  approved_by text REFERENCES users(user_id),
  decided_at timestamptz,
  consumed_at timestamptz,
  FOREIGN KEY(org_id,room_id) REFERENCES rooms(org_id,room_id),
  FOREIGN KEY(org_id,user_id) REFERENCES users(org_id,user_id),
  FOREIGN KEY(org_id,device_id) REFERENCES devices(org_id,device_id),
  FOREIGN KEY(org_id,agent_id) REFERENCES agents(org_id,agent_id),
  FOREIGN KEY(room_id,slot,observed_generation) REFERENCES slot_generations(room_id,slot,generation),
  CHECK(expires_at>requested_at AND expires_at<=requested_at+interval '10 minutes'),
  CHECK(status NOT IN ('approved','denied','consumed') OR (approved_by IS NOT NULL AND decided_at IS NOT NULL)),
  CHECK(status<>'consumed' OR consumed_at IS NOT NULL),
  CHECK(consumed_at IS NULL OR (status='consumed' AND decided_at IS NOT NULL))
);
CREATE TRIGGER binding_requests_no_delete BEFORE DELETE ON binding_requests FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER binding_requests_identity_immutable BEFORE UPDATE ON binding_requests FOR EACH ROW EXECUTE FUNCTION control_guard_columns('binding_requests_identity_immutable','request_id','org_id','room_id','slot','observed_generation','replacement','agent_id','binding_id','grant_id','token_hash','proof_hash','park_enabled','user_id','device_id','family_id','restore_epoch','requested_at','expires_at');
CREATE TRIGGER binding_requests_decision_once BEFORE UPDATE ON binding_requests FOR EACH ROW EXECUTE FUNCTION control_columns_once('approved_by','decided_at','consumed_at');
CREATE TABLE binding_operations (
  deployment_id text NOT NULL REFERENCES deployment(deployment_id),
  room_id text NOT NULL REFERENCES rooms(room_id),
  principal text NOT NULL REFERENCES users(user_id),
  op_kind text NOT NULL CHECK(op_kind IN ('request','decide','complete','renew')),
  identifier text NOT NULL CHECK(identifier<>''),
  intent_hash bytea NOT NULL CHECK(octet_length(intent_hash)=32),
  result_version integer NOT NULL CHECK(result_version=1),
  result_encoding bytea NOT NULL CHECK(octet_length(result_encoding) BETWEEN 1 AND 262144),
  result_hash bytea NOT NULL CHECK(octet_length(result_hash)=32),
  result_seq bigint NOT NULL CHECK(result_seq>=0),
  result_revision bigint NOT NULL CHECK(result_revision>=0),
  accepted_at timestamptz NOT NULL,
  PRIMARY KEY(deployment_id,room_id,principal,op_kind,identifier)
);
CREATE TRIGGER binding_operations_immutable BEFORE UPDATE OR DELETE ON binding_operations FOR EACH ROW EXECUTE FUNCTION control_reject_change();


-- Startup recovery is keyed by ownership epoch, never by a guessed timestamp.
-- A completed run cannot later reap claims created in the same incarnation.
CREATE TABLE startup_room_recoveries (
  ownership_epoch bigint NOT NULL CHECK(ownership_epoch>0),
  room_id text NOT NULL REFERENCES rooms(room_id),
  result_seq bigint NOT NULL CHECK(result_seq>=0),
  result_revision bigint NOT NULL CHECK(result_revision>=0),
  reaped_count bigint NOT NULL CHECK(reaped_count>=0),
  recovered_at timestamptz NOT NULL,
  PRIMARY KEY(ownership_epoch,room_id)
);
CREATE TABLE startup_recovery_runs (
  ownership_epoch bigint PRIMARY KEY CHECK(ownership_epoch>0),
  room_count bigint NOT NULL CHECK(room_count>=0),
  reaped_count bigint NOT NULL CHECK(reaped_count>=0),
  completed_at timestamptz NOT NULL
);
CREATE TRIGGER startup_room_recoveries_immutable BEFORE UPDATE OR DELETE ON startup_room_recoveries FOR EACH ROW EXECUTE FUNCTION control_reject_change();
CREATE TRIGGER startup_recovery_runs_immutable BEFORE UPDATE OR DELETE ON startup_recovery_runs FOR EACH ROW EXECUTE FUNCTION control_reject_change();

-- Merge into schema.sql after control_guard_columns/control_reject_change exist.
-- Defaults preserve pre-catalog primitive fixtures. A missing Project Owner
-- never grants discovery or write access; production creates both explicitly.
ALTER TABLE projects ADD COLUMN display_name text NOT NULL DEFAULT '' CHECK(octet_length(display_name)<=160);
ALTER TABLE projects ADD COLUMN owner_user_id text;
ALTER TABLE projects ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK(revision>0);
ALTER TABLE projects ADD CONSTRAINT projects_owner_fkey FOREIGN KEY(org_id,owner_user_id) REFERENCES users(org_id,user_id);
ALTER TABLE rooms ADD COLUMN display_name text NOT NULL DEFAULT '' CHECK(octet_length(display_name)<=160);

CREATE TABLE project_members (
  project_id text NOT NULL,
  org_id text NOT NULL REFERENCES configured_org(org_id),
  user_id text NOT NULL,
  role text NOT NULL CHECK(role IN ('owner','member')),
  disabled boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(project_id,user_id),
  FOREIGN KEY(org_id,project_id) REFERENCES projects(org_id,project_id),
  FOREIGN KEY(org_id,user_id) REFERENCES users(org_id,user_id)
);
CREATE UNIQUE INDEX project_members_owner_key ON project_members(project_id) WHERE role='owner' AND NOT disabled;
CREATE INDEX project_members_user_key ON project_members(user_id,project_id) WHERE NOT disabled;
CREATE TRIGGER project_members_no_delete BEFORE DELETE ON project_members FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER project_members_identity_immutable BEFORE UPDATE ON project_members FOR EACH ROW EXECUTE FUNCTION control_guard_columns('project_members_identity_immutable','project_id','org_id','user_id','created_at');
CREATE TRIGGER projects_no_delete BEFORE DELETE ON projects FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER projects_identity_immutable BEFORE UPDATE ON projects FOR EACH ROW EXECUTE FUNCTION control_guard_columns('projects_identity_immutable','project_id','org_id','owner_user_id','created_at');

CREATE FUNCTION project_owner_record_consistency_check() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $fn$
DECLARE
  selected_project text := COALESCE(NEW.project_id, OLD.project_id);
  selected_owner text;
  matching bigint;
  stray bigint;
BEGIN
  EXECUTE format('SELECT owner_user_id FROM %I.projects WHERE project_id=$1 FOR UPDATE', TG_TABLE_SCHEMA)
    INTO selected_owner USING selected_project;
  EXECUTE format('SELECT count(*) FILTER (WHERE user_id=$2 AND NOT disabled),count(*) FILTER (WHERE user_id IS DISTINCT FROM $2) FROM %I.project_members WHERE project_id=$1 AND role=''owner''', TG_TABLE_SCHEMA)
    INTO matching,stray USING selected_project,selected_owner;
  IF stray<>0 OR (selected_owner IS NOT NULL AND matching<>1) THEN
    RAISE EXCEPTION USING ERRCODE='23514', CONSTRAINT='project_owner_record_consistency_check', MESSAGE='Project Owner and owner membership disagree';
  END IF;
  RETURN NULL;
END
$fn$;
CREATE CONSTRAINT TRIGGER project_members_owner_record_check AFTER INSERT OR UPDATE OR DELETE ON project_members DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION project_owner_record_consistency_check();
CREATE CONSTRAINT TRIGGER projects_owner_record_check AFTER INSERT OR UPDATE ON projects DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION project_owner_record_consistency_check();

CREATE TABLE catalog_operations (
  deployment_id text NOT NULL REFERENCES deployment(deployment_id),
  principal text NOT NULL REFERENCES users(user_id),
  op_kind text NOT NULL CHECK(op_kind IN ('project.create','project.rename','project.archive','project.restore','project.member.add','project.member.remove','room.rename','room.archive','room.restore','room.project','session.revoke','agent.retire')),
  identifier text NOT NULL CHECK(identifier<>''),
  intent_hash bytea NOT NULL CHECK(octet_length(intent_hash)=32),
  result_version integer NOT NULL CHECK(result_version=1),
  result_encoding bytea NOT NULL CHECK(octet_length(result_encoding)>0 AND octet_length(result_encoding)<=16384),
  result_hash bytea NOT NULL CHECK(octet_length(result_hash)=32),
  accepted_at timestamptz NOT NULL,
  PRIMARY KEY(deployment_id,principal,op_kind,identifier)
);
CREATE TRIGGER catalog_operations_immutable BEFORE UPDATE OR DELETE ON catalog_operations FOR EACH ROW EXECUTE FUNCTION control_reject_change();

-- Enterprise objects are opaque Room-scoped identities, never digest paths.
CREATE TABLE attachments (
  object_id text PRIMARY KEY CHECK(object_id ~ '^att-[a-f0-9]{24}$'),
  room_id text NOT NULL REFERENCES rooms(room_id),
  owner_user_id text NOT NULL REFERENCES users(user_id),
  binding_id text NOT NULL DEFAULT '',
  restore_epoch text NOT NULL,
  state text NOT NULL CHECK(state IN ('reserved','verified','referenced','abandoned','deletion_pending')),
  name text NOT NULL CHECK(octet_length(name) BETWEEN 1 AND 256),
  kind text NOT NULL CHECK(kind IN ('file','image')),
  size bigint NOT NULL CHECK(size BETWEEN 1 AND 5242880),
  sha256 text NOT NULL CHECK(sha256 ~ '^[a-f0-9]{64}$'),
  media_type text NOT NULL DEFAULT '',
  width integer NOT NULL DEFAULT 0 CHECK(width>=0),
  height integer NOT NULL DEFAULT 0 CHECK(height>=0),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL CHECK(expires_at>created_at),
  verified_at timestamptz,
  abandoned_at timestamptz,
  marked_at timestamptz,
  deleted_at timestamptz,
  UNIQUE(room_id,object_id),
  CHECK(state NOT IN ('verified','referenced') OR (verified_at IS NOT NULL AND media_type<>'')),
  CHECK(state NOT IN ('abandoned','deletion_pending') OR abandoned_at IS NOT NULL),
  CHECK(state<>'deletion_pending' OR marked_at IS NOT NULL),
  CHECK(deleted_at IS NULL OR state='deletion_pending')
);
CREATE INDEX attachments_room ON attachments(room_id,object_id);
CREATE INDEX attachments_reclaim ON attachments(state,expires_at,abandoned_at) WHERE deleted_at IS NULL;
CREATE TRIGGER attachments_no_delete BEFORE DELETE ON attachments FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER attachments_identity_immutable BEFORE UPDATE ON attachments FOR EACH ROW EXECUTE FUNCTION control_guard_columns('attachments_identity_immutable','object_id','room_id','owner_user_id','binding_id','restore_epoch','name','kind','size','sha256','created_at','expires_at');
CREATE TRIGGER attachments_times_once BEFORE UPDATE ON attachments FOR EACH ROW EXECUTE FUNCTION control_columns_once('verified_at','abandoned_at','marked_at','deleted_at');
CREATE TABLE attachment_usage (
  scope text NOT NULL CHECK(scope IN ('deployment','room')),
  scope_id text NOT NULL,
  bytes bigint NOT NULL DEFAULT 0 CHECK(bytes>=0),
  pending bigint NOT NULL DEFAULT 0 CHECK(pending>=0),
  PRIMARY KEY(scope,scope_id)
);
CREATE TABLE attachment_references (
  room_id text NOT NULL,
  object_id text NOT NULL,
  message_id text NOT NULL CHECK(message_id<>''),
  first_seq bigint NOT NULL,
  PRIMARY KEY(room_id,object_id,message_id),
  FOREIGN KEY(room_id,object_id) REFERENCES attachments(room_id,object_id),
  FOREIGN KEY(room_id,first_seq) REFERENCES room_events(room_id,seq)
);
CREATE TRIGGER attachment_references_immutable BEFORE UPDATE OR DELETE ON attachment_references FOR EACH ROW EXECUTE FUNCTION control_reject_change();
CREATE TABLE attachment_operations (
  deployment_id text NOT NULL REFERENCES deployment(deployment_id),
  room_id text NOT NULL DEFAULT '',
  principal text NOT NULL,
  op_kind text NOT NULL CHECK(op_kind IN ('reserve','verify','cancel','expire','mark','deleted','backup.begin','backup.end')),
  identifier text NOT NULL,
  intent_hash bytea NOT NULL CHECK(octet_length(intent_hash)=32),
  result_version integer NOT NULL CHECK(result_version=1),
  result_encoding bytea NOT NULL CHECK(octet_length(result_encoding) BETWEEN 1 AND 16384),
  result_hash bytea NOT NULL CHECK(octet_length(result_hash)=32),
  accepted_at timestamptz NOT NULL,
  PRIMARY KEY(deployment_id,room_id,principal,op_kind,identifier)
);
CREATE TRIGGER attachment_operations_immutable BEFORE UPDATE OR DELETE ON attachment_operations FOR EACH ROW EXECUTE FUNCTION control_reject_change();
CREATE TABLE attachment_backup_barriers (
  barrier_id text PRIMARY KEY CHECK(barrier_id<>''),
  created_at timestamptz NOT NULL,
  ended_at timestamptz
);
CREATE TRIGGER attachment_backup_no_delete BEFORE DELETE ON attachment_backup_barriers FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER attachment_backup_identity BEFORE UPDATE ON attachment_backup_barriers FOR EACH ROW EXECUTE FUNCTION control_guard_columns('attachment_backup_identity','barrier_id','created_at');
CREATE TRIGGER attachment_backup_once BEFORE UPDATE ON attachment_backup_barriers FOR EACH ROW EXECUTE FUNCTION control_columns_once('ended_at');

-- Runtime restore barriers are deliberately excluded from backup data. No
-- listener may open while a reserved/imported restore remains incomplete.
CREATE TABLE restore_runs (
  restore_id text PRIMARY KEY CHECK(restore_id<>''),
  manifest_hash bytea NOT NULL CHECK(octet_length(manifest_hash)=32),
  restore_epoch text NOT NULL CHECK(restore_epoch<>''),
  started_at timestamptz NOT NULL,
  imported_at timestamptz,
  completed_at timestamptz,
  CHECK(completed_at IS NULL OR imported_at IS NOT NULL)
);
CREATE UNIQUE INDEX restore_one_pending ON restore_runs((true)) WHERE completed_at IS NULL;
CREATE TRIGGER restore_runs_no_delete BEFORE DELETE ON restore_runs FOR EACH ROW EXECUTE FUNCTION control_reject_delete();
CREATE TRIGGER restore_runs_identity BEFORE UPDATE ON restore_runs FOR EACH ROW EXECUTE FUNCTION control_guard_columns('restore_runs_identity','restore_id','manifest_hash','restore_epoch','started_at');
CREATE TRIGGER restore_runs_once BEFORE UPDATE ON restore_runs FOR EACH ROW EXECUTE FUNCTION control_columns_once('imported_at','completed_at');

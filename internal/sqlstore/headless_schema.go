package sqlstore

import (
	"context"
	"fmt"
	"strings"
)

func verifyHeadlessSchema(ctx context.Context, q rowQuerier, schema string) error {
	expected := map[string]string{
		"transport_mode": "text|NO|", "confirmation_hash": "bytea|YES|", "confirmation_failures": "integer|NO|0", "next_poll_at": "timestamp with time zone|YES|", "poll_sequence": "bigint|NO|0", "poll_violations": "integer|NO|0", "last_poll_intent_hash": "bytea|YES|", "last_poll_status": "text|YES|", "last_poll_next_at": "timestamp with time zone|YES|",
	}
	var count int
	if e := q.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='device_enrollments'`, schema).Scan(&count); e != nil || count != 25 {
		return fmt.Errorf("sqlstore: unsupported headless enrollment column count")
	}
	for name, want := range expected {
		var got string
		if e := q.QueryRow(ctx, `SELECT data_type||'|'||is_nullable||'|'||COALESCE(column_default,'') FROM information_schema.columns WHERE table_schema=$1 AND table_name='device_enrollments' AND column_name=$2`, schema, name).Scan(&got); e != nil || got != want {
			return fmt.Errorf("sqlstore: unsupported headless enrollment columns")
		}
	}
	checks := map[string]string{
		"device_enrollments_transport_mode_check":        `CHECK ((transport_mode = ANY (ARRAY['loopback'::text, 'headless'::text])))`,
		"device_enrollments_confirmation_failures_check": `CHECK (((confirmation_failures >= 0) AND (confirmation_failures <= 5)))`,
		"device_enrollments_poll_sequence_check":         `CHECK (((poll_sequence >= 0) AND (poll_sequence <= 120)))`,
		"device_enrollments_poll_violations_check":       `CHECK (((poll_violations >= 0) AND (poll_violations <= 5)))`,
		"device_enrollments_poll_shape":                  `CHECK ((((poll_sequence = 0) AND (last_poll_intent_hash IS NULL) AND (last_poll_status IS NULL) AND (last_poll_next_at IS NULL)) OR ((poll_sequence > 0) AND (octet_length(last_poll_intent_hash) = 32) AND (last_poll_intent_hash IS NOT NULL) AND (last_poll_status = ANY (ARRAY['pending'::text, 'approved'::text, 'denied'::text, 'completed'::text, 'invalidated'::text, 'expired'::text, 'slow_down'::text, 'polling_exhausted'::text])) AND (last_poll_status IS NOT NULL) AND (last_poll_next_at IS NOT NULL) AND (last_poll_next_at <= expires_at))))`,
		"device_enrollments_transport_shape":             `CHECK ((((transport_mode = 'loopback'::text) AND (callback_uri <> ''::text) AND (client_state <> ''::text) AND (confirmation_hash IS NULL) AND (confirmation_failures = 0) AND (next_poll_at IS NULL) AND (poll_sequence = 0) AND (poll_violations = 0) AND (last_poll_intent_hash IS NULL) AND (last_poll_status IS NULL) AND (last_poll_next_at IS NULL)) OR ((transport_mode = 'headless'::text) AND (callback_uri = ''::text) AND (client_state = ''::text) AND (octet_length(confirmation_hash) = 32) AND (confirmation_hash IS NOT NULL) AND (next_poll_at IS NOT NULL) AND (next_poll_at <= expires_at) AND (approval_count <= 1) AND ((status <> ALL (ARRAY['approved'::text, 'completed'::text])) OR (code_hash = confirmation_hash)))))`,
	}
	for name, want := range checks {
		var got string
		var valid bool
		if e := q.QueryRow(ctx, `SELECT pg_catalog.pg_get_constraintdef(c.oid),c.convalidated FROM pg_catalog.pg_constraint c WHERE c.conrelid=$1::regclass AND c.conname=$2`, qualified(schema, "device_enrollments"), name).Scan(&got, &valid); e != nil || !valid || got != want {
			return fmt.Errorf("sqlstore: unsupported headless enrollment constraints")
		}
	}
	for _, name := range []string{"device_enrollments_state_guard", "device_enrollments_identity_immutable", "device_enrollments_no_delete"} {
		var def, body string
		var enabled, namespace, function, arguments string
		var triggerType int16
		if e := q.QueryRow(ctx, `SELECT pg_catalog.pg_get_triggerdef(t.oid),p.prosrc,t.tgenabled::text,n.nspname,p.proname,pg_catalog.encode(t.tgargs,'hex'),t.tgtype FROM pg_catalog.pg_trigger t JOIN pg_catalog.pg_proc p ON p.oid=t.tgfoid JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE t.tgrelid=$1::regclass AND t.tgname=$2 AND NOT t.tgisinternal`, qualified(schema, "device_enrollments"), name).Scan(&def, &body, &enabled, &namespace, &function, &arguments, &triggerType); e != nil || enabled != "O" || namespace != schema {
			return fmt.Errorf("sqlstore: unsupported headless enrollment triggers")
		}
		if name == "device_enrollments_state_guard" {
			if function != "device_enrollments_guard" || triggerType != 19 || arguments != "" {
				return fmt.Errorf("sqlstore: unsupported headless enrollment state trigger")
			}
			start := strings.Index(schemaSQL, "CREATE FUNCTION device_enrollments_guard()")
			fragment := schemaSQL[start:]
			a := strings.Index(fragment, "$$") + 2
			b := strings.Index(fragment[a:], "$$") + a
			if body != fragment[a:b] || !strings.Contains(def, "BEFORE UPDATE ON") || !strings.HasSuffix(def, "FOR EACH ROW EXECUTE FUNCTION device_enrollments_guard()") && !strings.HasSuffix(def, "FOR EACH ROW EXECUTE FUNCTION "+schema+".device_enrollments_guard()") {
				return fmt.Errorf("sqlstore: unsupported headless enrollment state guard")
			}
		} else if name == "device_enrollments_identity_immutable" && (function != "control_guard_columns" || triggerType != 19 || arguments != fmt.Sprintf("%x", strings.Join([]string{"device_enrollments_identity_immutable", "transport_mode", "confirmation_hash", "request_id", "device_id", "label", "origin", "callback_uri", "client_state", "proof_hash", "grant_id", "access_hash", "refresh_hash", "created_at", "expires_at", ""}, "\x00"))) {
			return fmt.Errorf("sqlstore: unsupported headless enrollment identity guard")
		} else if name == "device_enrollments_no_delete" && (function != "control_reject_delete" || triggerType != 11 || arguments != "") {
			return fmt.Errorf("sqlstore: unsupported headless enrollment deletion guard")
		}
	}
	return nil
}

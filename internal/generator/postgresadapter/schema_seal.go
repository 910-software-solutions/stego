package postgresadapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

// The seal step of the runtime data path (ST-SUP-RC01-OC49, ST-SUP-RC01-OC50; decision ST-DECL-Q44). A seal moves a
// readable secret field that the base stores in clear to its sealed column <column>_sealed, across two releases, as
// the online encryption of a column in Rails (support_unencrypted_data and a backfill) and the encrypted attributes
// of GitLab, whose backfill runs after the deploy, do:
//
//  1. At expand the migration adds the nullable column <column>_sealed of type bytea, which the schema check of the
//     release before accepts, makes the table of the release reports and the seal checkpoint of the change, and
//     changes nothing else. The release of the step writes both columns and reads the clear one when the two
//     differ, and its controller seals and checks each row in passes (ST-SUP-RP10-LC49, ST-SUP-RP10-LC50). No trigger
//     can seal, since the database has no key, and the schema check of the release before accepts no new trigger;
//     so a pass completes only when no release before ran during it (ST-SUP-RP09-LC15).
//  2. At contract, in a later release, the migration refuses while a release before runs, while the pass is not
//     complete, did not start in this database instance or a release before ran after its start, and while a row has a clear value and no sealed value; it
//     validates the completeness CHECK, makes the sealed column of a required field NOT NULL, and then drops the
//     clear column in one transaction that checks the step again.

// sealsText is the text of the table of the seal checkpoints and its three functions of the runtime library
// (registry/runtime/lease.SealsText, ST-SUP-RP09-LC15), with %[1]s the runtime role, quoted; a copy, as the release
// reports are, which TestSyncTemplateCopy compares with the runtime's text. It is sealsTable, which the body of the
// expand migration of a seal holds, and then sealCheckpoints, which each migration job runs again outside the bodies
// and their digests (ST-SUP-RC01-OC49, ST-SUP-RC01-OC51).
const sealsText = sealsTable + sealCheckpoints

// sealsTable is the table of the seal checkpoints, the first statement of sealsText.
const sealsTable = `CREATE TABLE IF NOT EXISTS stego_schema.seals (
	change_id text PRIMARY KEY,
	expand text NOT NULL,
	last_tenant text,
	last_id uuid,
	sealed_rows bigint NOT NULL DEFAULT 0,
	checked_rows bigint NOT NULL DEFAULT 0,
	pass_started timestamptz,
	completed_at timestamptz,
	token bigint NOT NULL DEFAULT 0,
	created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp()
);
`

// sealCheckpoints is the rest of sealsText: the columns that later versions added, the functions, the grants and
// release_stop. Each statement runs again with no error, so the migration job runs it after each migration that it
// applies and once at its end when the table is there (applySealCheckpoints), as a repeatable migration of Flyway or a
// change set with runOnChange of Liquibase: a new version of the text takes effect on a database whose expand
// migration ran with an older one, and the ledger digest of that migration does not change.
const sealCheckpoints = `ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp();
CREATE OR REPLACE FUNCTION stego_schema.seal_instance()
 RETURNS text LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
 AS $stego_seal$SELECT d.oid::text || '/' || pg_catalog.to_char(pg_catalog.pg_postmaster_start_time() AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS')
  || '/' || 'stego_schema.seals'::pg_catalog.regclass::pg_catalog.oid::text || '/' || pg_catalog.pg_relation_filenode('stego_schema.seals'::pg_catalog.regclass)::text
  FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.seal_instance() FROM PUBLIC;
ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS armed_in text;
ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS armed_at timestamptz;
ALTER TABLE stego_schema.seals ALTER COLUMN armed_in SET DEFAULT stego_schema.seal_instance();
REVOKE ALL ON TABLE stego_schema.seals FROM PUBLIC;
CREATE OR REPLACE FUNCTION stego_schema.seal_read(text)
 RETURNS TABLE (last_tenant text, last_id text, sealed_rows bigint, checked_rows bigint, started boolean, completed boolean, waiting boolean, stale boolean)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$SELECT s.last_tenant, s.last_id::text, s.sealed_rows, s.checked_rows, s.pass_started IS NOT NULL, s.completed_at IS NOT NULL,
  GREATEST(s.created_at, s.armed_at) + interval '3 minutes' > pg_catalog.clock_timestamp() OR EXISTS (SELECT 1 FROM stego_schema.releases r WHERE r.release COLLATE "C" < s.expand COLLATE "C"
   AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > pg_catalog.clock_timestamp()),
  s.pass_started IS NOT NULL AND (s.armed_in IS DISTINCT FROM stego_schema.seal_instance() OR EXISTS (SELECT 1 FROM stego_schema.releases r
   WHERE r.release COLLATE "C" < s.expand COLLATE "C" AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > s.pass_started))
  FROM stego_schema.seals s WHERE s.change_id = $1$stego_seal$;
CREATE OR REPLACE FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint)
 RETURNS integer LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $7 AND $7 = 'stego.backfill.' || $1 AND l.holder = $8 AND l.token = $9 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  IF $6 THEN
    UPDATE stego_schema.seals s SET armed_in = stego_schema.seal_instance(), armed_at = pg_catalog.clock_timestamp(),
     last_tenant = NULL, last_id = NULL, sealed_rows = 0, checked_rows = 0, pass_started = NULL, completed_at = NULL
     WHERE s.change_id = $1 AND s.token <= $9 AND s.armed_in IS DISTINCT FROM stego_schema.seal_instance();
    IF FOUND THEN
      RETURN -1;
    END IF;
    IF EXISTS (SELECT 1 FROM stego_schema.seals s WHERE s.change_id = $1 AND GREATEST(s.created_at, s.armed_at) + interval '3 minutes' > pg_catalog.clock_timestamp())
     OR EXISTS (SELECT 1 FROM stego_schema.seals s JOIN stego_schema.releases r ON r.release COLLATE "C" < s.expand COLLATE "C"
     WHERE s.change_id = $1 AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > pg_catalog.clock_timestamp()) THEN
      RETURN -1;
    END IF;
    WITH changed AS (UPDATE stego_schema.seals s SET last_tenant = NULL, last_id = NULL, sealed_rows = 0, checked_rows = 0,
     pass_started = pg_catalog.clock_timestamp(), completed_at = NULL, token = $9 WHERE s.change_id = $1 AND s.token <= $9 RETURNING 1)
    SELECT pg_catalog.count(*) INTO n FROM changed;
    RETURN n;
  END IF;
  WITH changed AS (UPDATE stego_schema.seals s SET last_tenant = $2, last_id = $3, sealed_rows = $4, checked_rows = $5, token = $9
   WHERE s.change_id = $1 AND s.token <= $9 AND s.pass_started IS NOT NULL AND s.completed_at IS NULL RETURNING 1)
  SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_seal$;
CREATE OR REPLACE FUNCTION stego_schema.seal_complete(text, text, text, bigint)
 RETURNS integer LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $2 AND $2 = 'stego.backfill.' || $1 AND l.holder = $3 AND l.token = $4 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  IF EXISTS (SELECT 1 FROM stego_schema.seals s WHERE s.change_id = $1 AND s.armed_in IS DISTINCT FROM stego_schema.seal_instance())
   OR EXISTS (SELECT 1 FROM stego_schema.seals s JOIN stego_schema.releases r ON r.release COLLATE "C" < s.expand COLLATE "C"
   WHERE s.change_id = $1 AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > s.pass_started) THEN
    UPDATE stego_schema.seals s SET last_tenant = NULL, last_id = NULL, pass_started = NULL, completed_at = NULL, token = $4
     WHERE s.change_id = $1 AND s.token <= $4;
    RETURN -1;
  END IF;
  WITH changed AS (UPDATE stego_schema.seals s SET completed_at = pg_catalog.clock_timestamp(), token = $4
   WHERE s.change_id = $1 AND s.token <= $4 AND s.pass_started IS NOT NULL RETURNING 1)
  SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.seal_read(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.seal_complete(text, text, text, bigint) FROM PUBLIC;
GRANT USAGE ON SCHEMA stego_schema TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_read(text) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_complete(text, text, text, bigint) TO %[1]s;
CREATE OR REPLACE FUNCTION stego_schema.release_stop(text)
 RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$BEGIN
  DELETE FROM stego_schema.releases r WHERE r.holder = $1
   AND NOT EXISTS (SELECT 1 FROM stego_schema.seals s WHERE r.release COLLATE "C" < s.expand COLLATE "C");
END$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.release_stop(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION stego_schema.release_stop(text) TO %[1]s;
`

// The texts of the seal checkpoints that earlier versions of the adapter held in the bodies of the steps
// (sealsHistory): postgres-adapter 6.14.0 to 6.19.0 and 6.20.0. A database where such a version applied a step keeps the
// ledger digest of that body, which the ledger accepts as an earlier digest of the step (ST-SUP-RC01-OC51).
var sealsHistory = []string{sealsTextBefore620, sealsText620}

// sealsTextBefore620 is sealsText of postgres-adapter 6.14.0 to 6.19.0, unchanged.
const sealsTextBefore620 = `CREATE TABLE IF NOT EXISTS stego_schema.seals (
	change_id text PRIMARY KEY,
	expand text NOT NULL,
	last_tenant text,
	last_id uuid,
	sealed_rows bigint NOT NULL DEFAULT 0,
	checked_rows bigint NOT NULL DEFAULT 0,
	pass_started timestamptz,
	completed_at timestamptz,
	token bigint NOT NULL DEFAULT 0,
	created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp()
);
ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp();
REVOKE ALL ON TABLE stego_schema.seals FROM PUBLIC;
CREATE OR REPLACE FUNCTION stego_schema.seal_read(text)
 RETURNS TABLE (last_tenant text, last_id text, sealed_rows bigint, checked_rows bigint, started boolean, completed boolean, waiting boolean, stale boolean)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$SELECT s.last_tenant, s.last_id::text, s.sealed_rows, s.checked_rows, s.pass_started IS NOT NULL, s.completed_at IS NOT NULL,
  s.created_at + interval '3 minutes' > pg_catalog.clock_timestamp() OR EXISTS (SELECT 1 FROM stego_schema.releases r WHERE r.release COLLATE "C" < s.expand COLLATE "C"
   AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > pg_catalog.clock_timestamp()),
  s.pass_started IS NOT NULL AND EXISTS (SELECT 1 FROM stego_schema.releases r WHERE r.release COLLATE "C" < s.expand COLLATE "C"
   AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > s.pass_started)
  FROM stego_schema.seals s WHERE s.change_id = $1$stego_seal$;
CREATE OR REPLACE FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint)
 RETURNS integer LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $7 AND $7 = 'stego.backfill.' || $1 AND l.holder = $8 AND l.token = $9 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  IF $6 THEN
    IF EXISTS (SELECT 1 FROM stego_schema.seals s WHERE s.change_id = $1 AND s.created_at + interval '3 minutes' > pg_catalog.clock_timestamp())
     OR EXISTS (SELECT 1 FROM stego_schema.seals s JOIN stego_schema.releases r ON r.release COLLATE "C" < s.expand COLLATE "C"
     WHERE s.change_id = $1 AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > pg_catalog.clock_timestamp()) THEN
      RETURN -1;
    END IF;
    WITH changed AS (UPDATE stego_schema.seals s SET last_tenant = NULL, last_id = NULL, sealed_rows = 0, checked_rows = 0,
     pass_started = pg_catalog.clock_timestamp(), completed_at = NULL, token = $9 WHERE s.change_id = $1 AND s.token <= $9 RETURNING 1)
    SELECT pg_catalog.count(*) INTO n FROM changed;
    RETURN n;
  END IF;
  WITH changed AS (UPDATE stego_schema.seals s SET last_tenant = $2, last_id = $3, sealed_rows = $4, checked_rows = $5, token = $9
   WHERE s.change_id = $1 AND s.token <= $9 AND s.pass_started IS NOT NULL AND s.completed_at IS NULL RETURNING 1)
  SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_seal$;
CREATE OR REPLACE FUNCTION stego_schema.seal_complete(text, text, text, bigint)
 RETURNS integer LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $2 AND $2 = 'stego.backfill.' || $1 AND l.holder = $3 AND l.token = $4 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  IF EXISTS (SELECT 1 FROM stego_schema.seals s JOIN stego_schema.releases r ON r.release COLLATE "C" < s.expand COLLATE "C"
   WHERE s.change_id = $1 AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > s.pass_started) THEN
    UPDATE stego_schema.seals s SET last_tenant = NULL, last_id = NULL, pass_started = NULL, completed_at = NULL, token = $4
     WHERE s.change_id = $1 AND s.token <= $4;
    RETURN -1;
  END IF;
  WITH changed AS (UPDATE stego_schema.seals s SET completed_at = pg_catalog.clock_timestamp(), token = $4
   WHERE s.change_id = $1 AND s.token <= $4 AND s.pass_started IS NOT NULL RETURNING 1)
  SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.seal_read(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.seal_complete(text, text, text, bigint) FROM PUBLIC;
GRANT USAGE ON SCHEMA stego_schema TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_read(text) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_complete(text, text, text, bigint) TO %[1]s;
CREATE OR REPLACE FUNCTION stego_schema.release_stop(text)
 RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$BEGIN
  DELETE FROM stego_schema.releases r WHERE r.holder = $1
   AND NOT EXISTS (SELECT 1 FROM stego_schema.seals s WHERE r.release COLLATE "C" < s.expand COLLATE "C");
END$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.release_stop(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION stego_schema.release_stop(text) TO %[1]s;
`

// sealsText620 is sealsText of postgres-adapter 6.20.0, unchanged.
const sealsText620 = `CREATE TABLE IF NOT EXISTS stego_schema.seals (
	change_id text PRIMARY KEY,
	expand text NOT NULL,
	last_tenant text,
	last_id uuid,
	sealed_rows bigint NOT NULL DEFAULT 0,
	checked_rows bigint NOT NULL DEFAULT 0,
	pass_started timestamptz,
	completed_at timestamptz,
	token bigint NOT NULL DEFAULT 0,
	created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp()
);
ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp();
CREATE OR REPLACE FUNCTION stego_schema.seal_instance()
 RETURNS text LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
 AS $stego_seal$SELECT d.oid::text || '/' || pg_catalog.to_char(pg_catalog.pg_postmaster_start_time() AT TIME ZONE 'UTC', 'YYYYMMDDHH24MISSUS')
  FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.seal_instance() FROM PUBLIC;
ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS armed_in text;
ALTER TABLE stego_schema.seals ADD COLUMN IF NOT EXISTS armed_at timestamptz;
ALTER TABLE stego_schema.seals ALTER COLUMN armed_in SET DEFAULT stego_schema.seal_instance();
REVOKE ALL ON TABLE stego_schema.seals FROM PUBLIC;
CREATE OR REPLACE FUNCTION stego_schema.seal_read(text)
 RETURNS TABLE (last_tenant text, last_id text, sealed_rows bigint, checked_rows bigint, started boolean, completed boolean, waiting boolean, stale boolean)
 LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$SELECT s.last_tenant, s.last_id::text, s.sealed_rows, s.checked_rows, s.pass_started IS NOT NULL, s.completed_at IS NOT NULL,
  GREATEST(s.created_at, s.armed_at) + interval '3 minutes' > pg_catalog.clock_timestamp() OR EXISTS (SELECT 1 FROM stego_schema.releases r WHERE r.release COLLATE "C" < s.expand COLLATE "C"
   AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > pg_catalog.clock_timestamp()),
  s.pass_started IS NOT NULL AND EXISTS (SELECT 1 FROM stego_schema.releases r WHERE r.release COLLATE "C" < s.expand COLLATE "C"
   AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > s.pass_started)
  FROM stego_schema.seals s WHERE s.change_id = $1$stego_seal$;
CREATE OR REPLACE FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint)
 RETURNS integer LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $7 AND $7 = 'stego.backfill.' || $1 AND l.holder = $8 AND l.token = $9 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  IF $6 THEN
    UPDATE stego_schema.seals s SET armed_in = stego_schema.seal_instance(), armed_at = pg_catalog.clock_timestamp()
     WHERE s.change_id = $1 AND s.token <= $9 AND s.armed_in IS DISTINCT FROM stego_schema.seal_instance();
    IF FOUND THEN
      RETURN -1;
    END IF;
    IF EXISTS (SELECT 1 FROM stego_schema.seals s WHERE s.change_id = $1 AND GREATEST(s.created_at, s.armed_at) + interval '3 minutes' > pg_catalog.clock_timestamp())
     OR EXISTS (SELECT 1 FROM stego_schema.seals s JOIN stego_schema.releases r ON r.release COLLATE "C" < s.expand COLLATE "C"
     WHERE s.change_id = $1 AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > pg_catalog.clock_timestamp()) THEN
      RETURN -1;
    END IF;
    WITH changed AS (UPDATE stego_schema.seals s SET last_tenant = NULL, last_id = NULL, sealed_rows = 0, checked_rows = 0,
     pass_started = pg_catalog.clock_timestamp(), completed_at = NULL, token = $9 WHERE s.change_id = $1 AND s.token <= $9 RETURNING 1)
    SELECT pg_catalog.count(*) INTO n FROM changed;
    RETURN n;
  END IF;
  WITH changed AS (UPDATE stego_schema.seals s SET last_tenant = $2, last_id = $3, sealed_rows = $4, checked_rows = $5, token = $9
   WHERE s.change_id = $1 AND s.token <= $9 AND s.pass_started IS NOT NULL AND s.completed_at IS NULL RETURNING 1)
  SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_seal$;
CREATE OR REPLACE FUNCTION stego_schema.seal_complete(text, text, text, bigint)
 RETURNS integer LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$DECLARE n bigint;
BEGIN
  PERFORM 1 FROM public.leases l WHERE l.name = $2 AND $2 = 'stego.backfill.' || $1 AND l.holder = $3 AND l.token = $4 AND l.expires_at > pg_catalog.clock_timestamp() FOR SHARE;
  IF NOT FOUND THEN
    RETURN 0;
  END IF;
  IF EXISTS (SELECT 1 FROM stego_schema.seals s JOIN stego_schema.releases r ON r.release COLLATE "C" < s.expand COLLATE "C"
   WHERE s.change_id = $1 AND r.seen_at + r.every_ms * 3 * interval '1 millisecond' > s.pass_started) THEN
    UPDATE stego_schema.seals s SET last_tenant = NULL, last_id = NULL, pass_started = NULL, completed_at = NULL, token = $4
     WHERE s.change_id = $1 AND s.token <= $4;
    RETURN -1;
  END IF;
  WITH changed AS (UPDATE stego_schema.seals s SET completed_at = pg_catalog.clock_timestamp(), token = $4
   WHERE s.change_id = $1 AND s.token <= $4 AND s.pass_started IS NOT NULL RETURNING 1)
  SELECT pg_catalog.count(*) INTO n FROM changed;
  RETURN n;
END$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.seal_read(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.seal_complete(text, text, text, bigint) FROM PUBLIC;
GRANT USAGE ON SCHEMA stego_schema TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_read(text) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) TO %[1]s;
GRANT EXECUTE ON FUNCTION stego_schema.seal_complete(text, text, text, bigint) TO %[1]s;
CREATE OR REPLACE FUNCTION stego_schema.release_stop(text)
 RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_seal$BEGIN
  DELETE FROM stego_schema.releases r WHERE r.holder = $1
   AND NOT EXISTS (SELECT 1 FROM stego_schema.seals s WHERE r.release COLLATE "C" < s.expand COLLATE "C");
END$stego_seal$;
REVOKE ALL ON FUNCTION stego_schema.release_stop(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION stego_schema.release_stop(text) TO %[1]s;
`

// sealedField gives the field of the seal change in the context: the readable secret that it moves, with its clear
// column, the field's name, and its sealed column.
func sealedField(ctx gen.Context, change types.SchemaChange) (field types.Field, clear, sealed string, err error) {
	for _, entity := range ctx.Entities {
		if entity.Name != change.Entity {
			continue
		}
		for _, f := range entity.Fields {
			if f.Name == change.Field && f.Sealed() {
				return f, f.Name, gen.SealColumn(f.Name), nil
			}
		}
	}
	return types.Field{}, "", "", fmt.Errorf("the seal names the field %s.%s, which the declaration does not declare as a readable secret", change.Entity, change.Field)
}

// withSealing gives the tables after a step between the expand and the contract step of the seal change: the clear
// column, with its CHECK and its indexes from the tables before the expand step, stays, and the sealed column is
// nullable until the contract step (ST-SUP-RC01-OC49).
func withSealing(ctx gen.Context, models, earlier []tableModel, change types.SchemaChange) ([]tableModel, error) {
	_, clear, sealed, err := sealedField(ctx, change)
	if err != nil {
		return nil, err
	}
	name := tableName(change.Entity)
	old, ok := findTable(earlier, name)
	if !ok {
		return nil, fmt.Errorf("the seal names the entity %s, whose table is not there before its expand step", change.Entity)
	}
	if _, ok := findColumn(old, clear); !ok {
		return nil, fmt.Errorf("the seal of %s.%s finds no clear column %s before its expand step", change.Entity, change.Field, clear)
	}
	out := withColumnOf(models, earlier, change, clear)
	for i := range out {
		if out[i].name != name {
			continue
		}
		out[i].columns = slices.Clone(out[i].columns)
		for j := range out[i].columns {
			if out[i].columns[j].name == sealed {
				out[i].columns[j].notNull = false
			}
		}
	}
	return out, nil
}

// sealExpandSQL gives the migration of the expand step of a seal at the position of the schema history, from the
// tables before and after it (ST-SUP-RC01-OC49): it adds the sealed column, nullable bytea with no default, and in
// the same migration makes the table of the release reports, the table of the seal checkpoints and the row of the
// change. The functions of the seal checkpoints are not in the body: the migration job runs them (sealCheckpoints).
// It gives also the earlier digests of the step, the digests of the bodies that earlier versions of the adapter wrote
// with the functions in them (sealsHistory). It refuses a step that changes any other table or column, a field in clear that is a key, in
// a unique key or a reference, and a table with a column of the name of the sealed column.
func sealExpandSQL(ctx gen.Context, position int, before, after []tableModel, change types.SchemaChange) (string, []string, error) {
	runtime, _, err := serviceRoles(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("a seal needs the release reports and the seal job of the tenancy component: %w", err)
	}
	_, clear, sealed, err := sealedField(ctx, change)
	if err != nil {
		return "", nil, err
	}
	name := tableName(change.Entity)
	old, okOld := findTable(before, name)
	next, okNext := findTable(after, name)
	if !okOld || !okNext || !old.owned {
		return "", nil, fmt.Errorf("the seal names the entity %s, whose table is not there or no tenant owns; a sealed value is bound to its tenant", change.Entity)
	}
	if len(before) != len(after) {
		return "", nil, fmt.Errorf("the seal of %s.%s adds or removes a table", change.Entity, change.Field)
	}
	for _, table := range before {
		if table.name == name {
			continue
		}
		if other, ok := findTable(after, table.name); !ok || !reflect.DeepEqual(other, table) {
			return "", nil, fmt.Errorf("the seal of %s.%s changes the table %s", change.Entity, change.Field, table.name)
		}
	}
	if _, clash := findColumn(old, sealed); clash {
		return "", nil, fmt.Errorf("the table %s has a column %s already, so the seal cannot add it", name, sealed)
	}
	if fixedColumn(old, clear) {
		return "", nil, fmt.Errorf("the seal of %s.%s names a column that STEGO adds to each table itself", change.Entity, change.Field)
	}
	for _, fk := range old.foreign {
		if slices.Contains(fk.from, clear) {
			return "", nil, fmt.Errorf("the seal of %s.%s is of a reference, which is never secret", change.Entity, change.Field)
		}
	}
	u, ok := findColumn(next, sealed)
	if !ok || u.typ != "bytea" || u.notNull || u.defaultClause != "" {
		return "", nil, fmt.Errorf("the seal of %s.%s finds no nullable bytea column %s after its expand step", change.Entity, change.Field, sealed)
	}
	// Apart from the sealed column, the table after the step is the table before it.
	same := next
	same.columns = slices.DeleteFunc(slices.Clone(next.columns), func(c columnModel) bool { return c.name == sealed })
	if !reflect.DeepEqual(normalized(same), normalized(old)) {
		return "", nil, fmt.Errorf("the seal of %s.%s changes more of the table than its sealed column", change.Entity, change.Field)
	}
	table := "public." + sqlIdentifier(name)
	expand := StepMigrationName(position, change.ID, "expand")
	var b strings.Builder
	add := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s bytea;\n", table, sqlIdentifier(sealed))
	// The release reports, so the seal job and the contract see a release before that still runs
	// (ST-SUP-RP09-LC14), and the seal checkpoint of the change; the migration job then runs the functions of the
	// seal checkpoints with EXECUTE for the runtime role, which runs the seal job of the controller
	// (ST-SUP-RP09-LC15).
	releases := fmt.Sprintf(releasesText, sqlIdentifier(runtime))
	row := fmt.Sprintf("INSERT INTO stego_schema.seals (change_id, expand) VALUES (%s, %s) ON CONFLICT (change_id) DO NOTHING;\n", sqlString(change.ID), sqlString(expand))
	b.WriteString(add + releases + sealsTable + row)
	var earlier []string
	for _, text := range sealsHistory {
		earlier = append(earlier, ledgerDigest(add+releases+fmt.Sprintf(text, sqlIdentifier(runtime))+row))
	}
	return b.String(), earlier, nil
}

// sealContractSQL gives the contract step of a seal (ST-SUP-RC01-OC50): before is the tables of the step before the
// contract, with the clear column and the nullable sealed column, and after the tables after it, with the sealed
// column alone, NOT NULL for a required field. The check of the step (checkContract of the ledger, Seal) refuses
// while a release before runs, while the seal pass is not complete, did not start in this database instance or a
// release before ran after its start, and
// while a row of a listed tenant has a clear value and no sealed value; the statements then add the completeness
// CHECK NOT VALID and validate it, which reads every row past row-level security; for a required field make the
// sealed column NOT NULL as the contract of a require does; and in one transaction, after the check again, drop the
// completeness CHECK and the clear column and delete the seal checkpoint of the change.
func sealContractSQL(ctx gen.Context, before, after []tableModel, change types.SchemaChange) (*contractStep, error) {
	field, clear, sealed, err := sealedField(ctx, change)
	if err != nil {
		return nil, err
	}
	name := tableName(change.Entity)
	old, okOld := findTable(before, name)
	next, okNext := findTable(after, name)
	if !okOld || !okNext {
		return nil, fmt.Errorf("the contract of the seal names the entity %s, whose table is not there", change.Entity)
	}
	if _, ok := findColumn(old, clear); !ok {
		return nil, fmt.Errorf("the contract of the seal of %s.%s finds no clear column %s", change.Entity, change.Field, clear)
	}
	if _, kept := findColumn(next, clear); kept {
		return nil, fmt.Errorf("the table %s keeps the clear column %s after the contract of its seal", name, clear)
	}
	u, ok := findColumn(next, sealed)
	if !ok || u.notNull == field.Optional {
		return nil, fmt.Errorf("the contract of the seal of %s.%s finds no sealed column %s of the field after it", change.Entity, change.Field, sealed)
	}
	table := "public." + sqlIdentifier(name)
	regclass := sqlString(table) + "::pg_catalog.regclass"
	complete := CompleteCheckName(change.Entity, sealed)
	notNull := NotNullCheckName(change.Entity, sealed)
	constraint := func(n string) string {
		return "SELECT 1 FROM pg_catalog.pg_constraint c WHERE c.conrelid = " + regclass + " AND c.conname = " + sqlString(n)
	}
	column := "EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = " + regclass + " AND a.attname = " + sqlString(clear) + " AND a.attnum > 0 AND NOT a.attisdropped)"
	nullable := "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = " + regclass + " AND a.attname = " + sqlString(sealed) +
		" AND a.attnum > 0 AND NOT a.attisdropped AND NOT a.attnotnull)"
	tenant := ""
	if old.owned {
		tenant = tenantColumn(ctx)
	}
	out := &contractStep{change: change.ID, table: table, schema: "public", tableName: name, from: clear, seal: true, check: complete,
		old: sqlIdentifier(clear), new: sqlIdentifier(sealed), tenant: tenant, tenantSchema: "public"}
	out.statements = []contractStatement{
		{sql: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID", table, sqlIdentifier(complete), completeExpression(clear, sealed)),
			when: "SELECT " + column + " AND NOT EXISTS (" + constraint(complete) + ")", guard: true},
		{sql: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", table, sqlIdentifier(complete)), when: "SELECT EXISTS (" + constraint(complete) + " AND NOT c.convalidated)", long: true},
	}
	if !field.Optional {
		out.statements = append(out.statements,
			contractStatement{sql: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID", table, sqlIdentifier(notNull), sqlIdentifier(sealed)),
				when: nullable + " AND NOT EXISTS (" + constraint(notNull) + ")"},
			contractStatement{sql: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", table, sqlIdentifier(notNull)), when: "SELECT EXISTS (" + constraint(notNull) + ")", long: true},
			contractStatement{sql: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, sqlIdentifier(sealed)), when: nullable},
			contractStatement{sql: fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", table, sqlIdentifier(notNull))})
	}
	out.statements = append(out.statements, contractStatement{sql: fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", table, sqlIdentifier(complete)), guard: true,
		also: []string{fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s", table, sqlIdentifier(clear)),
			fmt.Sprintf("DELETE FROM stego_schema.seals WHERE change_id = %s", sqlString(change.ID))}})
	return out, nil
}

// openSeals gives, for each table, the optional constraints of each seal whose last step in the declaration is
// expand, which its contract step adds NOT VALID and validates while this release runs, and puts back in the models
// the clear column of each, which this release writes, with the sealed column nullable (ST-SUP-RC01-OC49). It gives
// the seal job of each too (sealColumns).
func openSeals(ctx gen.Context, models []tableModel, optional map[string][]syncTrigger) ([]tableModel, error) {
	last := map[string]string{}
	for _, step := range ctx.SchemaHistory {
		last[step.Change] = step.Phase
	}
	for _, change := range ctx.SchemaChanges {
		if change.Kind != "seal" || last[change.ID] != "expand" {
			continue
		}
		expand := -1
		for i, step := range ctx.SchemaHistory {
			if step.Change == change.ID && step.Phase == "expand" {
				expand = i
			}
		}
		earlierCtx, err := ctx.AtSchemaStep(expand)
		if err != nil {
			return nil, err
		}
		earlier, err := tableModels(earlierCtx)
		if err != nil {
			return nil, err
		}
		if models, err = withSealing(ctx, models, earlier, change); err != nil {
			return nil, fmt.Errorf("schema change %s: %w", change.ID, err)
		}
		field, clear, sealed, err := sealedField(ctx, change)
		if err != nil {
			return nil, err
		}
		name := tableName(change.Entity)
		// The release of the expand step goes on after the contract step drops the clear column (ST-SUP-RC01-OC49),
		// so the manifest names it as droppable, and a start without it is ready (ST-SUP-RC01-OC12).
		for i := range models {
			if models[i].name != name {
				continue
			}
			for j := range models[i].columns {
				if models[i].columns[j].name == clear {
					models[i].columns[j].droppable = true
				}
			}
		}
		trigger := syncTrigger{notNull: CompleteCheckName(change.Entity, sealed), notNullDefinition: completeDefinition(clear, sealed)}
		optional[name] = append(optional[name], trigger)
		if !field.Optional {
			optional[name] = append(optional[name], syncTrigger{notNull: NotNullCheckName(change.Entity, sealed),
				notNullDefinition: "CHECK ((" + deparsedName(sealed) + " IS NOT NULL))"})
		}
	}
	return models, nil
}

// sealColumns gives the source of the seal job of each seal whose last step in the declaration is expand
// (ST-SUP-RP10-LC50, ST-SUP-RC32-OC23): its change, entity, table and columns, and its statements, which take the
// tenant as $1, page by the ID after $2 and read FOR UPDATE.
func sealColumns(ctx gen.Context) (string, int, error) {
	last := map[string]string{}
	for _, step := range ctx.SchemaHistory {
		last[step.Change] = step.Phase
	}
	key := tenantColumn(ctx)
	var b bytes.Buffer
	count := 0
	for _, change := range ctx.SchemaChanges {
		if change.Kind != "seal" || last[change.ID] != "expand" {
			continue
		}
		_, clear, sealed, err := sealedField(ctx, change)
		if err != nil {
			return "", 0, err
		}
		if key == "" {
			return "", 0, fmt.Errorf("the seal %s needs the tenant column of the tenancy component", change.ID)
		}
		table := "public." + sqlIdentifier(tableName(change.Entity))
		tenant, id := sqlIdentifier(key), sqlIdentifier(keyColumn)
		fmt.Fprintf(&b, "\t{Change: %q, Entity: %q, Table: %q, Clear: %q, Sealed: %q,\n", change.ID, change.Entity, tableName(change.Entity), clear, sealed)
		fmt.Fprintf(&b, "\t\tSelect: %q,\n", fmt.Sprintf("SELECT %s::text, %s::text, %s FROM %s WHERE %s = $1 AND %s > $2::uuid ORDER BY %s LIMIT $3 FOR UPDATE",
			id, sqlIdentifier(clear), sqlIdentifier(sealed), table, tenant, id, id))
		fmt.Fprintf(&b, "\t\tUpdate: %q},\n", fmt.Sprintf("UPDATE %s SET %s = $3 WHERE %s = $1 AND %s = $2::uuid", table, sqlIdentifier(sealed), tenant, id))
		count++
	}
	return b.String(), count, nil
}

// ledgerDigest is the ledger digest of a migration body, as Digest of the generated ledger gives it: the SHA-256 of
// the body in hexadecimal.
func ledgerDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// sealCheckpointsOf gives the text of the seal checkpoints with the runtime role of the service, which the ledger
// runs outside the bodies (RegisterSealCheckpoints), when a step is the expand step of a seal, and else empty.
func sealCheckpointsOf(ctx gen.Context, steps []schemaStep) (string, error) {
	if !slices.ContainsFunc(steps, func(s schemaStep) bool { return s.Seal }) {
		return "", nil
	}
	runtime, _, err := serviceRoles(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(sealCheckpoints, sqlIdentifier(runtime)), nil
}

// sealsBefore gives sealsAgain for the body of a step of the change, with the runtime role of the service, and none
// when no seal step at expand comes before the change.
func sealsBefore(ctx gen.Context, change, body string) ([]string, error) {
	kinds := map[string]string{}
	for _, c := range ctx.SchemaChanges {
		kinds[c.ID] = c.Kind
	}
	if !slices.ContainsFunc(ctx.SchemaHistory, func(s types.SchemaStep) bool { return kinds[s.Change] == "seal" && s.Phase == "expand" }) {
		return nil, nil
	}
	runtime, _, err := serviceRoles(ctx)
	if err != nil {
		return nil, err
	}
	return sealsAgain(ctx, change, runtime, body), nil
}

// sealsAgain gives the earlier digests of the migration body of a step of the change that makes the release reports
// again, when a seal step at expand comes before it in the schema history, and else none: earlier versions of the
// adapter put the text of the seal checkpoints after the release reports in such a body, because the text of the
// release reports makes release_stop delete the report of a stopped process, and the seal checkpoints need
// release_stop to keep the report of a release before an open seal (ST-SUP-RP09-LC15). The body holds it no more:
// the migration job runs the seal checkpoints after each migration in the same transaction (sealCheckpoints).
func sealsAgain(ctx gen.Context, change, runtime, body string) []string {
	kinds := map[string]string{}
	for _, c := range ctx.SchemaChanges {
		kinds[c.ID] = c.Kind
	}
	for _, step := range ctx.SchemaHistory {
		if step.Change == change {
			return nil
		}
		if kinds[step.Change] == "seal" && step.Phase == "expand" {
			var earlier []string
			for _, text := range sealsHistory {
				earlier = append(earlier, ledgerDigest(body+fmt.Sprintf(text, sqlIdentifier(runtime))))
			}
			return earlier
		}
	}
	return nil
}

== 001_initial.sql
CREATE TABLE public."notes" (
  "tenant_id" text NOT NULL DEFAULT current_setting('app.tenant'),
  "id" uuid NOT NULL,
  "title" character varying(200) NOT NULL,
  "summary" character varying(1000),
  "created_time" timestamp with time zone NOT NULL DEFAULT now(),
  "updated_time" timestamp with time zone NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  CONSTRAINT "notes_pkey" PRIMARY KEY ("tenant_id", "id")
);
CREATE INDEX "notes_title_search" ON public."notes" ("tenant_id", "title");
CREATE INDEX "notes_summary_search" ON public."notes" ("tenant_id", "summary");
== step_0001_note-summary-drop_expand.sql
CREATE TABLE IF NOT EXISTS stego_schema.releases (
	holder text PRIMARY KEY,
	release text NOT NULL,
	version text NOT NULL,
	every_ms integer NOT NULL,
	seen_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS releases_seen_at ON stego_schema.releases (seen_at);
REVOKE ALL ON TABLE stego_schema.releases FROM PUBLIC;
CREATE OR REPLACE FUNCTION stego_schema.release_beat(text, text, text, integer)
 RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_release$BEGIN
  IF $1 IS NULL OR $2 IS NULL OR $3 IS NULL OR $4 IS NULL OR $1 !~ '^[0-9a-f]{32}$' OR $2 !~ '^[0-9a-z_-]{1,200}$'
   OR $3 !~ '^[ -~]{0,64}$' OR $4 < 1000 OR $4 > 60000 THEN
    RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = 'stego release report: an argument is out of range';
  END IF;
  INSERT INTO stego_schema.releases AS r (holder, release, version, every_ms, seen_at)
   VALUES ($1, $2, $3, $4, pg_catalog.clock_timestamp())
   ON CONFLICT (holder) DO UPDATE SET release = EXCLUDED.release, version = EXCLUDED.version, every_ms = EXCLUDED.every_ms,
   seen_at = EXCLUDED.seen_at;
END$stego_release$;
CREATE OR REPLACE FUNCTION stego_schema.release_stop(text)
 RETURNS void LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp SET lock_timeout = '1s'
 AS $stego_release$DELETE FROM stego_schema.releases r WHERE r.holder = $1$stego_release$;
REVOKE ALL ON FUNCTION stego_schema.release_beat(text, text, text, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION stego_schema.release_stop(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA stego_schema TO "tables_check_runtime";
GRANT EXECUTE ON FUNCTION stego_schema.release_beat(text, text, text, integer) TO "tables_check_runtime";
GRANT EXECUTE ON FUNCTION stego_schema.release_stop(text) TO "tables_check_runtime";
== step_0002_note-summary-drop_contract.sql
ALTER TABLE public."notes" DROP COLUMN IF EXISTS "summary";
DO $purge$ DECLARE t text; BEGIN IF pg_catalog.to_regclass('public.attempt_counts') IS NULL OR pg_catalog.to_regclass('public.stego_tenants') IS NULL THEN RETURN; END IF; FOR t IN SELECT s.tenant_id FROM public.stego_tenants s ORDER BY s.tenant_id COLLATE "C" LOOP PERFORM pg_catalog.set_config('app.tenant', t, true); DELETE FROM public.attempt_counts WHERE tenant_id = t AND counter = 'Note.summary'; END LOOP; PERFORM pg_catalog.set_config('app.tenant', '', true); END $purge$;

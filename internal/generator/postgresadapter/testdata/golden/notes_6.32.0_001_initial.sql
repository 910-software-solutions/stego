CREATE TABLE public."notes" (
  "tenant_id" text NOT NULL DEFAULT current_setting('app.tenant'),
  "id" uuid NOT NULL,
  "title" character varying(200) NOT NULL,
  "body" character varying(4000),
  "summary" character varying(1000),
  "tag" character varying(40),
  "created_time" timestamp with time zone NOT NULL DEFAULT now(),
  "updated_time" timestamp with time zone NOT NULL DEFAULT now(),
  "version" bigint NOT NULL DEFAULT 1,
  CONSTRAINT "notes_pkey" PRIMARY KEY ("tenant_id", "id")
);
CREATE INDEX "notes_title_search" ON public."notes" ("tenant_id", "title");
CREATE INDEX "notes_body_search" ON public."notes" ("tenant_id", "body");
CREATE INDEX "notes_summary_search" ON public."notes" ("tenant_id", "summary");
CREATE INDEX "notes_tag_search" ON public."notes" ("tenant_id", "tag");

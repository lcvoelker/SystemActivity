-- One-time setup, run as a superuser (postgres) against the filemaster database:
--
--   psql -h localhost -U postgres -d filemaster -f db/bootstrap.sql
--
-- Safe to re-run. Afterwards, set passwords with \password fm_owner and
-- \password fm_app if the roles were created here, then run
-- `filemaster migrate up` as fm_owner.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'fm_owner') THEN
        CREATE ROLE fm_owner LOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'fm_app') THEN
        CREATE ROLE fm_app LOGIN;
    END IF;
END
$$;

GRANT CONNECT ON DATABASE filemaster TO fm_owner, fm_app;

-- Keep application objects out of public.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

CREATE SCHEMA IF NOT EXISTS filemaster AUTHORIZATION fm_owner;
GRANT USAGE ON SCHEMA filemaster TO fm_app;

-- Anything fm_owner creates in the schema later is usable by fm_app,
-- but fm_app can never create, alter or drop objects itself.
ALTER DEFAULT PRIVILEGES FOR ROLE fm_owner IN SCHEMA filemaster
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO fm_app;
ALTER DEFAULT PRIVILEGES FOR ROLE fm_owner IN SCHEMA filemaster
    GRANT USAGE, SELECT ON SEQUENCES TO fm_app;

-- Needed for the no-overlapping-versions exclusion constraint.
CREATE EXTENSION IF NOT EXISTS btree_gist SCHEMA filemaster;

ALTER ROLE fm_owner IN DATABASE filemaster SET search_path = filemaster;
ALTER ROLE fm_app IN DATABASE filemaster SET search_path = filemaster;

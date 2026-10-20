-- +goose Up
-- Notes: the partner's free text and the elements that failed their format.
-- They never reach the agent as text; the quarantined reader (edi.extract_note)
-- is the only way to get closed values out of one. quarantine_calls is the
-- cost record of every model call (tokens and latency per trace id).

CREATE TABLE edi_notes (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id  uuid NOT NULL REFERENCES edi_documents (id),
    tenant_id    text NOT NULL,
    ref          text NOT NULL,
    segment_path text NOT NULL,
    reason       text NOT NULL CHECK (reason IN ('free_text', 'format')),
    body         text NOT NULL,
    taint_label  text NOT NULL,
    canary       text NOT NULL,
    UNIQUE (document_id, ref)
);

CREATE TABLE quarantine_calls (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         text NOT NULL,
    trace_id          text NOT NULL,
    document_id       uuid NOT NULL,
    ref               text NOT NULL,
    schema_id         text NOT NULL,
    model             text NOT NULL,
    prompt_tokens     integer NULL,
    completion_tokens integer NULL,
    latency_ms        integer NOT NULL,
    outcome           text NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX quarantine_calls_tenant_trace ON quarantine_calls (tenant_id, trace_id);

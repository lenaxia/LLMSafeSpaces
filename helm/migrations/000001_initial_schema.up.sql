--
--




--
-- Name: update_updated_at_column(); Type: FUNCTION; Schema: public; Owner: -
--

CREATE OR REPLACE FUNCTION public.update_updated_at_column() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$;




--
-- Name: api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.api_keys (
    id character varying(36) NOT NULL,
    user_id character varying(36) NOT NULL,
    key character varying(255) NOT NULL,
    name character varying(255) NOT NULL,
    active boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    key_prefix character varying(12) DEFAULT ''::character varying NOT NULL,
    key_legacy boolean DEFAULT false NOT NULL,
    decrypt_access boolean DEFAULT false NOT NULL,
    kek_salt bytea,
    wrapped_dek bytea,
    dek_synced boolean DEFAULT false NOT NULL,
    key_ciphertext bytea,
    allowed_cidrs text[],
    key_version integer DEFAULT 1 NOT NULL
);


--
-- Name: audit_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.audit_log (
    id bigint NOT NULL,
    actor_id text NOT NULL,
    domain text NOT NULL,
    action text NOT NULL,
    target_id text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    org_id uuid,
    CONSTRAINT audit_log_domain_chk CHECK ((domain = ANY (ARRAY['billing'::text, 'secrets'::text, 'admin'::text, 'org'::text])))
);


--
-- Name: audit_log_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.audit_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: audit_log_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.audit_log_id_seq OWNED BY public.audit_log.id;


--
-- Name: billing_accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.billing_accounts (
    id bigint NOT NULL,
    owner_id text NOT NULL,
    owner_type text DEFAULT 'user'::text NOT NULL,
    provider text NOT NULL,
    external_customer_id text NOT NULL,
    external_subscription_id text,
    status text DEFAULT 'active'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: billing_accounts_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.billing_accounts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: billing_accounts_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.billing_accounts_id_seq OWNED BY public.billing_accounts.id;


--
-- Name: billing_export_cursor; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.billing_export_cursor (
    provider text NOT NULL,
    last_exported_id bigint DEFAULT 0 NOT NULL,
    last_exported_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: credential_auto_apply; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.credential_auto_apply (
    credential_id uuid NOT NULL,
    target_type text NOT NULL,
    target_id text,
    within_priority integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT credential_auto_apply_target_type_check CHECK ((target_type = ANY (ARRAY['user'::text, 'org'::text, 'all'::text])))
);


--
-- Name: credential_backfill_jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.credential_backfill_jobs (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    credential_id uuid NOT NULL,
    status text DEFAULT 'running'::text NOT NULL,
    processed integer DEFAULT 0 NOT NULL,
    errors jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT credential_backfill_jobs_status_check CHECK ((status = ANY (ARRAY['running'::text, 'complete'::text, 'failed'::text])))
);


--
-- Name: email_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.email_tokens (
    id text NOT NULL,
    user_id text NOT NULL,
    kind text NOT NULL,
    token_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT email_tokens_kind_check CHECK ((kind = ANY (ARRAY['password_reset'::text, 'email_verify'::text])))
);


--
-- Name: instance_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.instance_settings (
    key text NOT NULL,
    value jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: jwt_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.jwt_sessions (
    jti uuid NOT NULL,
    user_id text NOT NULL,
    wrapped_dek bytea NOT NULL,
    kek_salt bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL
);


--
-- Name: org_invitations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.org_invitations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    org_id uuid NOT NULL,
    email text NOT NULL,
    role text DEFAULT 'member'::text NOT NULL,
    invited_by text NOT NULL,
    token_hash text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    accepted_at timestamp with time zone,
    accepted_by text,
    declined_at timestamp with time zone,
    bounce_type text,
    bounced_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT org_invitations_bounce_type_check CHECK ((bounce_type = ANY (ARRAY['permanent'::text, 'transient'::text, 'complaint'::text]))),
    CONSTRAINT org_invitations_role_check CHECK ((role = ANY (ARRAY['admin'::text, 'member'::text])))
);


--
-- Name: org_memberships; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.org_memberships (
    org_id uuid NOT NULL,
    user_id text NOT NULL,
    role text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT org_memberships_role_check CHECK ((role = ANY (ARRAY['admin'::text, 'member'::text])))
);


--
-- Name: org_policies; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.org_policies (
    org_id uuid NOT NULL,
    key text NOT NULL,
    value jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_by text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT org_policies_key_check CHECK ((key = ANY (ARRAY['allowed_models'::text, 'allowed_providers'::text, 'max_workspaces_per_member'::text, 'max_active_workspaces_per_member'::text])))
);


--
-- Name: org_sso_configs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.org_sso_configs (
    org_id uuid NOT NULL,
    oidc_discovery_url text NOT NULL,
    oidc_client_id text NOT NULL,
    oidc_client_secret bytea NOT NULL,
    claimed_domains text[] DEFAULT '{}'::text[] NOT NULL,
    auto_provision boolean DEFAULT true NOT NULL,
    group_role_mapping jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    verified_domains text[] DEFAULT '{}'::text[] NOT NULL,
    verification_token text,
    key_version integer DEFAULT 1 NOT NULL
);


--
-- Name: organizations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.organizations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    status text DEFAULT 'pending_activation'::text NOT NULL,
    plan_id text DEFAULT 'free'::text NOT NULL,
    subscription_status text DEFAULT 'inactive'::text NOT NULL,
    CONSTRAINT organizations_status_check CHECK ((status = ANY (ARRAY['pending_activation'::text, 'active'::text, 'suspended'::text]))),
    CONSTRAINT organizations_subscription_status_check CHECK ((subscription_status = ANY (ARRAY['inactive'::text, 'active'::text, 'trialing'::text, 'past_due'::text, 'canceled'::text, 'unpaid'::text])))
);


--
-- Name: permissions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.permissions (
    id character varying(36) NOT NULL,
    user_id character varying(36) NOT NULL,
    resource_type character varying(255) NOT NULL,
    resource_id character varying(255) NOT NULL,
    action character varying(255) NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: provider_credentials; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.provider_credentials (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    owner_type text NOT NULL,
    owner_id text NOT NULL,
    name text NOT NULL,
    ciphertext bytea NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    model_allowlist text[] DEFAULT '{}'::text[] NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    model_context_limits jsonb DEFAULT '{}'::jsonb NOT NULL,
    model_output_limits jsonb DEFAULT '{}'::jsonb NOT NULL,
    kind text NOT NULL,
    slug text NOT NULL,
    CONSTRAINT provider_credentials_kind_check CHECK ((kind = ANY (ARRAY['openai'::text, 'anthropic'::text, 'google'::text, 'opencode'::text, 'bedrock'::text, 'azure_openai'::text, 'vertex'::text, 'cohere'::text, 'mistral'::text, 'perplexity'::text, 'groq'::text, 'xai'::text, 'openrouter'::text, 'together'::text, 'openai_compatible'::text]))),
    CONSTRAINT provider_credentials_owner_type_check CHECK ((owner_type = ANY (ARRAY['user'::text, 'org'::text, 'admin'::text]))),
    CONSTRAINT provider_credentials_slug_check CHECK ((slug ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'::text))
);




-- schema_migrations table omitted — golang-migrate creates and manages
-- it on first apply.


--
-- Name: secret_audit_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.secret_audit_log (
    id bigint NOT NULL,
    user_id character varying(36) NOT NULL,
    action character varying(50) NOT NULL,
    secret_id uuid,
    workspace_id character varying(36),
    metadata jsonb DEFAULT '{}'::jsonb,
    "timestamp" timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: secret_audit_log_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.secret_audit_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: secret_audit_log_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.secret_audit_log_id_seq OWNED BY public.secret_audit_log.id;


--
-- Name: session_index; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.session_index (
    workspace_id text NOT NULL,
    session_id text NOT NULL,
    title text,
    last_message_at timestamp with time zone,
    message_count integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    parent_session_id text,
    last_seen_at timestamp with time zone,
    context_used bigint
);


--
-- Name: stripe_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.stripe_events (
    event_id text NOT NULL,
    event_type text NOT NULL,
    processed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: usage_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.usage_events (
    id bigint NOT NULL,
    idempotency_key text,
    owner_id text NOT NULL,
    owner_type text DEFAULT 'user'::text NOT NULL,
    actor_id text NOT NULL,
    workspace_id text,
    event_type text NOT NULL,
    event_subtype text,
    quantity bigint NOT NULL,
    resource_tier text,
    region text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    request_context jsonb,
    source text DEFAULT 'api'::text NOT NULL,
    event_time timestamp with time zone NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    period date DEFAULT CURRENT_DATE NOT NULL,
    CONSTRAINT usage_events_event_type_check CHECK ((event_type = ANY (ARRAY['compute_seconds'::text, 'llm_request'::text, 'llm_tokens'::text, 'storage_bytes'::text, 'api_call'::text]))),
    CONSTRAINT usage_events_owner_type_check CHECK ((owner_type = ANY (ARRAY['user'::text, 'org'::text]))),
    CONSTRAINT usage_events_quantity_check CHECK ((quantity >= 0)),
    CONSTRAINT usage_events_source_check CHECK ((source = ANY (ARRAY['api'::text, 'controller'::text, 'cron'::text, 'reconciliation'::text])))
);


--
-- Name: usage_events_dlq; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.usage_events_dlq (
    id bigint NOT NULL,
    payload jsonb NOT NULL,
    error_message text NOT NULL,
    retry_count integer DEFAULT 0 NOT NULL,
    first_failed_at timestamp with time zone DEFAULT now() NOT NULL,
    last_retried_at timestamp with time zone,
    resolved_at timestamp with time zone,
    resolution text,
    CONSTRAINT usage_events_dlq_resolution_check CHECK ((resolution = ANY (ARRAY['reprocessed'::text, 'discarded'::text, 'dead'::text]))),
    CONSTRAINT usage_events_dlq_retry_count_check CHECK ((retry_count >= 0))
);


--
-- Name: usage_events_dlq_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.usage_events_dlq_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: usage_events_dlq_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.usage_events_dlq_id_seq OWNED BY public.usage_events_dlq.id;


--
-- Name: usage_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.usage_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: usage_events_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.usage_events_id_seq OWNED BY public.usage_events.id;


--
-- Name: usage_limits; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.usage_limits (
    id bigint NOT NULL,
    owner_id text NOT NULL,
    owner_type text DEFAULT 'user'::text NOT NULL,
    event_type text NOT NULL,
    period_type text DEFAULT 'monthly'::text NOT NULL,
    max_quantity bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT usage_limits_max_quantity_check CHECK ((max_quantity > 0)),
    CONSTRAINT usage_limits_period_type_check CHECK ((period_type = ANY (ARRAY['daily'::text, 'monthly'::text, 'lifetime'::text])))
);


--
-- Name: usage_limits_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.usage_limits_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: usage_limits_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.usage_limits_id_seq OWNED BY public.usage_limits.id;


--
-- Name: user_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.user_keys (
    user_id character varying(36) NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    wrapped_dek bytea NOT NULL,
    wrapped_dek_recovery bytea,
    salt bytea NOT NULL,
    recovery_salt bytea,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    rotated_at timestamp with time zone
);


--
-- Name: user_secret_bindings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.user_secret_bindings (
    secret_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: user_secrets; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.user_secrets (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id character varying(36) NOT NULL,
    name character varying(255) NOT NULL,
    type character varying(50) NOT NULL,
    ciphertext bytea NOT NULL,
    key_version integer NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: user_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.user_settings (
    user_id character varying(36) NOT NULL,
    key text NOT NULL,
    value jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.users (
    id character varying(36) NOT NULL,
    username character varying(255) NOT NULL,
    email character varying(255) NOT NULL,
    password_hash character varying(255) NOT NULL,
    active boolean DEFAULT true NOT NULL,
    role character varying(50) DEFAULT 'user'::character varying NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    plan_id text DEFAULT 'free'::text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    email_verified boolean DEFAULT false NOT NULL,
    CONSTRAINT users_status_check CHECK ((status = ANY (ARRAY['active'::text, 'suspended'::text])))
);


--
-- Name: workspace_agent_state; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.workspace_agent_state (
    workspace_id uuid NOT NULL,
    last_credential_changed_at timestamp with time zone,
    last_agent_disposed_at timestamp with time zone,
    pending_refresh boolean DEFAULT false NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: workspace_credential_bindings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.workspace_credential_bindings (
    credential_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    source_type text DEFAULT 'explicit'::text NOT NULL,
    within_priority integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT workspace_credential_bindings_source_type_check CHECK ((source_type = ANY (ARRAY['explicit'::text, 'auto'::text])))
);


--
-- Name: workspace_lifecycle_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.workspace_lifecycle_events (
    id bigint NOT NULL,
    workspace_id text NOT NULL,
    owner_id text NOT NULL,
    owner_type text DEFAULT 'user'::text NOT NULL,
    from_phase text,
    to_phase text NOT NULL,
    resource_tier text,
    event_time timestamp with time zone NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: workspace_lifecycle_events_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

CREATE SEQUENCE IF NOT EXISTS public.workspace_lifecycle_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: workspace_lifecycle_events_id_seq; Type: SEQUENCE OWNED BY; Schema: public; Owner: -
--

ALTER SEQUENCE public.workspace_lifecycle_events_id_seq OWNED BY public.workspace_lifecycle_events.id;


--
-- Name: workspaces; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE IF NOT EXISTS public.workspaces (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name character varying(255) NOT NULL,
    user_id character varying(36) NOT NULL,
    namespace character varying(255) DEFAULT 'default'::character varying NOT NULL,
    runtime character varying(255),
    security_level character varying(50) DEFAULT 'standard'::character varying,
    storage_size character varying(50) DEFAULT '5Gi'::character varying,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    image_tag text DEFAULT ''::text NOT NULL,
    agent_version text DEFAULT ''::text NOT NULL,
    default_model character varying(255),
    org_id uuid
);


--
-- Name: audit_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_log ALTER COLUMN id SET DEFAULT nextval('public.audit_log_id_seq'::regclass);


--
-- Name: billing_accounts id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.billing_accounts ALTER COLUMN id SET DEFAULT nextval('public.billing_accounts_id_seq'::regclass);


--
-- Name: secret_audit_log id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.secret_audit_log ALTER COLUMN id SET DEFAULT nextval('public.secret_audit_log_id_seq'::regclass);


--
-- Name: usage_events id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_events ALTER COLUMN id SET DEFAULT nextval('public.usage_events_id_seq'::regclass);


--
-- Name: usage_events_dlq id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_events_dlq ALTER COLUMN id SET DEFAULT nextval('public.usage_events_dlq_id_seq'::regclass);


--
-- Name: usage_limits id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.usage_limits ALTER COLUMN id SET DEFAULT nextval('public.usage_limits_id_seq'::regclass);


--
-- Name: workspace_lifecycle_events id; Type: DEFAULT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.workspace_lifecycle_events ALTER COLUMN id SET DEFAULT nextval('public.workspace_lifecycle_events_id_seq'::regclass);


--
-- Name: api_keys api_keys_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_key_key UNIQUE (key);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: api_keys api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: audit_log audit_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.audit_log
    ADD CONSTRAINT audit_log_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: billing_accounts billing_accounts_owner_id_owner_type_provider_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.billing_accounts
    ADD CONSTRAINT billing_accounts_owner_id_owner_type_provider_key UNIQUE (owner_id, owner_type, provider);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: billing_accounts billing_accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.billing_accounts
    ADD CONSTRAINT billing_accounts_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: billing_export_cursor billing_export_cursor_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.billing_export_cursor
    ADD CONSTRAINT billing_export_cursor_pkey PRIMARY KEY (provider);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: credential_backfill_jobs credential_backfill_jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.credential_backfill_jobs
    ADD CONSTRAINT credential_backfill_jobs_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: email_tokens email_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.email_tokens
    ADD CONSTRAINT email_tokens_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: email_tokens email_tokens_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.email_tokens
    ADD CONSTRAINT email_tokens_token_hash_key UNIQUE (token_hash);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: instance_settings instance_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.instance_settings
    ADD CONSTRAINT instance_settings_pkey PRIMARY KEY (key);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: jwt_sessions jwt_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.jwt_sessions
    ADD CONSTRAINT jwt_sessions_pkey PRIMARY KEY (jti);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_invitations org_invitations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_invitations
    ADD CONSTRAINT org_invitations_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_invitations org_invitations_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_invitations
    ADD CONSTRAINT org_invitations_token_hash_key UNIQUE (token_hash);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_memberships org_memberships_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_memberships
    ADD CONSTRAINT org_memberships_pkey PRIMARY KEY (org_id, user_id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_policies org_policies_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_policies
    ADD CONSTRAINT org_policies_pkey PRIMARY KEY (org_id, key);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_sso_configs org_sso_configs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_sso_configs
    ADD CONSTRAINT org_sso_configs_pkey PRIMARY KEY (org_id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: organizations organizations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.organizations
    ADD CONSTRAINT organizations_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: permissions permissions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.permissions
    ADD CONSTRAINT permissions_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: permissions permissions_user_id_resource_type_resource_id_action_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.permissions
    ADD CONSTRAINT permissions_user_id_resource_type_resource_id_action_key UNIQUE (user_id, resource_type, resource_id, action);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: provider_credentials provider_credentials_owner_slug_uniq; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.provider_credentials
    ADD CONSTRAINT provider_credentials_owner_slug_uniq UNIQUE (owner_type, owner_id, slug);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: provider_credentials provider_credentials_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.provider_credentials
    ADD CONSTRAINT provider_credentials_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;




--
-- Name: secret_audit_log secret_audit_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.secret_audit_log
    ADD CONSTRAINT secret_audit_log_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: session_index session_index_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.session_index
    ADD CONSTRAINT session_index_pkey PRIMARY KEY (workspace_id, session_id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: stripe_events stripe_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.stripe_events
    ADD CONSTRAINT stripe_events_pkey PRIMARY KEY (event_id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: usage_events_dlq usage_events_dlq_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.usage_events_dlq
    ADD CONSTRAINT usage_events_dlq_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: usage_events usage_events_idempotency_key_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.usage_events
    ADD CONSTRAINT usage_events_idempotency_key_key UNIQUE (idempotency_key);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: usage_events usage_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.usage_events
    ADD CONSTRAINT usage_events_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: usage_limits usage_limits_owner_id_owner_type_event_type_period_type_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.usage_limits
    ADD CONSTRAINT usage_limits_owner_id_owner_type_event_type_period_type_key UNIQUE (owner_id, owner_type, event_type, period_type);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: usage_limits usage_limits_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.usage_limits
    ADD CONSTRAINT usage_limits_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_keys user_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_keys
    ADD CONSTRAINT user_keys_pkey PRIMARY KEY (user_id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_secret_bindings user_secret_bindings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_secret_bindings
    ADD CONSTRAINT user_secret_bindings_pkey PRIMARY KEY (secret_id, workspace_id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_secrets user_secrets_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_secrets
    ADD CONSTRAINT user_secrets_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_secrets user_secrets_user_id_name_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_secrets
    ADD CONSTRAINT user_secrets_user_id_name_key UNIQUE (user_id, name);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_settings user_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_settings
    ADD CONSTRAINT user_settings_pkey PRIMARY KEY (user_id, key);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: users users_email_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_email_key UNIQUE (email);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: users users_username_key; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_username_key UNIQUE (username);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: workspace_agent_state workspace_agent_state_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.workspace_agent_state
    ADD CONSTRAINT workspace_agent_state_pkey PRIMARY KEY (workspace_id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: workspace_credential_bindings workspace_credential_bindings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.workspace_credential_bindings
    ADD CONSTRAINT workspace_credential_bindings_pkey PRIMARY KEY (credential_id, workspace_id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: workspace_lifecycle_events workspace_lifecycle_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.workspace_lifecycle_events
    ADD CONSTRAINT workspace_lifecycle_events_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: workspaces workspaces_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_pkey PRIMARY KEY (id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: idx_api_keys_key_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key_active ON public.api_keys USING btree (key) WHERE (active = true);


--
-- Name: idx_api_keys_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON public.api_keys USING btree (user_id);


--
-- Name: idx_audit_actor; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_audit_actor ON public.audit_log USING btree (actor_id, created_at DESC);


--
-- Name: idx_audit_domain; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_audit_domain ON public.audit_log USING btree (domain, created_at DESC);


--
-- Name: idx_audit_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_audit_org ON public.audit_log USING btree (org_id, created_at DESC) WHERE (org_id IS NOT NULL);


--
-- Name: idx_audit_user_time; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_audit_user_time ON public.secret_audit_log USING btree (user_id, "timestamp" DESC);


--
-- Name: idx_cred_auto_apply_all; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_cred_auto_apply_all ON public.credential_auto_apply USING btree (target_type) WHERE (target_type = 'all'::text);


--
-- Name: idx_cred_auto_apply_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_cred_auto_apply_org ON public.credential_auto_apply USING btree (target_id) WHERE (target_type = 'org'::text);


--
-- Name: idx_cred_auto_apply_unique_all; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX IF NOT EXISTS idx_cred_auto_apply_unique_all ON public.credential_auto_apply USING btree (credential_id, target_type) WHERE (target_id IS NULL);


--
-- Name: idx_cred_auto_apply_unique_targeted; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX IF NOT EXISTS idx_cred_auto_apply_unique_targeted ON public.credential_auto_apply USING btree (credential_id, target_type, target_id) WHERE (target_id IS NOT NULL);


--
-- Name: idx_cred_auto_apply_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_cred_auto_apply_user ON public.credential_auto_apply USING btree (target_id) WHERE (target_type = 'user'::text);


--
-- Name: idx_dlq_unresolved; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_dlq_unresolved ON public.usage_events_dlq USING btree (last_retried_at NULLS FIRST) WHERE (resolved_at IS NULL);


--
-- Name: idx_email_tokens_user_kind; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_email_tokens_user_kind ON public.email_tokens USING btree (user_id, kind);


--
-- Name: idx_jwt_sessions_expires_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_jwt_sessions_expires_at ON public.jwt_sessions USING btree (expires_at);


--
-- Name: idx_jwt_sessions_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_jwt_sessions_user_id ON public.jwt_sessions USING btree (user_id);


--
-- Name: idx_org_invitations_email; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_org_invitations_email ON public.org_invitations USING btree (email) WHERE ((accepted_at IS NULL) AND (declined_at IS NULL));


--
-- Name: idx_org_invitations_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_org_invitations_org ON public.org_invitations USING btree (org_id) WHERE ((accepted_at IS NULL) AND (declined_at IS NULL));


--
-- Name: idx_org_memberships_single_user; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX IF NOT EXISTS idx_org_memberships_single_user ON public.org_memberships USING btree (user_id);


--
-- Name: idx_org_memberships_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_org_memberships_user ON public.org_memberships USING btree (user_id);


--
-- Name: idx_org_sso_domains; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_org_sso_domains ON public.org_sso_configs USING gin (claimed_domains);


--
-- Name: idx_org_sso_verified_domains; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_org_sso_verified_domains ON public.org_sso_configs USING gin (verified_domains);


--
-- Name: idx_orgs_created_by; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_orgs_created_by ON public.organizations USING btree (created_by);


--
-- Name: idx_orgs_slug_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX IF NOT EXISTS idx_orgs_slug_active ON public.organizations USING btree (slug) WHERE (deleted_at IS NULL);


--
-- Name: idx_orgs_slug_lower_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX IF NOT EXISTS idx_orgs_slug_lower_active ON public.organizations USING btree (lower(slug)) WHERE (deleted_at IS NULL);


--
-- Name: idx_orgs_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_orgs_status ON public.organizations USING btree (status);


--
-- Name: idx_permissions_resource; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_permissions_resource ON public.permissions USING btree (resource_type, resource_id);


--
-- Name: idx_permissions_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_permissions_user_id ON public.permissions USING btree (user_id);


--
-- Name: idx_provider_creds_owner; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_provider_creds_owner ON public.provider_credentials USING btree (owner_type, owner_id);


--
-- Name: idx_secret_bindings_workspace; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_secret_bindings_workspace ON public.user_secret_bindings USING btree (workspace_id);


--
-- Name: idx_session_index_parent; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_session_index_parent ON public.session_index USING btree (workspace_id, parent_session_id) WHERE (parent_session_id IS NOT NULL);


--
-- Name: idx_session_index_workspace; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_session_index_workspace ON public.session_index USING btree (workspace_id, last_message_at DESC NULLS LAST);


--
-- Name: idx_usage_actor_period; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_usage_actor_period ON public.usage_events USING btree (actor_id, period);


--
-- Name: idx_usage_idempotency; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_usage_idempotency ON public.usage_events USING btree (idempotency_key) WHERE (idempotency_key IS NOT NULL);


--
-- Name: idx_usage_owner_period; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_usage_owner_period ON public.usage_events USING btree (owner_id, owner_type, period);


--
-- Name: idx_usage_type_period; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_usage_type_period ON public.usage_events USING btree (event_type, period);


--
-- Name: idx_usage_workspace; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_usage_workspace ON public.usage_events USING btree (workspace_id, period) WHERE (workspace_id IS NOT NULL);


--
-- Name: idx_user_secrets_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_user_secrets_user_id ON public.user_secrets USING btree (user_id);


--
-- Name: idx_users_status; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_users_status ON public.users USING btree (status) WHERE (status <> 'active'::text);


--
-- Name: idx_wle_owner; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_wle_owner ON public.workspace_lifecycle_events USING btree (owner_id, owner_type, event_time);


--
-- Name: idx_wle_workspace; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_wle_workspace ON public.workspace_lifecycle_events USING btree (workspace_id, event_time);


--
-- Name: idx_workspace_agent_state_pending; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_workspace_agent_state_pending ON public.workspace_agent_state USING btree (pending_refresh) WHERE (pending_refresh = true);


--
-- Name: idx_workspaces_deleted; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_workspaces_deleted ON public.workspaces USING btree (deleted_at) WHERE (deleted_at IS NOT NULL);


--
-- Name: idx_workspaces_org; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_workspaces_org ON public.workspaces USING btree (org_id) WHERE (org_id IS NOT NULL);


--
-- Name: idx_workspaces_user_id; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_workspaces_user_id ON public.workspaces USING btree (user_id);


--
-- Name: idx_ws_cred_bindings_credential; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_ws_cred_bindings_credential ON public.workspace_credential_bindings USING btree (credential_id);


--
-- Name: idx_ws_cred_bindings_workspace; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX IF NOT EXISTS idx_ws_cred_bindings_workspace ON public.workspace_credential_bindings USING btree (workspace_id);


--
-- Name: instance_settings trg_instance_settings_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

DROP TRIGGER IF EXISTS trg_instance_settings_updated_at ON public.instance_settings;
CREATE TRIGGER trg_instance_settings_updated_at BEFORE UPDATE ON public.instance_settings FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();


--
-- Name: org_invitations trg_org_invitations_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

DROP TRIGGER IF EXISTS trg_org_invitations_updated_at ON public.org_invitations;
CREATE TRIGGER trg_org_invitations_updated_at BEFORE UPDATE ON public.org_invitations FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();


--
-- Name: org_sso_configs trg_org_sso_configs_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

DROP TRIGGER IF EXISTS trg_org_sso_configs_updated_at ON public.org_sso_configs;
CREATE TRIGGER trg_org_sso_configs_updated_at BEFORE UPDATE ON public.org_sso_configs FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();


--
-- Name: organizations trg_organizations_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

DROP TRIGGER IF EXISTS trg_organizations_updated_at ON public.organizations;
CREATE TRIGGER trg_organizations_updated_at BEFORE UPDATE ON public.organizations FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();


--
-- Name: provider_credentials trg_provider_credentials_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

DROP TRIGGER IF EXISTS trg_provider_credentials_updated_at ON public.provider_credentials;
CREATE TRIGGER trg_provider_credentials_updated_at BEFORE UPDATE ON public.provider_credentials FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();


--
-- Name: user_settings trg_user_settings_updated_at; Type: TRIGGER; Schema: public; Owner: -
--

DROP TRIGGER IF EXISTS trg_user_settings_updated_at ON public.user_settings;
CREATE TRIGGER trg_user_settings_updated_at BEFORE UPDATE ON public.user_settings FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();


--
-- Name: api_keys api_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: audit_log audit_log_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.audit_log
    ADD CONSTRAINT audit_log_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.organizations(id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: credential_auto_apply credential_auto_apply_credential_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.credential_auto_apply
    ADD CONSTRAINT credential_auto_apply_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES public.provider_credentials(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: credential_backfill_jobs credential_backfill_jobs_credential_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.credential_backfill_jobs
    ADD CONSTRAINT credential_backfill_jobs_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES public.provider_credentials(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: email_tokens email_tokens_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.email_tokens
    ADD CONSTRAINT email_tokens_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: jwt_sessions jwt_sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.jwt_sessions
    ADD CONSTRAINT jwt_sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_invitations org_invitations_accepted_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_invitations
    ADD CONSTRAINT org_invitations_accepted_by_fkey FOREIGN KEY (accepted_by) REFERENCES public.users(id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_invitations org_invitations_invited_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_invitations
    ADD CONSTRAINT org_invitations_invited_by_fkey FOREIGN KEY (invited_by) REFERENCES public.users(id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_invitations org_invitations_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_invitations
    ADD CONSTRAINT org_invitations_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_memberships org_memberships_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_memberships
    ADD CONSTRAINT org_memberships_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_memberships org_memberships_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_memberships
    ADD CONSTRAINT org_memberships_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_policies org_policies_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_policies
    ADD CONSTRAINT org_policies_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_policies org_policies_updated_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_policies
    ADD CONSTRAINT org_policies_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES public.users(id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: org_sso_configs org_sso_configs_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.org_sso_configs
    ADD CONSTRAINT org_sso_configs_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: organizations organizations_created_by_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.organizations
    ADD CONSTRAINT organizations_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.users(id);
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: permissions permissions_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.permissions
    ADD CONSTRAINT permissions_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_keys user_keys_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_keys
    ADD CONSTRAINT user_keys_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_secret_bindings user_secret_bindings_secret_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_secret_bindings
    ADD CONSTRAINT user_secret_bindings_secret_id_fkey FOREIGN KEY (secret_id) REFERENCES public.user_secrets(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_secret_bindings user_secret_bindings_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_secret_bindings
    ADD CONSTRAINT user_secret_bindings_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_secrets user_secrets_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_secrets
    ADD CONSTRAINT user_secrets_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: user_settings user_settings_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.user_settings
    ADD CONSTRAINT user_settings_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: workspace_agent_state workspace_agent_state_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.workspace_agent_state
    ADD CONSTRAINT workspace_agent_state_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: workspace_credential_bindings workspace_credential_bindings_credential_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.workspace_credential_bindings
    ADD CONSTRAINT workspace_credential_bindings_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES public.provider_credentials(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: workspace_credential_bindings workspace_credential_bindings_workspace_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.workspace_credential_bindings
    ADD CONSTRAINT workspace_credential_bindings_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: workspaces workspaces_org_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_org_id_fkey FOREIGN KEY (org_id) REFERENCES public.organizations(id) ON DELETE SET NULL;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
-- Name: workspaces workspaces_user_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

DO $idempotent$ BEGIN
ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.users(id) ON DELETE RESTRICT;
EXCEPTION WHEN duplicate_object THEN NULL; WHEN duplicate_table THEN NULL; WHEN invalid_table_definition THEN NULL; WHEN duplicate_alias THEN NULL;  END $idempotent$;


--
--



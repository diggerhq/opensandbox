# Managed channels (Telegram + iMessage via Linq) — Pattern B

## Goal

Add a "managed-channel" path to OpenClaw agent creation: the user picks a
primary channel (Telegram or iMessage), enters the minimum info needed
(a phone number for iMessage, nothing for Telegram), receives a
verification code via that channel, and starts chatting with their
agent. Additional channels can be attached to the same agent later. A
"Test" tab in the dashboard offers a web-chat sidekick for quick
testing.

This is **Phase 1**. Phase 2 (credits + paywall) is sketched at the
bottom but not implemented here.

## Architectural decision — Pattern B (managed)

Two patterns were considered:

- **Pattern A — per-agent credentials.** Each agent has its own Linq
  token + phone number, or its own Telegram bot/token. The OpenClaw
  in-sandbox plugins (`openclaw-linq-plugin`,
  `openclaw-telegram-plugin`) own the inbound listener and outbound
  send. Existing code in `sessions-api` already implements this for
  Telegram in `src/lib/orchestrators/channel.ts`.
- **Pattern B — platform-owned credentials, per-agent routing in a
  binding table.** One Linq account + small number pool, one Telegram
  bot at the platform level. A `channel_bindings` table maps
  `(channel, external_id) → agent_id`. Sessions-api owns inbound
  webhooks and outbound send; the sandbox is reduced to chat
  completions. The OpenClaw plugins are NOT used in this path.

**Phase 1 implements Pattern B as the default.** Pattern A stays
intact as an "Advanced / BYO bot" path; we don't remove it but we
don't surface it as the default in the create-agent UI.

Why Pattern B is the right default for the consumer flow:

- Linq onboarding is sales-driven (per their public docs), so users
  cannot self-provision Linq accounts. Per-agent Linq credentials are
  impossible for self-serve.
- BotFather is a manual chat-and-paste flow; not a consumer UX.
- "Receive a code on iMessage" only works if WE send from a number we
  own — i.e. platform-level credentials.

## End-user flow (the thing this enables)

1. Already-authenticated OpenComputer user clicks "Create agent".
2. Form: auto-generated nickname (e.g. `brave-otter`, editable) +
   channel picker (Telegram | iMessage) + channel-specific input
   (phone number in E.164 for iMessage; nothing for Telegram).
3. Submit → sandbox boot kicks off (existing `bootOpenclawViaDocker`
   path) AND the verification dispatch fires:
   - **iMessage**: 6-digit code generated, sent via Linq from the
     platform pool number to the user's phone. Dashboard shows
     "Enter the code we just texted you".
   - **Telegram**: random code generated, dashboard shows a deep-link
     `https://t.me/<platform_bot>?start=<code>` and a QR rendering of
     the same. User taps; the platform bot receives `/start <code>`;
     server matches it.
4. On verify, `channel_bindings` row is written:
   `(channel, external_id) → agent_id`. `external_id` = phone (linq)
   or `chat_id` (telegram).
5. User starts texting the platform handle (phone number / bot
   username). Inbound is routed to their agent's sandbox by the
   `channel_bindings` lookup.
6. Adding another channel later: agent detail page → "Add channel" →
   pick channel → run the same verify dance → second row written.
   One agent, multiple channels.
7. Web chat: "Test" tab on agent detail talks straight to the
   existing sessions-api `POST /v1/agents/:agentId/sessions` path. No
   channel involvement.

## Repos touched

- `sessions-api` (~/dev/local-test/sessions-api) — schema, routes,
  gateway, worker, channel adapters.
- `opencomputer` (~/dev/local-test/opencomputer) — `web/` dashboard
  changes only (new create-agent form, "Add channel" UI, "Test" tab).

## Schema additions (sessions-api `src/lib/schema.ts`)

Two new tables. Same drizzle style as the existing tables (see
`agents`, `instances`, `agent_events` for reference).

```ts
export const channelBindings = pgTable("channel_bindings", {
  id: serial("id").primaryKey(),
  channel: text("channel").notNull(),               // "linq" | "telegram"
  externalId: text("external_id").notNull(),        // phone (linq) or chat_id (telegram)
  agentId: text("agent_id").notNull().references(() => agents.id, { onDelete: "cascade" }),
  poolHandle: text("pool_handle").notNull(),        // platform phone (linq) or bot username (telegram)
  verifiedAt: timestamp("verified_at", { withTimezone: true, mode: "date" }).notNull().defaultNow(),
  createdAt: timestamp("created_at", { withTimezone: true, mode: "date" }).notNull().defaultNow(),
}, (t) => ({
  uniqExt: uniqueIndex("channel_bindings_channel_ext_id_unique").on(t.channel, t.externalId),
}));

export const channelPendingVerifications = pgTable("channel_pending_verifications", {
  code: text("code").primaryKey(),                  // unique 6-digit (linq) / 8-char alphanumeric (telegram)
  agentId: text("agent_id").notNull().references(() => agents.id, { onDelete: "cascade" }),
  channel: text("channel").notNull(),
  externalIdHint: text("external_id_hint"),         // phone the user typed (linq); null for telegram
  expiresAt: timestamp("expires_at", { withTimezone: true, mode: "date" }).notNull(),
  createdAt: timestamp("created_at", { withTimezone: true, mode: "date" }).notNull().defaultNow(),
});
```

`channel_bindings` has a unique index on `(channel, external_id)` so a
single phone or chat_id can only ever be bound to one agent. If a
user tries to re-pair the same phone to a new agent, the verify-confirm
endpoint must update or refuse based on policy (default: refuse with a
human-friendly error and tell them to disconnect the existing one
first).

A `channel_mode` column on `agents` is also added so the worker can
tell Pattern B agents apart from existing Pattern A ones:

```ts
// agents table — new column
channelMode: text("channel_mode").notNull().default("managed"), // "managed" | "byo"
```

Existing agents migrate to `byo` so their behavior doesn't change. New
agents created via the dashboard are `managed`.

## Channel adapter abstraction (sessions-api `src/lib/channels/`)

New folder. The orchestrator stops being telegram-specific.

```
src/lib/channels/
  registry.ts          // ChannelAdapter interface + lookup by name
  linq.ts              // Linq adapter
  telegram-shared.ts   // Shared Telegram bot adapter
```

Interface:

```ts
export interface ChannelAdapter {
  name: "linq" | "telegram";

  // Fired during verify-start. For linq: send the 6-digit code via Linq.
  // For telegram: just return the deep-link (sending happens when user clicks).
  startVerification(opts: {
    code: string;
    agentId: string;
    externalIdHint?: string; // phone for linq
  }): Promise<{ deepLink?: string }>;

  // Fired when the worker has a chat-completion reply to deliver.
  sendMessage(opts: {
    agentId: string;
    externalId: string;       // user's phone or chat_id
    poolHandle: string;       // platform phone or bot username
    text: string;
  }): Promise<void>;

  // Inbound webhook handlers register with the gateway routes (below).
  // Each adapter exports a function that parses + verifies its provider's
  // webhook shape and returns a normalized inbound event.
  parseInboundWebhook(req: Request): Promise<NormalizedInbound>;
}

export interface NormalizedInbound {
  channel: "linq" | "telegram";
  externalId: string;       // from-phone (linq) or chat_id (telegram)
  rawText: string;
  // For telegram only: detected /start <code> for verification matching
  startPayload?: string;
}
```

## API endpoint changes (sessions-api `src/routes/`)

### `agents.ts` — POST `/v1/agents` (modify existing)

Accept new optional fields:

```ts
{
  // existing fields…
  primary_channel?: "linq" | "telegram";
  channel_params?: { phone?: string };  // phone required when primary_channel === "linq"
}
```

When `primary_channel` is set:

1. Set `agent.channel_mode = "managed"`.
2. After the agent + sandbox boot is kicked off (existing async path),
   also call `startChannelVerification(agentId, primary_channel,
   channel_params)`.
3. Response includes a `pending_verification` block:
   ```ts
   {
     agent: { ... },
     pending_verification: {
       channel: "linq" | "telegram",
       code_required: boolean,           // true for linq, false for telegram
       deep_link?: string,               // telegram only
       expires_at: string,
     }
   }
   ```

### `agent-channels.ts` — new routes

- `POST /v1/agents/:agentId/channels/:channel/verify-start` — manual
  trigger (used by "Add channel" UI; the create-agent path triggers
  this internally). Returns the same `pending_verification` shape.
- `POST /v1/agents/:agentId/channels/:channel/verify-confirm` — body
  `{ code }`. For Linq, this is the user pasting the code from
  iMessage. For Telegram this endpoint exists but isn't called by the
  user; instead, the gateway's `/start` handler writes the binding.
  Dashboard polls `GET /v1/agents/:agentId/channels` to detect the
  binding write and advance the UI.
- `GET /v1/agents/:agentId/channels` — list bound channels. Existing
  endpoint is updated to return data from `channel_bindings`.
- `DELETE /v1/agents/:agentId/channels/:channel` — already exists for
  Pattern A; extend to also delete the corresponding
  `channel_bindings` row for managed agents.

### Gateway routes (sessions-api `src/routes/`)

Two new global routes (NOT per-agent):

- `POST /gw/linq` — Linq webhook. Verify HMAC-SHA256 with
  `LINQ_WEBHOOK_SECRET`. Parse via `linq.parseInboundWebhook`. Look up
  `(channel="linq", externalId=from_phone)` in `channel_bindings` →
  `agent_id`. Enqueue `webhook_inbound` (existing pgmq queue) with the
  normalized event. If no binding found AND the message looks like a
  pairing attempt, fall through to pending-verification matching.
- `POST /gw/telegram` — single platform Telegram bot webhook. Parse
  via `telegram-shared.parseInboundWebhook`. If the inbound is a
  `/start <code>` message: look up `channel_pending_verifications`
  by code, write `channel_bindings` row, delete the pending row,
  reply via `telegram-shared.sendMessage` with "You're connected to
  <agent nickname>". Otherwise: look up `(channel="telegram",
  externalId=chat_id)` and enqueue.

The existing per-agent route `POST /gw/agents/:agentId/telegram`
stays intact for Pattern A / BYO agents.

## Worker changes (sessions-api `src/lib/worker.ts`)

The `webhook_inbound` consumer already routes by `agent_id`. Add a
branch on `agent.channel_mode`:

- `byo` (existing path): leave alone — the OpenClaw in-sandbox plugin
  has already handled inbound and outbound.
- `managed` (new): the gateway already extracted the raw text. Worker:
  1. POST the user's text to the sandbox's openclaw
     `/v1/chat/completions` (already-bearer-token-protected; we own the
     token from `bootOpenclawViaDocker`'s return value, currently
     stored on the instance — see `src/lib/openclaw-image.ts:62`).
  2. Take the assistant reply, call the channel adapter's
     `sendMessage` to deliver back.
  3. Emit `agent_events` rows for "message_received" and "reply_sent"
     so the dashboard's Events tab shows traffic (existing pattern in
     `src/routes/gateway.ts`).

For Pattern B, the sandbox does NOT need the openclaw-linq-plugin or
openclaw-telegram-plugin installed. `bootOpenclawViaDocker` does NOT
need to push channel-specific config into `openclaw.json`. Boot path
is unchanged for the managed flow — it provisions a vanilla openclaw
runtime.

## Web UI changes (opencomputer `web/`)

### Create-agent form (`web/src/pages/Agents.tsx` or wherever agent-create lives)

- Replace nickname text input with auto-generated nickname (editable in
  small text below).
- Channel picker: two big tiles (Telegram, iMessage). Clicking one
  reveals channel-specific input.
- iMessage: phone-number field (E.164 with country picker).
- Telegram: nothing — just shows "We'll show you a link to tap once
  the agent is ready."
- Submit calls existing `POST /v1/agents` with the new
  `primary_channel` + `channel_params` fields.

### Verification UI (post-submit, same page or modal)

Two states based on `pending_verification.channel`:

- `linq`: input for the 6-digit code, calls `verify-confirm`. Show
  "Resend code" link with cooldown.
- `telegram`: a button "Open in Telegram" linking to
  `pending_verification.deep_link`, plus a QR code rendering of the
  same. Polls `GET /v1/agents/:agentId/channels` until the binding
  shows up; redirects to agent detail when it does.

### Agent detail page (`web/src/pages/AgentDetail.tsx`)

- "Channels" tab — already exists (per the route in `App.tsx`).
  Update to show bound channels with the platform handle the user
  texts (e.g. "iMessage: text +1-555-PLATFORM" / "Telegram: message
  @opencomputer_bot"). "Add channel" button kicks off the same
  verify flow for a second channel.
- "Test" tab — new. Simple chat UI. Calls existing
  `POST /v1/agents/:agentId/sessions` to create a session, then
  `POST /v1/agents/:agentId/instances/:id/messages` for each user
  turn (these endpoints already exist per the route map in
  `src/routes/`).

## Env / secrets needed (sessions-api fly app `bolt-platform`)

Set as fly secrets before deploy:

- `LINQ_API_TOKEN` — provided by Linq.
- `LINQ_FROM_PHONE` — the platform pool number Linq has assigned
  (E.164). Single number to start; can become a comma-separated pool
  later.
- `LINQ_WEBHOOK_SECRET` — set on the Linq dashboard side; used here
  to HMAC-verify inbound webhooks.
- `LINQ_API_URL` — defaults to `https://api.linqapp.com`; override
  for testing if needed.
- `TELEGRAM_PLATFORM_BOT_TOKEN` — token of the platform bot
  (created once via BotFather).
- `TELEGRAM_PLATFORM_BOT_USERNAME` — used to construct deep-links.

The Linq webhook URL must be registered with Linq once. Their public
docs landing page does NOT show the webhook-registration API; check
their full reference under `https://docs.linqapp.com/webhooks` (or
ask your Linq rep). Worst case: register `https://api.opencomputer.dev/gw/linq`
once via the Linq dashboard at the account level; routing is by
`from-phone` regardless.

The Telegram webhook URL is registered by us calling
`https://api.telegram.org/bot<token>/setWebhook` once at boot time
(or via a one-shot script). URL = `https://api.opencomputer.dev/gw/telegram`.

## Implementation order (the days)

Each day is one PR ideally.

**Day 1 — schema + channel adapter scaffolding + Linq outbound proven**
- Add the two new tables + `channel_mode` column to `agents`.
  Drizzle migration via `drizzle-kit generate`.
- Stub out `src/lib/channels/registry.ts`, `linq.ts`,
  `telegram-shared.ts` with the interface.
- Implement `linq.startVerification` (sends a 6-digit code via Linq
  `POST /api/partner/v3/chats`).
- Smoke: a small one-off script that sends "test 123456" to your
  iPhone via Linq, proving outbound works with the platform token.

**Day 2 — Linq inbound + verify-confirm + worker delivery loop**
- `POST /gw/linq` + HMAC verify + binding lookup + pgmq enqueue.
- `POST /v1/agents/:agentId/channels/linq/verify-start` and
  `verify-confirm`.
- Extend worker `webhook_inbound` consumer with the managed-mode
  branch — POST to sandbox openclaw `/v1/chat/completions`, call
  `linq.sendMessage` back.
- End-to-end: hand-curate one agent in `managed` mode, run verify,
  text the platform number from your iPhone, see the agent reply.

**Day 3 — Telegram shared bot end-to-end**
- One-shot script that calls `setWebhook` on the platform bot.
- `telegram-shared.parseInboundWebhook` + `sendMessage`.
- `POST /gw/telegram` with `/start <code>` handling.
- `verify-start` returns deep-link; binding written by gateway.
- End-to-end: hand-curate a second agent for Telegram, deep-link
  yourself in, text the bot, see the reply.

**Day 4 — Create-agent UI**
- Channel picker, auto-nickname, phone input.
- Verification UI (code input for linq, deep-link/QR for telegram).
- Wire to the new `primary_channel` field on `POST /v1/agents`.

**Day 5 — Add channel UI + cross-channel agent detail**
- "Add channel" button on agent detail.
- Channels tab shows bound channels with the platform handle.
- Test that a second channel binds to the same agent and inbound
  on either channel routes correctly.

**Day 6 — "Test" web chat tab + cleanup**
- New tab on agent detail. Talks to existing sessions endpoints. No
  channel involvement.
- Move the existing per-agent BotFather-token Telegram path behind
  an "Advanced" disclosure on the channels tab; default UI never
  exposes it.

Day-by-day scope estimates assume one engineer focused. Day 2 is the
riskiest since it's where Linq's HMAC + webhook payload shape gets
proven against reality.

## Pattern A coexistence notes

- Existing `connectChannel` orchestrator in
  `src/lib/orchestrators/channel.ts` stays in place. Refactor only the
  hard-coded `if (channel !== "telegram") throw` to dispatch via the
  channel registry, but its existing telegram phases (store_secret →
  webhook_register → channel_wire → wait_channel_ready) stay for `byo`
  agents.
- `bootOpenclawViaDocker` for `byo` agents continues to install the
  openclaw plugins; for `managed` agents it doesn't. Branch on
  `agent.channel_mode` early in the boot path.
- Existing `channel_status` JSON on instances stays for `byo`. For
  `managed` agents, channel state lives in `channel_bindings` only.

## Test plan

- One-off script that sends a code via Linq from a node REPL using
  `LINQ_API_TOKEN`. Phone receives "Your code: 123456". Pass.
- `POST /v1/agents` with `primary_channel: "linq"` and a phone →
  agent created, code received on phone, dashboard shows code input.
- Type the code → binding written → text the platform number → reply
  arrives on phone within ~5s.
- Repeat for telegram with deep-link.
- Add a second channel (telegram) to a linq-primary agent → bindings
  for both → texts on either route to the same agent.
- "Test" tab → chat works without involving channels.
- Try to bind the same phone to two different agents → second
  attempt returns a clear error.

## Out of scope (Phase 2 — credits + paywall)

To pick up after Phase 1 is shipped. Decisions already locked:

- **Flat-per-reply** credits model. 1 credit = 1 reply, regardless of
  channel. Internal model-cost ratios hidden from the user.
- **Per-user trial** of $10 (or 50 credits at $0.20/credit), one-time
  grant on first agent create, idempotent on `(user_id,
  reason="trial_grant")`.
- $20/mo subscription product + one-time top-up SKU. Stripe webhook
  already wired in `internal/api/router.go`.
- Schema: `account_credits(user_id PK, balance_cents, plan,
  trial_granted_at)` and immutable ledger
  `credit_events(id, user_id, delta_cents, reason, ref, created_at)`.
- Metering hook: in `worker.ts` between "received inbound" and
  "deliver to openclaw" — pre-flight check `balance_cents > 0`,
  debit after the chat-completion returns.
- Paywall message: when out of credits, the worker skips inference
  and instead replies via the channel adapter with a fixed paywall
  message + a billing link (`https://app.opencomputer.dev/billing`).
  iMessage: plain link. Telegram: inline-keyboard "Top up" button.
- "Out of credits" is strict (`balance_cents <= 0`); pre-flight
  check means we never dip negative.

Roughly 2–3 days for the credits subsystem after Phase 1.

## Open questions surfaced during impl (resolve as encountered)

1. **Linq webhook signature scheme** — exact header name (`X-Linq-Signature`
   guess; verify against Linq docs) and what's signed (raw body,
   timestamp + body, etc.).
2. **Linq webhook URL registration** — programmatic API or
   dashboard-only? If programmatic, add a one-shot `register_webhook.ts`
   script alongside the telegram one. If dashboard-only, just document
   the manual step.
3. **Linq inbound payload shape** — confirm `from`, `to`, `text`,
   message id, conversation id field names. Adjust
   `linq.parseInboundWebhook` accordingly.
4. **Telegram bot uniqueness** — confirm the platform bot exists and
   is dedicated to this purpose (not shared with the existing per-agent
   path). If it doesn't exist, create one via BotFather, copy token
   into fly secrets.
5. **Pool phone capacity** — start with one Linq number; revisit when
   active iMessage agents exceed Linq's per-number rate limits or when
   conversation-context confusion arises (unlikely, but possible if
   two users with the same area code text the same platform number
   from different US carrier-shared shortcodes).

## References (read these in any fresh context)

- `sessions-api/src/lib/orchestrators/channel.ts` —
  existing Pattern A telegram orchestrator. The phase pattern
  (`store_secret` → `webhook_register` → `channel_wire` →
  `wait_channel_ready` → `update_state`) is the template the
  channel-adapter dispatch will follow.
- `sessions-api/src/routes/gateway.ts` — existing
  per-agent telegram gateway. Mirror its enqueue-to-pgmq +
  emit-event pattern in the new global gateways.
- `sessions-api/src/lib/openclaw-image.ts` — boot
  path. The `gatewayToken` returned from `bootOpenclawViaDocker` is
  what the worker uses to authenticate to openclaw's
  `/v1/chat/completions`; persist it on the instance row if it isn't
  already.
- `sessions-api/src/lib/schema.ts` — drizzle schema
  reference. Match its style for the new tables.
- `sessions-api/src/routes/agent-sessions.ts` — the
  existing `POST /v1/agents/:agentId/sessions` endpoint that the
  "Test" tab on the dashboard will reuse.
- `https://github.com/linq-team/openclaw-linq-plugin` — README
  describes the plugin's `openclaw.json` config keys. Useful only as
  a reference for what the plugin layer would have done; we are
  replacing it with sessions-api-side transport.
- `https://docs.linqapp.com/` — Linq docs. The landing page covers
  `POST /api/partner/v3/chats` (outbound) and bearer-token auth.
  Deeper webhook + signature details are on subpages — read those
  before Day 2.

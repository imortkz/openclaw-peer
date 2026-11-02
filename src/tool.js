import { resolveService } from "./active.js";
import { LANE_KEY } from "./bridge.js";
import kit from "@sessionbus/kit";
import schemaKit from "@sessionbus/kit/sdk/js/schema.js";

const { ACTIONS, ProtocolError } = kit;
const { validationError } = schemaKit;

const SEND_LIST_TIMEOUT_MS = 5000;
const LIST_TIMEOUT_MS = 10000;

class ListTimeoutError extends Error {}

// Vendored from peer-common@d41b0ad mcp/sessionbus_tools.go:13-28; only the spawn retention default and OpenClaw wake guidance differ.
const TOOL_DESCRIPTION = `Call Sessionbus using the current Sessionbus identity. Written means local write completion only.
Arguments by action (wire validation remains in the public kit):
list: {} or {session_id:string} or {host:string}; session_id and host are exclusive. Returns self_info {session_id,name?,product,groups} for the bound originating caller, independent of filters or queried host. Compare row IDs to self_info.session_id; never infer self from names or order. Older daemons may omit self_info.
send: {message:nonempty string, target:string} or {message, targets:unique nonempty string[]} or {message, group:string, host?:string}; choose exactly one addressing form; host applies only to group. Use returned session IDs or unambiguous names. send accepts only message, target, targets, group and host; there is no summary field. Put all intended content in message. Example: {"action":"send","arguments":{"target":"RETURNED_SESSION_ID","message":"Your complete message"}}.
Rejected delivery is final for the same message; resend only after changing the message or target.
describe: {product:string, host?:string}; returns supported open fields and native extra arguments.
spawn fresh: {product:string, name:string, open:object, host?:string, extra_groups?:unique string[], persistent?:boolean, auto_close_ms?:nonnegative integer, notify?:boolean, notify_target?:string, trace?:"off"|"events"|"content"}. open accepts cwd, permission_mode, model, reasoning_effort (strings), arguments (string[]); use describe to check product support. spawn resume: {resume_session_id:string, persistent?:boolean, auto_close_ms?:nonnegative integer, notify?:boolean, notify_target?:string, trace?:"off"|"events"|"content"}. Choose a product using describe (for example "claude-peer" or "codex-peer"); use returned session_id, never a shell substitute.
trace: {session_id:string, mode:"off"|"events"|"content"}; changes live tracing for a direct child and returns its effective mode. Tracing defaults off, applies only to subsequent Sessionbus message sends and settled delivery metadata, and is not retained. events omits message bodies; content includes them. Trace copies are daemon-generated JSON trace envelopes delivered as ordinary messages. Each starts or schedules native work under the same mandatory-wake contract and is not replayed. It does not include Run or lane lifecycle events.
Policies are independent: persistent=false ends the lane when its owner leaves; true survives owner exit. This tool sends auto_close_ms 600000 (10 min) when you omit it on spawn, fresh or resume. Automatic close starts after a native completed/failed/interrupted terminal; 0 disables. An unavailable record without a native terminal does not start a new grace. Every lane message starts or schedules native work; there is no passive idle mode. Open itself starts no work or retirement deadline. Resume preserves persistence. The raw daemon API resets omitted auto_close_ms to 60000, but this tool supplies 600000 on resume too; persistence can be promoted, not demoted. Parent-owned lanes notify their owner unless notify=false; persistent lanes need an explicit notify_target (or one retained on resume/promotion). queued_for_next_turn means the delivery is retained in a bounded in-memory queue for an automatic next native run; it is neither durable storage nor model consumption. A completion pointer is an ordinary peer message from the lane, not the answer. Interactive wake follows its native carrier; delivery success does not prove collection.
run: {session_id:string, input:nonempty string}; starts and waits, returning session_id/run_id/state and either result (done) or reason (unavailable), without consuming the record.
start: same fields as run; returns {session_id,run_id} for collection by an authorized caller while that worker lives.
status: {session_id:string, run_id?:string}; non-consuming read, omitted run_id selects oldest unacknowledged record.
wait: {session_id:string, run_id?:string, timeout_ms?:nonnegative integer}; non-consuming read or running at the requested bound. Cancellation stops this wait, not the native turn or result retention.
ack: {session_id:string, run_id:string}; acknowledge the oldest terminal record: for state=done, first receive and use its result/outcome/native reason; for state=unavailable, first record/report its reason (there is no result). Both states must be acknowledged to advance the cursor. Never acknowledge running or infer an acknowledgeable record from an RPC error. Repeating an acknowledgment is idempotent, without returning the answer again. No skipping older records; unavailable does not claim a native terminal. Closing, auto-close or worker loss makes unacknowledged output unavailable; collection does not reset the retirement deadline.
interrupt: {session_id:string}; acknowledgment is not terminal completion.
close: {session_id:string, forget?:boolean}; forget: {session_id:string} closes with forget=true.
No unlisted argument fields. Message and input strings have a 262144-character wire limit. Native policy can deny any public call.
Prefer start over run. After start, end your turn: a 'Lane … is done' message wakes you; then wait with its run_id and ack. The result lives auto_close_ms after the lane finishes — set it higher, or 0 and close the lane yourself, if you will be busy longer. This plugin puts no time limit of its own on run and wait. It bounds two lookups that a federated host asleep would stall: before a send it looks up the recipients (a target on its own host, a bare name on this host, a group across every host unless host is given) and fails with "message NOT sent" if that takes over 5 s; an unfiltered list fails after 10 s. A filtered list has no plugin limit; list {host} for an awake host answers at once.`;

// Vendored from peer-common@d41b0ad mcp/sessionbus_tools.go:40-64.
const argumentProperties = Object.fromEntries(
  ["session_id", "host", "message", "target", "group", "product", "name", "resume_session_id", "notify_target", "input", "run_id"]
    .map((field) => [field, { type: "string" }]),
);
for (const field of ["targets", "extra_groups"]) argumentProperties[field] = { type: "array", items: { type: "string" } };
for (const field of ["persistent", "notify", "forget"]) argumentProperties[field] = { type: "boolean" };
for (const field of ["auto_close_ms", "timeout_ms"]) argumentProperties[field] = { type: "integer" };
for (const field of ["trace", "mode"]) argumentProperties[field] = { type: "string", enum: ["off", "events", "content"] };
argumentProperties.open = {
  type: "object",
  additionalProperties: false,
  properties: {
    cwd: { type: "string" }, permission_mode: { type: "string" }, model: { type: "string" }, reasoning_effort: { type: "string" },
    arguments: { type: "array", items: { type: "string" } },
  },
};
const TOOL_PARAMETERS = {
  type: "object", additionalProperties: false, required: ["action", "arguments"],
  properties: {
    action: { type: "string", enum: [...ACTIONS] },
    arguments: {
      type: "object", additionalProperties: false, properties: argumentProperties,
      description: "Use only the fields listed for the selected action in the tool description. send has no summary field; put the complete content in message.",
    },
  },
};

function textResult(value, suffix = "") {
  return { content: [{ type: "text", text: JSON.stringify(value) + suffix }] };
}

function errorResult(message) {
  return { content: [{ type: "text", text: message }], isError: true };
}

function formatError(error) {
  if (error instanceof ProtocolError) {
    return `sessionbus error ${error.code}: ${error.message}${error.data === undefined ? "" : ` ${JSON.stringify(error.data)}`}`;
  }
  return `sessionbus: ${error?.message || error}`;
}

function hostOf(sessionId) {
  if (typeof sessionId !== "string") return undefined;
  const at = sessionId.lastIndexOf("@");
  return at >= 0 ? sessionId.slice(at + 1) : undefined;
}

/**
 * Resolve a direct-send label to a row's canonical session_id, or null if unresolvable. As the
 * daemon does, an exact ID wins over names, and a name matching several rows (names are not unique
 * since Sessionbus 0.5.11) resolves to none, so its ambiguous rejection charges no lane's budget.
 */
function resolveLabel(label, rows, selfHost) {
  const byId = rows.find((row) => row.session_id === label || (selfHost && row.session_id === `${label}@${selfHost}`));
  if (byId) return byId.session_id;
  const byName = new Set(rows
    .filter((row) => row.name === label || (selfHost && row.name && `${row.name}@${selfHost}` === label))
    .map((row) => row.session_id));
  return byName.size === 1 ? [...byName][0] : null;
}

/**
 * Canonical recipient ids this send would reach, for the pre-send loop-guard
 * check. `resolved` gets pre-checked and reserved before the send;
 * unresolvable direct labels (`unresolvedCount`) keep the old behavior of
 * recording the receipt's session_id after the send, since we have no
 * canonical id for them ahead of time. Group sends with a successful lookup are fully
 * resolved — membership comes straight from the same `list` rows, and rows
 * are already keyed by distinct session_id so they cannot collide. Direct
 * labels may resolve to the same id; the guard counts that id only once.
 */
async function resolveRecipients(call, args, signal, timeoutMs) {
  const labels = Object.hasOwn(args, "target") ? [args.target] : args.targets;
  const scopeFor = (label) => {
    const host = hostOf(label);
    return host === undefined ? { session_id: label } : { host };
  };
  const scopes = labels ? labels.map(scopeFor) : [args.host === undefined ? {} : { host: args.host }];
  const uniqueScopes = new Map(scopes.map((scope) => [JSON.stringify(scope), scope]));
  const results = new Map(await Promise.all([...uniqueScopes].map(async ([key, scope]) => {
    const unfiltered = Object.keys(scope).length === 0;
    const message = unfiltered
      ? `message NOT sent — the bus list did not answer within ${timeoutMs / 1000} s (a federated host may be asleep). Nothing was delivered; address recipients as <name>@<host>, or give group with host.`
      : `message NOT sent — ${scope.session_id === undefined ? `host ${scope.host}` : `the local lookup for ${scope.session_id}`} did not answer within ${timeoutMs / 1000} s (a federated host may be asleep or unreachable). Nothing was delivered; retry later.`;
    try {
      return [key, await deadlineList(call, scope, signal, timeoutMs, message)];
    } catch (error) {
      if (!labels || error instanceof ListTimeoutError || signal?.aborted) throw error;
      // The daemon remains authoritative when a scoped lookup cannot resolve a recipient.
      return [key, undefined];
    }
  })));
  const listResult = results.values().next().value;
  const rowsFor = (label) => results.get(JSON.stringify(scopeFor(label)))?.sessions ?? [];
  const rows = listResult?.sessions ?? [];
  const selfId = [...results.values()].find((result) => result?.self_info)?.self_info.session_id;
  const selfHost = hostOf(selfId);

  if (Object.hasOwn(args, "target")) {
    const id = resolveLabel(args.target, rowsFor(args.target), selfHost);
    return id ? { resolved: [id], unresolvedCount: 0 } : { resolved: [], unresolvedCount: 1 };
  }
  if (Object.hasOwn(args, "targets")) {
    const resolved = [];
    let unresolvedCount = 0;
    for (const label of args.targets) {
      const id = resolveLabel(label, rowsFor(label), selfHost);
      if (!id) {
        unresolvedCount++;
        continue;
      }
      resolved.push(id);
    }
    return { resolved, unresolvedCount };
  }
  // group: every row other than self whose groups include it, restricted to @host when given.
  const resolved = rows
    .filter((row) => row.session_id !== selfId)
    .filter((row) => Array.isArray(row.groups) && row.groups.includes(args.group))
    .filter((row) => (args.host ? hostOf(row.session_id) === args.host : true))
    .map((row) => row.session_id);
  return { resolved, unresolvedCount: 0 };
}

async function handleSend(service, call, localId, args, signal, timeoutMs) {
  const invalid = validationError("MessageSendRequest", args);
  if (invalid) return errorResult(formatError(invalid));

  let resolved, unresolvedCount;
  try {
    ({ resolved, unresolvedCount } = await resolveRecipients(call, args, signal, timeoutMs));
  } catch (error) {
    return errorResult(formatError(error));
  }

  resolved = [...new Set(resolved)];

  // Pure pre-check first: would sending push any resolved recipient's pair
  // over budget, even before cooldown starts? All-or-nothing
  // — one blocked recipient refuses the whole fan-out, nothing sent.
  const wouldExceed = resolved.filter((id) => service.guard.wouldExceed(localId, id));
  if (wouldExceed.length > 0) {
    return errorResult(`openclaw-peer loop guard: pair budget exceeded for ${wouldExceed.join(", ")}`);
  }
  // Reserve synchronously, in this same tick, before awaiting the send — so
  // a concurrent call sees this reservation and cannot also pass the check
  // for the same pair. Never released on a rejected receipt (conservative).
  for (const id of resolved) service.guard.record(localId, id);

  let result;
  try {
    result = await call("send", args);
  } catch (error) {
    return errorResult(formatError(error));
  }

  const deliveries = result?.deliveries ?? [];
  if (unresolvedCount > 0) {
    // Only labels we couldn't canonicalize ahead of time weren't already
    // reserved above; record their receipts now (old behavior, kept for
    // exactly this case per the finding).
    const alreadyReserved = new Set(resolved);
    for (const delivery of deliveries) {
      if (delivery.disposition === "rejected" || !delivery.session_id) continue;
      if (!alreadyReserved.has(delivery.session_id)) service.guard.record(localId, delivery.session_id);
    }
  }
  service.logger?.info?.(
    `sessionbus: send ${result?.message_id} → ${deliveries.map((d) => d.session_id ?? d.target).join(", ")}`,
  );
  const hints = deliveries.filter((delivery) => delivery.disposition === "rejected")
    .map((delivery) => `rejected: ${delivery.reason || "no reason given"}. Resending the same message will be rejected again — change the message or target, or tell the owner.`);
  return textResult(result, hints.length ? `\n${hints.join("\n")}` : "");
}

function waitForReconnect(ms, signal) {
  if (signal?.aborted) return Promise.reject(signal.reason ?? new Error("aborted"));
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => { signal?.removeEventListener("abort", abort); resolve(); }, ms);
    const abort = () => { clearTimeout(timer); reject(signal.reason ?? new Error("aborted")); };
    signal?.addEventListener("abort", abort, { once: true });
  });
}

async function deadlineList(call, args, signal, timeoutMs, message) {
  const timer = AbortSignal.timeout(timeoutMs);
  const combined = signal ? AbortSignal.any([signal, timer]) : timer;
  try {
    return await call("list", args, combined);
  } catch (error) {
    if (combined.aborted && combined.reason === timer.reason) throw new ListTimeoutError(message);
    throw error;
  }
}

function reconnectingCall(peer, signal) {
  let deadline;
  return async (action, args, callSignal = signal) => {
    for (;;) {
      if (callSignal?.aborted) throw callSignal.reason ?? new Error("aborted");
      try {
        return await peer.action(action, args, callSignal);
      } catch (error) {
        if (error?.message !== "not connected") throw error;
        deadline ??= Date.now() + 5000;
        const remaining = deadline - Date.now();
        if (remaining <= 0) throw error;
        await waitForReconnect(Math.min(remaining, 250), callSignal);
      }
    }
  };
}

/**
 * Tool exists only in the bound session. Actions proxy to the owned
 * peer's `.action()` with the abort signal threaded through; `send` resolves
 * recipients to canonical ids and pre-checks the loop guard before sending,
 * refusing the whole fan-out if any recipient is blocked.
 */
export function createToolFactory(registeredService, { sendListTimeoutMs = SEND_LIST_TIMEOUT_MS, listTimeoutMs = LIST_TIMEOUT_MS } = {}) {
  return (ctx) => {
    // OpenClaw asks for tools from a tool-discovery load whose own service never started (see active.js).
    const service = resolveService(registeredService);
    const binding = service.bindingFor(ctx.agentId);
    // D7: the owner's bound key, or a channel route recorded by a bus delivery to this agent.
    const channelRoute = Boolean(binding) && service.routeKeys?.(ctx.agentId)?.has(ctx.sessionKey) === true;
    const owner = Boolean(binding) && (ctx.sessionKey === binding.sessionKey || channelRoute);
    // A lane's own session: its worker holds a live bridge registration for exactly this key.
    const laneConn = owner ? null : service.laneConn?.(ctx.sessionKey) ?? null;
    // Acceptance diagnostic: one line per factory call shows whether OpenClaw asks for the tool on a given
    // surface, with which session identity, and the source that admitted it.
    let admittedBy;
    if (channelRoute) admittedBy = "offered (channel route)";
    else if (owner) admittedBy = `offered (static agent:${ctx.agentId})`;
    else if (laneConn) admittedBy = "offered (lane bridge)";
    else admittedBy = `null (bound ${binding?.sessionKey ?? "none"})`;
    service.logger?.debug?.(
      `sessionbus: tool factory agentId=${ctx.agentId} sessionKey=${ctx.sessionKey} → ${admittedBy}`,
    );
    // A lane-shaped session without a live bridge registration gets no tool (D7); say so, it is the usual cause of a
    // lane agent reporting "Tool sessionbus not found" (worker not registered yet, bridge lost, or the Gateway hides
    // plugin tools behind Tool Search).
    if (!owner && !laneConn && typeof ctx.sessionKey === "string" && LANE_KEY.test(ctx.sessionKey)) {
      service.logger?.warn?.(`sessionbus: tool not offered to lane session ${ctx.sessionKey}: no live lane registration on the bridge`);
    }
    if (!owner && !laneConn) return null;
    // Lane voice: actions run in the lane worker, reached over its bridge connection.
    const voice = laneConn
      ? { action: (action, args, signal) => laneConn.request("bus.action", { action, arguments: args }, signal).then((reply) => reply.result) }
      : null;
    const localId = laneConn ? ctx.sessionKey : `openclaw-${ctx.agentId}`;

    return {
      name: "sessionbus",
      description: TOOL_DESCRIPTION,
      parameters: TOOL_PARAMETERS,
      async execute(_toolCallId, params, signal) {
        if (!params || typeof params !== "object" || Array.isArray(params) ||
            Object.keys(params).some((key) => key !== "action" && key !== "arguments") ||
            !ACTIONS.includes(params.action) || !params.arguments ||
            typeof params.arguments !== "object" || Array.isArray(params.arguments) ||
            Object.getPrototypeOf(params.arguments) !== Object.prototype) {
          return errorResult("sessionbus: expected a Sessionbus action and arguments object");
        }
        const { action } = params;
        const args = action === "spawn" && !Object.hasOwn(params.arguments, "auto_close_ms")
          ? { ...params.arguments, auto_close_ms: 600000 } : params.arguments;
        if (action !== "list") {
          const ids = ["session_id", "product", "host", "run_id"].filter((key) => Object.hasOwn(args, key))
            .map((key) => `${key}=${args[key]}`).join(" ");
          service.logger?.debug?.(`sessionbus: action ${action}${ids ? ` ${ids}` : ""}`);
        }
        try {
          const peer = voice ?? service.peerFor(ctx.agentId);
          if (!peer) return errorResult("sessionbus: this agent is not connected (plugin stopped or binding removed)");
          const call = reconnectingCall(peer, signal);
          if (action === "send") return await handleSend(service, call, localId, args, signal, sendListTimeoutMs);
          if (action === "list" && !Object.hasOwn(args, "session_id") && !Object.hasOwn(args, "host")) {
            return textResult(await deadlineList(call, args, signal, listTimeoutMs,
              `list did not answer within ${listTimeoutMs / 1000} s (a federated host may be asleep); use list {host} for one host.`));
          }
          return textResult(await call(action, args));
        } catch (error) {
          return errorResult(formatError(error));
        }
      },
    };
  };
}

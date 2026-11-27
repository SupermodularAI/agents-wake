//! Invocations as an OTLP `ExportTraceServiceRequest` (JSON mapping).

use crate::event::{Context, Invocation, Kind, Outcome, UserId};
use crate::ids::trace_id;
use serde_json::{json, Value};

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

struct Attrs(Vec<Value>);

impl Attrs {
    fn str(&mut self, k: &str, v: &str) {
        self.0.push(json!({"key": k, "value": {"stringValue": v}}));
    }
    fn int(&mut self, k: &str, v: u64) {
        self.0
            .push(json!({"key": k, "value": {"intValue": v.to_string()}}));
    }
    fn opt_str(&mut self, k: &str, v: &Option<String>) {
        if let Some(v) = v {
            self.str(k, v);
        }
    }
    fn opt_int(&mut self, k: &str, v: Option<u64>) {
        if let Some(v) = v {
            self.int(k, v);
        }
    }
}

fn tools_string(tools: &[(String, u64)]) -> String {
    let mut t: Vec<&(String, u64)> = tools.iter().collect();
    t.sort_by(|a, b| b.1.cmp(&a.1).then_with(|| a.0.cmp(&b.0)));
    t.iter()
        .map(|(n, c)| format!("{n}:{c}"))
        .collect::<Vec<_>>()
        .join(",")
}

fn span(ctx: &Context, trace: &str, inv: &Invocation) -> Value {
    let mut a = Attrs(Vec::new());
    a.str("wake.harness", ctx.harness);
    a.opt_str("wake.harness.version", &ctx.harness_version);
    a.str("wake.session.id", &ctx.session_id);
    a.str("session.id", &ctx.session_id);
    match &ctx.user {
        UserId::Email(e) => {
            a.str("user.id", e);
            a.str("wake.user.email", e);
        }
        UserId::Hash(h) => {
            a.str("user.id", h);
            a.str("wake.user.hash", h);
        }
        UserId::Unknown => {}
    }
    a.opt_str("wake.repo", &ctx.repo);
    // Trace-level in Langfuse, accepted on any span: groups sessions by repository.
    a.opt_str("langfuse.trace.name", &ctx.repo);
    a.str("wake.primitive.kind", inv.kind.as_str());
    a.str("wake.primitive.name", &inv.name);
    a.opt_str("wake.primitive.version", &inv.version);
    // Observation version in Langfuse, so dashboards can break a primitive down by version.
    a.opt_str("langfuse.version", &inv.version);
    a.opt_str("wake.plugin", &inv.plugin);
    a.opt_str("wake.marketplace", &inv.marketplace);
    a.opt_int("wake.primitive.context_tokens", inv.context_tokens);
    if let Some(i) = inv.invoker {
        a.str("wake.invoker", i.as_str());
    }
    if let Some(o) = inv.outcome {
        a.str("wake.outcome", o.as_str());
        // Langfuse groups observations by level but not by metadata, and its tags are
        // trace-level (a trace is a whole session), so the outcome is carried as the level.
        match o {
            Outcome::Error => a.str("langfuse.observation.level", "ERROR"),
            Outcome::Interrupted => a.str("langfuse.observation.level", "WARNING"),
            Outcome::Ok => {}
        }
    }
    // OTel GenAI semconv for the primitives it has a name for; skills and commands have none.
    match inv.kind {
        Kind::Subagent => {
            a.str("gen_ai.operation.name", "invoke_agent");
            a.str("gen_ai.agent.name", &inv.name);
        }
        Kind::McpTool => {
            a.str("gen_ai.operation.name", "execute_tool");
            a.str("gen_ai.tool.name", &inv.name);
        }
        Kind::Skill | Kind::Command | Kind::Inventory => {}
    }
    if inv.end_ns >= inv.start_ns {
        a.int(
            "wake.window.duration_ms",
            (inv.end_ns - inv.start_ns) / 1_000_000,
        );
    }
    a.opt_int("wake.window.tool_calls", inv.tool_calls);
    a.opt_int("wake.window.tool_errors", inv.tool_errors);
    if !inv.tools.is_empty() {
        a.str("wake.window.tools", &tools_string(&inv.tools));
    }
    if let Some(m) = &inv.mcp {
        a.str("wake.mcp.server", &m.server);
        a.int("wake.mcp.calls", m.calls);
        a.int("wake.mcp.errors", m.errors);
        a.int("wake.mcp.total_duration_ms", m.total_duration_ms);
        a.int("wake.mcp.max_duration_ms", m.max_duration_ms);
    }
    a.opt_str("gen_ai.request.model", &inv.model);
    let t = &inv.tokens;
    // OTel GenAI semconv: input_tokens is the total input, cached tokens included (Langfuse
    // subtracts the cache fields from it). Claude Code's input_tokens excludes cache, so add
    // the cache fields back. Absent only when no input-side count is known.
    let total_input = [t.input, t.cache_read, t.cache_creation]
        .iter()
        .flatten()
        .copied()
        .reduce(u64::saturating_add);
    a.opt_int("gen_ai.usage.input_tokens", total_input);
    a.opt_int("gen_ai.usage.output_tokens", t.output);
    a.opt_int("gen_ai.usage.cache_read_input_tokens", t.cache_read);
    a.opt_int("gen_ai.usage.cache_creation_input_tokens", t.cache_creation);
    let has_tokens = [t.input, t.output, t.cache_read, t.cache_creation]
        .iter()
        .any(Option::is_some);
    if inv.model.is_some() || has_tokens {
        // An explicit type always wins over Langfuse's inference, and generations are priced.
        a.str("langfuse.observation.type", "generation");
    }
    if inv.kind == Kind::Inventory {
        let values: Vec<Value> = inv
            .inventory
            .iter()
            .map(|s| json!({"stringValue": s}))
            .collect();
        a.0.push(json!({"key": "wake.inventory", "value": {"arrayValue": {"values": values}}}));
    }

    let name = if inv.kind == Kind::Inventory {
        "wake.inventory".to_string()
    } else {
        format!("wake.{} {}", inv.kind.as_str(), inv.name)
    };
    let mut s = json!({
        "traceId": trace,
        "spanId": hex(&inv.span_id),
        "name": name,
        "kind": 1,
        "startTimeUnixNano": inv.start_ns.to_string(),
        "endTimeUnixNano": inv.end_ns.to_string(),
        "attributes": a.0,
    });
    if let Some(p) = inv.parent_span_id {
        s["parentSpanId"] = json!(hex(&p));
    }
    if inv.outcome == Some(Outcome::Error) {
        s["status"] = json!({"code": 2}); // STATUS_CODE_ERROR
    }
    s
}

pub fn encode(ctx: &Context, invs: &[Invocation]) -> Value {
    let trace = hex(&trace_id(&ctx.session_id));
    let spans: Vec<Value> = invs.iter().map(|i| span(ctx, &trace, i)).collect();
    json!({"resourceSpans": [{
        "resource": {"attributes": [
            {"key": "service.name", "value": {"stringValue": "wake"}},
            {"key": "service.version", "value": {"stringValue": env!("CARGO_PKG_VERSION")}},
        ]},
        "scopeSpans": [{"scope": {"name": "wake"}, "spans": spans}],
    }]})
}

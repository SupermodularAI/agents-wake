pub mod claude_code;

use crate::event::Kind;

/// What wake knows about one installed skill or agent.
#[derive(Debug, Clone, PartialEq)]
pub struct PrimitiveInfo {
    pub kind: Kind,
    pub name: String,
    pub version: Option<String>,
    pub plugin: Option<String>,
    pub marketplace: Option<String>,
    pub context_tokens: Option<u64>,
}

pub trait PrimitiveLookup {
    fn skill(&self, name: &str) -> Option<PrimitiveInfo>;
    fn agent(&self, name: &str) -> Option<PrimitiveInfo>;
}

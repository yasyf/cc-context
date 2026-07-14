use std::fmt;

/// A validated raw JSON number lexeme, preserved verbatim from the source.
///
/// The decoder guarantees the lexeme matches the RFC 8259 number grammar;
/// nothing in the crate ever routes it through `f64` except the TOON adapter,
/// which guards that conversion (`toon.rs`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Number(String);

impl Number {
    pub(crate) fn from_lexeme(lexeme: String) -> Self {
        Self(lexeme)
    }

    /// The verbatim source lexeme.
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// Whether the lexeme is a pure decimal integer (no fraction, no exponent).
    pub fn is_integer_lexeme(&self) -> bool {
        !self.0.bytes().any(|b| matches!(b, b'.' | b'e' | b'E'))
    }
}

impl fmt::Display for Number {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

/// The ordered, duplicate-preserving IR every encoder consumes.
///
/// Objects keep source key order and duplicate keys; numbers are validated
/// raw lexemes. This mirrors the Go package's `toon.Object`/`[]any`/scalar
/// model (decode.go), where `float64` never appears.
#[derive(Clone, Debug, PartialEq)]
pub enum Value {
    Null,
    Bool(bool),
    Number(Number),
    String(String),
    Array(Vec<Value>),
    Object(Vec<(String, Value)>),
}

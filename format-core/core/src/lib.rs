//! Converts JSON and NDJSON tool output into token-lean encodings — TOON,
//! TRON, CSV/TSV, markdown tables, JSONL, prose unwrap, or compact JSON.
//! `select_encoding` classifies the payload's shape and emits its preferred
//! candidate encoding — the earliest within tolerance of the leanest — never
//! exceeding compact JSON by bytes; `encode_as` forces one format, emitting
//! even when larger. Canonical port of the Go package `internal/format`.

#![cfg_attr(not(test), deny(clippy::unwrap_used, clippy::expect_used))]

#[doc(hidden)]
pub mod classify;
mod decode;
mod encoders;
mod json;
mod jsonl;
mod toon;
mod tron;
mod value;

pub use value::{Number, Value};

use std::fmt;
use std::str::FromStr;

/// An output encoding. `select_encoding` is the auto path; every variant here
/// forces its encoder.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
#[repr(u8)]
pub enum Format {
    Toon,
    Tron,
    Csv,
    Tsv,
    Markdown,
    Jsonl,
    Prose,
    Json,
}

impl Format {
    pub const ALL: [Format; 8] = [
        Format::Toon,
        Format::Tron,
        Format::Csv,
        Format::Tsv,
        Format::Markdown,
        Format::Jsonl,
        Format::Prose,
        Format::Json,
    ];

    pub fn as_str(self) -> &'static str {
        match self {
            Format::Toon => "toon",
            Format::Tron => "tron",
            Format::Csv => "csv",
            Format::Tsv => "tsv",
            Format::Markdown => "markdown",
            Format::Jsonl => "jsonl",
            Format::Prose => "prose",
            Format::Json => "json",
        }
    }
}

impl fmt::Display for Format {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl FromStr for Format {
    type Err = Error;

    fn from_str(name: &str) -> Result<Self, Error> {
        Format::ALL
            .into_iter()
            .find(|f| f.as_str() == name)
            .ok_or_else(|| {
                Error::UnknownFormat(format!(
                    "invalid format {name:?}: want toon|tron|csv|tsv|markdown|jsonl|prose|json"
                ))
            })
    }
}

/// A hand-rolled bitset over [`Format`] for `SelectOpts::allow`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct FormatSet(u8);

impl FormatSet {
    pub const EMPTY: FormatSet = FormatSet(0);
    pub const ALL: FormatSet = FormatSet(u8::MAX);

    pub const fn with(self, f: Format) -> FormatSet {
        FormatSet(self.0 | 1 << f as u8)
    }

    pub const fn without(self, f: Format) -> FormatSet {
        FormatSet(self.0 & !(1 << f as u8))
    }

    pub const fn contains(self, f: Format) -> bool {
        self.0 & 1 << f as u8 != 0
    }
}

impl Default for FormatSet {
    fn default() -> Self {
        FormatSet::ALL
    }
}

/// The character separating values inside TOON array scopes.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Delimiter {
    #[default]
    Comma,
    Tab,
    Pipe,
}

impl From<Delimiter> for toon_format::Delimiter {
    fn from(d: Delimiter) -> Self {
        match d {
            Delimiter::Comma => toon_format::Delimiter::Comma,
            Delimiter::Tab => toon_format::Delimiter::Tab,
            Delimiter::Pipe => toon_format::Delimiter::Pipe,
        }
    }
}

impl FromStr for Delimiter {
    type Err = Error;

    fn from_str(s: &str) -> Result<Self, Error> {
        match s {
            "," => Ok(Delimiter::Comma),
            "\t" => Ok(Delimiter::Tab),
            "|" => Ok(Delimiter::Pipe),
            _ => Err(Error::UnknownFormat(format!("invalid delimiter {s:?}"))),
        }
    }
}

/// TOON-only encoding knobs.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ToonOpts {
    pub indent: usize,
    pub delimiter: Delimiter,
}

impl Default for ToonOpts {
    fn default() -> Self {
        Self {
            indent: 2,
            delimiter: Delimiter::Comma,
        }
    }
}

/// Options for [`select_encoding`] and [`encode_as`].
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct SelectOpts {
    /// Formats the auto path may pick from; compact JSON stays the implicit
    /// fallback regardless.
    pub allow: FormatSet,
    pub toon: ToonOpts,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// Decode failure or empty input; the caller owns passthrough policy.
    NotJson(String),
    UnknownFormat(String),
    /// The forced format cannot represent this shape (e.g. CSV on an object).
    UnsupportedShape(String),
    /// A number lexeme would be corrupted by TOON's f64 canonicalization.
    UnsafeNumber(String),
    /// The encoder is stubbed pending a later phase; auto mode skips it.
    Unimplemented(Format),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::NotJson(msg) => write!(f, "decode json: {msg}"),
            Error::UnknownFormat(msg) | Error::UnsupportedShape(msg) | Error::UnsafeNumber(msg) => {
                f.write_str(msg)
            }
            Error::Unimplemented(fmt_) => write!(f, "encode {fmt_}: not implemented"),
        }
    }
}

impl std::error::Error for Error {}

/// A chosen encoding: which format won and its rendered text.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Encoded {
    pub format: Format,
    pub text: String,
}

/// Decodes JSON or NDJSON from `src`, classifies its shape, and returns the
/// preferred candidate encoding — the earliest within tolerance of the
/// smallest — never exceeding compact JSON by bytes (port of format.go's
/// FormatAuto arm).
pub fn select_encoding(src: &str, opts: &SelectOpts) -> Result<Encoded, Error> {
    let v = decode::decode_all(src)?;
    Ok(encode_auto(&v, opts))
}

/// Decodes JSON or NDJSON from `src` and forces `format`'s encoder, emitting
/// even when larger than compact JSON and erroring loudly on an incompatible
/// shape.
pub fn encode_as(src: &str, format: Format, opts: &SelectOpts) -> Result<String, Error> {
    let v = decode::decode_all(src)?;
    encode_value(&v, format, opts)
}

/// Classifier candidates for `src`, exposed for the golden-corpus harness.
#[doc(hidden)]
pub fn classify_candidates(src: &str) -> Result<Vec<Format>, Error> {
    let v = decode::decode_all(src)?;
    Ok(classify::classify(&v).0)
}

fn encode_value(v: &Value, format: Format, opts: &SelectOpts) -> Result<String, Error> {
    match format {
        Format::Toon => toon::encode_toon(v, &opts.toon),
        Format::Tron => tron::encode_tron(v),
        Format::Csv => encoders::encode_csv(v),
        Format::Tsv => encoders::encode_tsv(v),
        Format::Markdown => encoders::encode_markdown(v),
        Format::Jsonl => jsonl::encode_jsonl(v),
        Format::Prose => encoders::encode_prose(v),
        Format::Json => Ok(json::compact_json(v)),
    }
}

// candidateTolerance is the relative size slack within which classifier order
// outranks a byte win: an earlier candidate beats a smaller later one unless
// the later one is more than 5% smaller.
const CANDIDATE_TOLERANCE_PCT: u64 = 5; // heuristic

/// Classifies `v` and returns the earliest candidate encoding whose size is
/// within CANDIDATE_TOLERANCE_PCT of the smallest, among candidates that pass
/// the byte-net invariant len(out) <= len(compact_json(v)) — classifier order
/// is a preference ranking, so a near-tie goes to the preferred format.
/// Candidates that error (Unimplemented included) or exceed the net are
/// skipped, and compact JSON is the implicit last contender, so auto mode
/// never fails. The tolerance check runs in integer arithmetic:
/// out_len * 100 <= min_len * 105.
fn encode_auto(v: &Value, opts: &SelectOpts) -> Encoded {
    let (candidates, _) = classify::classify(v);
    let json_out = json::compact_json(v);
    let mut outs: Vec<(Format, String)> = Vec::new();
    let mut min_len = 0usize;
    for f in candidates.into_iter().filter(|f| opts.allow.contains(*f)) {
        let Ok(out) = encode_value(v, f, opts) else {
            continue;
        };
        if out.len() > json_out.len() {
            continue;
        }
        if outs.is_empty() || out.len() < min_len {
            min_len = out.len();
        }
        outs.push((f, out));
    }
    for (format, text) in outs {
        if text.len() as u64 * 100 <= min_len as u64 * (100 + CANDIDATE_TOLERANCE_PCT) {
            return Encoded { format, text };
        }
    }
    Encoded {
        format: Format::Json,
        text: json_out,
    }
}

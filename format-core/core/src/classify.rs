//! Shape classifier: `analyze` computes payload statistics, `classify`
//! returns candidate formats in priority order. Port of classify.go; the
//! threshold provenance comments are the tuning log and are preserved
//! verbatim.

use crate::json::compact_json;
use crate::value::Value;
use crate::Format;
use std::collections::HashMap;

// Classifier thresholds. Each is annotated with its provenance: "measured"
// cites the literature claim it is grounded in; "heuristic" flags a value we
// invented and expect to tune.
pub const SMALL_PAYLOAD_BYTES: usize = 200; // measured: minification alone recovers 37–46% vs pretty JSON; format deltas below benchmark noise under this size
pub const PROSE_SHARE: f64 = 0.66; // heuristic: our invention — one prose field holding ≥2/3 of payload bytes marks the payload prose-dominant
pub const PROSE_MIN_BYTES: usize = 512; // heuristic: a "prose" field shorter than this is not worth unwrapping
pub const PROSE_ABSOLUTE_BYTES: usize = 2048; // heuristic: a prose field this large reads better unwrapped regardless of its share of the payload
pub const PROSE_CELL_CHARS: f64 = 80.0; // heuristic: average cell length past which a column reads as prose, not tabular data
pub const TABLE_TOKEN_PRESSURE: usize = 2000; // measured: markdown tables beat CSV on accuracy (+7.6pp, non-overlapping CIs) at a ~25–29% token premium that is cheap under this size (len/4 estimator)
pub const TOON_MIN_ROWS: usize = 100; // measured: the only published row floor for TOON wins; independent evals report TOON underperforming with Claude models on smaller tables
pub const TRON_MIN_REPEAT: usize = 3; // measured: TRON saves 0–27% when shapes repeat and inflates +21% when they do not (arXiv 2605.29676)
pub const UNIFORM_SHARE: f64 = 0.9; // heuristic: modal key-set share at or above which an array of objects counts as uniform
pub const HETERO_SHARE: f64 = 0.5; // heuristic: modal key-set share below which (≥3 rows) an array counts as heterogeneous
pub const NESTED_DEPTH_MIN: usize = 2; // heuristic: repeated shapes must sit at depth ≥2 for TRON's class-table overhead to amortize

/// The shape statistics `classify` branches on (port of Go's `analysis`).
#[derive(Debug, Default)]
pub struct Analysis {
    pub compact_bytes: usize, // len(compact_json(v))
    pub est_tokens: usize,    // compact_bytes/4 — the render.Cap charsPerToken estimator

    pub single_string: bool, // root is one JSON string

    // Dominant prose field: the largest multi-word string field on a root
    // object (raw string bytes, not their JSON-escaped length).
    pub prose_field_bytes: usize,
    pub prose_field_share: f64, // prose_field_bytes / compact_bytes

    // Root-array stats. rows counts every element; the modal share is over
    // element fingerprints — objects by key-set, non-objects by JSON kind —
    // so a scalar stream reads uniform, not heterogeneous.
    pub rows: usize,
    pub modal_share: f64,
    pub all_objects: bool,
    pub all_scalar: bool,   // every cell of every object row is an IR scalar
    pub prose_column: bool, // some column averages > PROSE_CELL_CHARS chars or holds an embedded newline
    pub has_nulls: bool,    // some object cell is null
    pub uniform: bool,      // all_objects && rows ≥ 2 && modal_share ≥ UNIFORM_SHARE && all_scalar
    pub hetero: bool,       // rows ≥ 3 && modal_share < HETERO_SHARE

    pub max_depth: usize, // deepest container nesting; the root container sits at depth 0
    pub max_repeat: usize, // occurrences of the most-repeated ≥2-prop key-set fingerprint at depth ≥ NESTED_DEPTH_MIN
}

/// Computes the shape statistics for an IR value.
pub fn analyze(v: &Value) -> Analysis {
    let mut a = Analysis {
        compact_bytes: compact_json(v).len(),
        ..Analysis::default()
    };
    a.est_tokens = a.compact_bytes / 4;

    match v {
        Value::String(_) => a.single_string = true,
        Value::Object(fields) => {
            a.prose_field_bytes = classify_prose_field(fields);
            if a.prose_field_bytes > 0 {
                a.prose_field_share = a.prose_field_bytes as f64 / a.compact_bytes as f64;
            }
        }
        Value::Array(elems) => classify_array_stats(&mut a, elems),
        _ => {}
    }

    let mut counts = HashMap::new();
    classify_walk(v, 0, &mut a.max_depth, &mut counts);
    for &n in counts.values() {
        a.max_repeat = a.max_repeat.max(n);
    }
    a
}

/// Returns candidate formats for `v` in priority order — first match wins —
/// plus the analysis it branched on. The auto arm (`encode_auto` in lib.rs)
/// encodes the candidates in order and picks the earliest within
/// CANDIDATE_TOLERANCE_PCT of the smallest that passes the byte-net invariant
/// len(out) <= len(compact_json(v)); compact JSON is always the implicit last
/// contender. That byte-net is the chart's step-7 "avoid full output
/// compression unless benchmarked" guard — a per-payload eval standing in for
/// accuracy benchmarks we can't run inline.
///
/// Branches against the user's 8-step format chart:
///
///  1. Size floor (pre-chart): compact JSON under SMALL_PAYLOAD_BYTES → JSON.
///     Format deltas are below benchmark noise at this size; minification
///     alone is the cheapest win in the whole chart.
///  2. Prose-dominant (chart step 2): the payload is a single JSON string, one
///     prose-like field of ≥ PROSE_MIN_BYTES holds ≥ PROSE_SHARE of payload
///     bytes, or a prose-like field reaches PROSE_ABSOLUTE_BYTES outright — a
///     body that big reads better unwrapped whatever rides along → prose
///     unwrap.
///  3. Uniform array of scalar-celled objects (chart steps 3+4): a prose
///     column → JSONL then markdown (CSV/TOON degrade on prose cells);
///     estimated tokens under TABLE_TOKEN_PRESSURE → markdown (accuracy beats
///     the ~25–29% token premium at this size); null cells under token
///     pressure → TOON then markdown (TOON's ~ handles nulls, CSV cannot
///     distinguish null from empty string); otherwise the CSV/TSV shootout,
///     with TOON entering only at ≥ TOON_MIN_ROWS.
///  4. Repeated nested shapes (chart step 5): some ≥2-prop key-set fingerprint
///     repeating ≥ TRON_MIN_REPEAT times at depth ≥ NESTED_DEPTH_MIN → TRON;
///     the repeat gate plus the byte-net covers TRON's +21% inflation failure
///     mode on non-repeating shapes.
///  5. Heterogeneous array (≥3 rows, modal fingerprint share < HETERO_SHARE)
///     or a folded NDJSON stream of mixed shapes → JSONL (self-delimiting,
///     per-line schema; honestly unbenchmarked).
///  6. Everything else → minified JSON. YAML rejected on measurement: +21–27%
///     tokens vs minified JSON with only model-conditional accuracy wins.
///
/// Chart steps deliberately N/A: step 1 (machine-validated generation) — read-
/// side only, we format tool output for the model to read, not model output
/// for machines to validate; step 3's HTML sub-branch — JSON-derived tables
/// cannot have merged cells/hierarchy, and HTML costs ~3× tokens; step 8
/// (file-native grep context) — we format transient tool output, not files.
/// A future refinement out of v1 scope: the HYVE-style hybrid split (majority
/// shape → table, stragglers → JSONL) when one shape covers ~75% of a mixed
/// array.
pub fn classify(v: &Value) -> (Vec<Format>, Analysis) {
    let a = analyze(v);
    let candidates = if a.compact_bytes < SMALL_PAYLOAD_BYTES {
        vec![Format::Json]
    } else if a.single_string
        || (a.prose_field_bytes >= PROSE_MIN_BYTES && a.prose_field_share >= PROSE_SHARE)
        || a.prose_field_bytes >= PROSE_ABSOLUTE_BYTES
    {
        vec![Format::Prose]
    } else if a.uniform {
        if a.prose_column {
            vec![Format::Jsonl, Format::Markdown]
        } else if a.est_tokens < TABLE_TOKEN_PRESSURE {
            vec![Format::Markdown]
        } else if a.has_nulls {
            vec![Format::Toon, Format::Markdown]
        } else {
            let mut candidates = vec![Format::Csv, Format::Tsv];
            if a.rows >= TOON_MIN_ROWS {
                candidates.push(Format::Toon);
            }
            candidates
        }
    } else if a.max_repeat >= TRON_MIN_REPEAT {
        vec![Format::Tron]
    } else if a.hetero {
        vec![Format::Jsonl]
    } else {
        vec![Format::Json]
    };
    (candidates, a)
}

/// Finds the byte size of the largest string field on `fields` that reads as
/// prose. Prose-like matches the prose encoder's dominant-field test exactly —
/// at least two whitespace-separated words — so the classifier never nominates
/// a field the prose encoder rejects (a single token with trailing whitespace
/// is not prose).
fn classify_prose_field(fields: &[(String, Value)]) -> usize {
    let mut size = 0;
    for (_, v) in fields {
        if let Value::String(s) = v {
            if s.len() > size && s.split_whitespace().count() >= 2 {
                size = s.len();
            }
        }
    }
    size
}

fn classify_array_stats(a: &mut Analysis, arr: &[Value]) {
    a.rows = arr.len();
    if a.rows == 0 {
        return;
    }
    a.all_objects = true;
    a.all_scalar = true;

    let mut counts: HashMap<String, usize> = HashMap::new();
    let mut col_chars: HashMap<&str, usize> = HashMap::new();
    let mut col_cells: HashMap<&str, usize> = HashMap::new();
    for e in arr {
        let Value::Object(fields) = e else {
            a.all_objects = false;
            *counts
                .entry(classify_kind_fingerprint(e).to_string())
                .or_default() += 1;
            continue;
        };
        *counts.entry(classify_fingerprint(fields)).or_default() += 1;
        for (key, cell) in fields {
            match cell {
                Value::Object(_) | Value::Array(_) => a.all_scalar = false,
                Value::Null => a.has_nulls = true,
                Value::String(s) => {
                    *col_chars.entry(key).or_default() += s.len();
                    *col_cells.entry(key).or_default() += 1;
                    if s.contains('\n') {
                        a.prose_column = true;
                    }
                }
                _ => {}
            }
        }
    }

    let modal = counts.values().copied().max().unwrap_or(0);
    a.modal_share = modal as f64 / a.rows as f64;
    for (key, &cells) in &col_cells {
        if col_chars.get(key).copied().unwrap_or(0) as f64 / cells as f64 > PROSE_CELL_CHARS {
            a.prose_column = true;
        }
    }
    a.uniform = a.all_objects && a.rows >= 2 && a.modal_share >= UNIFORM_SHARE && a.all_scalar;
    a.hetero = a.rows >= 3 && a.modal_share < HETERO_SHARE;
}

/// Records the deepest container and counts ≥2-prop key-set fingerprints
/// occurring at depth ≥ NESTED_DEPTH_MIN (root container = depth 0). Recursion
/// is bounded by the decoder's nesting-depth cap.
fn classify_walk(
    v: &Value,
    depth: usize,
    max_depth: &mut usize,
    counts: &mut HashMap<String, usize>,
) {
    match v {
        Value::Object(fields) => {
            *max_depth = (*max_depth).max(depth);
            if depth >= NESTED_DEPTH_MIN && fields.len() >= 2 {
                *counts.entry(classify_fingerprint(fields)).or_default() += 1;
            }
            for (_, val) in fields {
                classify_walk(val, depth + 1, max_depth, counts);
            }
        }
        Value::Array(elems) => {
            *max_depth = (*max_depth).max(depth);
            for e in elems {
                classify_walk(e, depth + 1, max_depth, counts);
            }
        }
        _ => {}
    }
}

/// The order-insensitive key-set fingerprint of an object: sorted keys emitted
/// as self-delimiting len:key blocks, so no key content — commas, NULs, any
/// separator — can forge a block boundary and collide two distinct key-sets.
fn classify_fingerprint(fields: &[(String, Value)]) -> String {
    let mut keys: Vec<&str> = fields.iter().map(|(k, _)| k.as_str()).collect();
    keys.sort_unstable();
    let mut out = String::new();
    for k in keys {
        out.push_str(&k.len().to_string());
        out.push(':');
        out.push_str(k);
    }
    out
}

/// Tags a non-object array element by its JSON kind so mixed-type streams read
/// heterogeneous while scalar streams read uniform.
fn classify_kind_fingerprint(e: &Value) -> &'static str {
    match e {
        Value::Array(_) => "\x00kind:array",
        Value::String(_) => "\x00kind:string",
        Value::Bool(_) => "\x00kind:bool",
        Value::Null => "\x00kind:null",
        Value::Number(_) => "\x00kind:number",
        Value::Object(_) => unreachable!("objects are fingerprinted by key-set"),
    }
}

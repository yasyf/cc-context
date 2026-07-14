//! TRON encoder ported from tron.go (tron-format.github.io): a JSON superset
//! that mints a `class NAME: k1,k2` header for every object key-set with two
//! or more properties occurring two or more times, emits matching objects as
//! `NAME(v1,v2,…)` with values reordered to declaration order, and leaves
//! everything else compact JSON.

use crate::json::{write_compact, write_json_string};
use crate::value::Value;
use crate::Error;
use std::collections::{HashMap, HashSet};

/// One minted TRON class: the key-set fingerprint, the assigned name, and the
/// declaration keys in first-seen order.
struct TronClass {
    fp: String,
    name: String,
    keys: Vec<String>,
}

/// Renders the IR as TRON: mints a class for every object key-set with two or
/// more properties occurring two or more times, declares each as a
/// `class NAME: k1,k2` header line, and emits matching objects as
/// `NAME(v1,v2,…)` with values reordered to declaration order. Everything else
/// is compact JSON.
pub(crate) fn encode_tron(v: &Value) -> Result<String, Error> {
    let order = tron_discover(v);
    let classes: HashMap<&str, &TronClass> = order.iter().map(|c| (c.fp.as_str(), c)).collect();

    let mut out = String::new();
    for cls in &order {
        out.push_str("class ");
        out.push_str(&cls.name);
        out.push_str(": ");
        for (i, k) in cls.keys.iter().enumerate() {
            if i > 0 {
                out.push(',');
            }
            tron_write_header_key(&mut out, k);
        }
        out.push('\n');
    }
    if !order.is_empty() {
        out.push('\n');
    }
    tron_write(&mut out, v, &classes);
    Ok(out)
}

/// Walks `v` in DFS pre-order fingerprinting every non-empty duplicate-free
/// object (each object counted before its children), then mints the qualifying
/// key-sets — at least two keys and at least two occurrences — assigning names
/// sequentially in discovery order. Returns the minted classes in order.
fn tron_discover(v: &Value) -> Vec<TronClass> {
    let mut counts: HashMap<String, usize> = HashMap::new();
    let mut seen: HashSet<String> = HashSet::new();
    let mut discovered: Vec<(String, Vec<String>)> = Vec::new();
    tron_walk(v, &mut counts, &mut seen, &mut discovered);

    let mut order: Vec<TronClass> = Vec::new();
    for (fp, keys) in discovered {
        if keys.len() < 2 || counts.get(&fp).copied().unwrap_or(0) < 2 {
            continue;
        }
        let name = tron_class_name(order.len());
        order.push(TronClass { fp, name, keys });
    }
    order
}

fn tron_walk(
    v: &Value,
    counts: &mut HashMap<String, usize>,
    seen: &mut HashSet<String>,
    discovered: &mut Vec<(String, Vec<String>)>,
) {
    match v {
        Value::Object(fields) => {
            if fields.is_empty() {
                return;
            }
            // A duplicate-key object never mints or matches a class —
            // NAME(v1,v2,…) carries one value per declaration key — so it stays
            // JSON with every field intact.
            if !tron_has_duplicate_key(fields) {
                let fp = tron_fingerprint(fields);
                *counts.entry(fp.clone()).or_default() += 1;
                if seen.insert(fp.clone()) {
                    discovered.push((fp, fields.iter().map(|(k, _)| k.clone()).collect()));
                }
            }
            for (_, val) in fields {
                tron_walk(val, counts, seen, discovered);
            }
        }
        Value::Array(elems) => {
            for e in elems {
                tron_walk(e, counts, seen, discovered);
            }
        }
        _ => {}
    }
}

/// The order-insensitive key-set identity of the object: sorted keys emitted as
/// self-delimiting len:key blocks, an encoding no key content can forge. The JS
/// reference joins with "," which collides for comma-containing keys
/// ({"a,b","c"} vs {"a","b,c"}) and corrupts the losing shape's data; a bare NUL
/// join merely relocates the collision to NUL-containing keys. The length prefix
/// is a deliberate divergence that keeps every key-set distinct.
fn tron_fingerprint(fields: &[(String, Value)]) -> String {
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

/// Reports whether the object repeats a field key; such objects cannot
/// round-trip through a class instance's one-value-per-key shape.
fn tron_has_duplicate_key(fields: &[(String, Value)]) -> bool {
    let mut seen: HashSet<&str> = HashSet::with_capacity(fields.len());
    fields.iter().any(|(k, _)| !seen.insert(k.as_str()))
}

/// Assigns the nth class name: A-Z, then A1-Z1, A2-Z2, ….
fn tron_class_name(index: usize) -> String {
    const LETTERS: &[u8; 26] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZ";
    let (cycle, pos) = (index / 26, index % 26);
    let letter = LETTERS[pos] as char;
    match cycle {
        0 => letter.to_string(),
        _ => format!("{letter}{cycle}"),
    }
}

/// Emits a declaration key: raw when it is an identifier, JSON-quoted otherwise.
fn tron_write_header_key(out: &mut String, key: &str) {
    match tron_is_ident(key) {
        true => out.push_str(key),
        false => write_json_string(out, key),
    }
}

/// Whether `key` is a bare identifier — /^[A-Za-z_][A-Za-z0-9_]*$/.
fn tron_is_ident(key: &str) -> bool {
    let mut bytes = key.bytes();
    match bytes.next() {
        Some(b) if b == b'_' || b.is_ascii_alphabetic() => {
            bytes.all(|b| b == b'_' || b.is_ascii_alphanumeric())
        }
        _ => false,
    }
}

/// Serializes `v` compactly: minted objects as `NAME(values in declaration
/// order)`, other objects as JSON with every key quoted, arrays as JSON,
/// scalars via the compact writer.
fn tron_write(out: &mut String, v: &Value, classes: &HashMap<&str, &TronClass>) {
    match v {
        Value::Object(fields) if fields.is_empty() => out.push_str("{}"),
        Value::Object(fields) => match classes.get(tron_fingerprint(fields).as_str()) {
            Some(cls) => {
                out.push_str(&cls.name);
                out.push('(');
                for (i, k) in cls.keys.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    tron_write(out, tron_field_value(fields, k), classes);
                }
                out.push(')');
            }
            None => {
                out.push('{');
                for (i, (key, val)) in fields.iter().enumerate() {
                    if i > 0 {
                        out.push(',');
                    }
                    write_json_string(out, key);
                    out.push(':');
                    tron_write(out, val, classes);
                }
                out.push('}');
            }
        },
        Value::Array(elems) => {
            out.push('[');
            for (i, e) in elems.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                tron_write(out, e, classes);
            }
            out.push(']');
        }
        _ => write_compact(out, v),
    }
}

/// Looks up `key` in the object; a fingerprint match guarantees presence.
fn tron_field_value<'a>(fields: &'a [(String, Value)], key: &str) -> &'a Value {
    match fields.iter().find(|(k, _)| k == key) {
        Some((_, v)) => v,
        None => panic!("tron_field_value: key {key:?} missing from fingerprint-matched object"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // Typed-IR-only port of tron.go's TestTronClassName: names cycle A-Z, then
    // A1-Z1, A2-Z2, ….
    #[test]
    fn class_name_sequence() {
        for (index, want) in [
            (0, "A"),
            (1, "B"),
            (25, "Z"),
            (26, "A1"),
            (27, "B1"),
            (51, "Z1"),
            (52, "A2"),
            (77, "Z2"),
        ] {
            assert_eq!(tron_class_name(index), want, "index {index}");
        }
    }

    #[test]
    fn is_ident_matches_regex() {
        for key in ["a", "_", "valid_name", "A1", "_x9"] {
            assert!(tron_is_ident(key), "want identifier: {key:?}");
        }
        for key in ["", "1a", "foo-bar", "a,b", "a b", "a\u{0000}b"] {
            assert!(!tron_is_ident(key), "want non-identifier: {key:?}");
        }
    }

    // The length-prefixed fingerprint keeps comma-containing key-sets distinct
    // where the JS reference's comma join collides {"a,b","c"} with
    // {"a","b,c"}.
    #[test]
    fn fingerprint_resists_comma_collision() {
        let ab_c = vec![
            ("a,b".to_string(), Value::Null),
            ("c".to_string(), Value::Null),
        ];
        let a_bc = vec![
            ("a".to_string(), Value::Null),
            ("b,c".to_string(), Value::Null),
        ];
        assert_ne!(tron_fingerprint(&ab_c), tron_fingerprint(&a_bc));
        assert_eq!(tron_fingerprint(&ab_c), "3:a,b1:c");
    }

    // A duplicate-key object cannot round-trip through a class instance's
    // one-value-per-key shape, so it never mints or matches.
    #[test]
    fn duplicate_key_detection() {
        let dup = vec![
            ("a".to_string(), Value::Null),
            ("a".to_string(), Value::Null),
        ];
        let uniq = vec![
            ("a".to_string(), Value::Null),
            ("b".to_string(), Value::Null),
        ];
        assert!(tron_has_duplicate_key(&dup));
        assert!(!tron_has_duplicate_key(&uniq));
    }
}

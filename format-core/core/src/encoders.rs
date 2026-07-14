//! The CSV, TSV, markdown, and prose encoders — `encode_csv`, `encode_tsv`,
//! `encode_markdown`, `encode_prose`, each a `pub(crate) fn(&Value) ->
//! Result<String, Error>` over the IR (TRON, TOON, JSONL, and compact JSON
//! live in their own modules). The returned string carries no trailing
//! newline. An encoder that cannot represent `v` (e.g. CSV on a non-tabular
//! shape) returns `Error::UnsupportedShape` with a message prefixed by its
//! name ("encode csv: …") and never falls back — lib.rs's dispatch owns
//! fallback policy.
//!
//! ## The IR
//!
//! Every encoder consumes the `Value` produced by the decoder (decode.rs),
//! never raw JSON, and nothing re-decodes. `Value::Object` preserves source
//! key order and duplicate keys; `Value::Number` is a validated raw lexeme —
//! f64 NEVER appears in the IR, because routing any number through f64
//! corrupts integers beyond 2^53. An NDJSON payload of two or more top-level
//! documents arrives pre-folded into a single `Value::Array` of those
//! documents (a lone document arrives as itself, unwrapped); encoders cannot
//! and must not distinguish a folded stream from a literal top-level array.
//!
//! ## Scalar rendering
//!
//! Every scalar emitted in a JSON-quoted position goes through
//! `json::write_compact`/`write_json_string` (json.rs): strings JSON-escaped
//! WITHOUT HTML escaping (<, >, & stay raw), numbers as verbatim lexemes,
//! bool as true/false, null as null. The encoders here whose output positions
//! take raw unquoted text — CSV/TSV cells, markdown cells, prose bodies —
//! handle the string case themselves and route every non-string scalar through
//! the compact writer so lexeme precision survives.
//!
//! ## Naming
//!
//! Every unexported helper carries its encoder's prefix: `csv_x` for the
//! CSV/TSV pair, `md_x` for markdown, `prose_x` for prose.

use crate::value::Value;
use crate::Error;
use std::collections::HashMap;

pub(crate) fn encode_csv(v: &Value) -> Result<String, Error> {
    csv_encode("csv", ',', v)
}

pub(crate) fn encode_tsv(v: &Value) -> Result<String, Error> {
    csv_encode("tsv", '\t', v)
}

fn csv_encode(name: &str, delimiter: char, v: &Value) -> Result<String, Error> {
    let (header, rows) = csv_table(name, v)?;
    Ok(csv_write_table(&header, &rows, delimiter))
}

fn csv_table(name: &str, v: &Value) -> Result<(Vec<String>, Vec<Vec<String>>), Error> {
    let Value::Array(elems) = v else {
        return Err(Error::UnsupportedShape(format!(
            "encode {name}: {} is not an array of objects",
            csv_value_kind(v)
        )));
    };
    if elems.is_empty() {
        return Err(Error::UnsupportedShape(format!(
            "encode {name}: empty array has no header"
        )));
    }
    let Value::Object(first) = &elems[0] else {
        return Err(Error::UnsupportedShape(format!(
            "encode {name}: row 0 is {}, not an object",
            csv_value_kind(&elems[0])
        )));
    };
    if first.is_empty() {
        return Err(Error::UnsupportedShape(format!(
            "encode {name}: row 0 has no keys"
        )));
    }
    let header: Vec<String> = first.iter().map(|(key, _)| key.clone()).collect();

    let mut rows = Vec::with_capacity(elems.len());
    for (i, elem) in elems.iter().enumerate() {
        let Value::Object(fields) = elem else {
            return Err(Error::UnsupportedShape(format!(
                "encode {name}: row {i} is {}, not an object",
                csv_value_kind(elem)
            )));
        };
        if fields.len() != header.len() {
            return Err(Error::UnsupportedShape(format!(
                "encode {name}: row {i} has {} keys, header has {}",
                fields.len(),
                header.len()
            )));
        }

        let mut cells: HashMap<&str, &Value> = HashMap::with_capacity(fields.len());
        for (key, value) in fields {
            if cells.insert(key, value).is_some() {
                return Err(Error::UnsupportedShape(format!(
                    "encode {name}: row {i} duplicates key {key:?}"
                )));
            }
        }

        let mut row = Vec::with_capacity(header.len());
        for key in &header {
            let Some(cell) = cells.get(key.as_str()) else {
                return Err(Error::UnsupportedShape(format!(
                    "encode {name}: row {i} is missing key {key:?}"
                )));
            };
            let Some(text) = csv_cell(cell) else {
                return Err(Error::UnsupportedShape(format!(
                    "encode {name}: row {i} key {key:?} holds {}, not a scalar",
                    csv_value_kind(cell)
                )));
            };
            row.push(text);
        }
        rows.push(row);
    }
    Ok((header, rows))
}

fn csv_cell(v: &Value) -> Option<String> {
    match v {
        Value::Null => Some(String::new()),
        Value::String(s) => Some(s.clone()),
        Value::Number(_) | Value::Bool(_) => {
            let mut out = String::new();
            crate::json::write_compact(&mut out, v);
            Some(out)
        }
        Value::Array(_) | Value::Object(_) => None,
    }
}

fn csv_write_table(header: &[String], rows: &[Vec<String>], delimiter: char) -> String {
    let mut out = String::new();
    csv_write_record(&mut out, header, delimiter);
    for row in rows {
        csv_write_record(&mut out, row, delimiter);
    }
    out.truncate(out.len() - 1);
    out
}

fn csv_write_record(out: &mut String, record: &[String], delimiter: char) {
    for (i, field) in record.iter().enumerate() {
        if i > 0 {
            out.push(delimiter);
        }
        if !csv_field_needs_quotes(field, delimiter) {
            out.push_str(field);
            continue;
        }
        out.push('"');
        for c in field.chars() {
            match c {
                '"' => out.push_str("\"\""),
                _ => out.push(c),
            }
        }
        out.push('"');
    }
    out.push('\n');
}

fn csv_field_needs_quotes(field: &str, delimiter: char) -> bool {
    if field.is_empty() {
        return false;
    }
    if field == r"\." {
        return true;
    }
    field.contains([delimiter, '"', '\r', '\n'])
        || field.chars().next().is_some_and(char::is_whitespace)
}

fn csv_value_kind(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

/// Renders the shared tabular IR (`csv_table`) as a GitHub-style markdown
/// table: `|a|b|` header, `|---|---|` separator, one `md_cell` row per object.
pub(crate) fn encode_markdown(v: &Value) -> Result<String, Error> {
    let (header, rows) = csv_table("markdown", v)?;
    let mut out = String::new();
    md_row(&mut out, &header);
    out.push_str("\n|");
    for _ in &header {
        out.push_str("---|");
    }
    for row in &rows {
        out.push('\n');
        md_row(&mut out, row);
    }
    Ok(out)
}

fn md_row(out: &mut String, cells: &[String]) {
    out.push('|');
    for cell in cells {
        md_cell(out, cell);
        out.push('|');
    }
}

fn md_cell(out: &mut String, s: &str) {
    // Backslash first: a cell ending in `\` must not escape the `\|` we add.
    // `\r\n` collapses to one `<br>` before any stray `\n`/`\r` does.
    out.push_str(
        &s.replace('\\', "\\\\")
            .replace('|', "\\|")
            .replace("\r\n", "<br>")
            .replace(['\n', '\r'], "<br>"),
    );
}

/// Unwraps a prose payload: a bare string verbatim, or an object's largest
/// multi-word string field as the body with the rest as `<key>value</key>` tags.
pub(crate) fn encode_prose(v: &Value) -> Result<String, Error> {
    match v {
        Value::String(s) => Ok(s.clone()),
        Value::Object(fields) => prose_object(fields),
        _ => Err(Error::UnsupportedShape(format!(
            "encode prose: cannot unwrap {}",
            csv_value_kind(v)
        ))),
    }
}

fn prose_object(fields: &[(String, Value)]) -> Result<String, Error> {
    let Some((dominant, body)) = prose_dominant(fields) else {
        return Err(Error::UnsupportedShape(
            "encode prose: no dominant prose field".to_string(),
        ));
    };
    let mut out = String::new();
    for (i, (key, value)) in fields.iter().enumerate() {
        if i != dominant {
            prose_write_tag(&mut out, key, value);
        }
    }
    if out.is_empty() {
        return Ok(body.to_string());
    }
    out.push('\n');
    out.push_str(body);
    Ok(out)
}

// Largest multi-word (>= 2 whitespace-split tokens) string field as
// `(index, body)`; a lone token never qualifies and ties keep the earlier field.
fn prose_dominant(fields: &[(String, Value)]) -> Option<(usize, &str)> {
    fields
        .iter()
        .enumerate()
        .filter_map(|(i, (_, value))| match value {
            Value::String(s) if s.split_whitespace().nth(1).is_some() => Some((i, s.as_str())),
            _ => None,
        })
        .fold(None, |best: Option<(usize, &str)>, (i, s)| match best {
            Some((_, b)) if s.len() <= b.len() => best,
            _ => Some((i, s)),
        })
}

fn prose_write_tag(out: &mut String, key: &str, value: &Value) {
    out.push('<');
    out.push_str(key);
    out.push('>');
    match value {
        Value::String(s) => out.push_str(s),
        _ => crate::json::write_compact(out, value),
    }
    out.push_str("</");
    out.push_str(key);
    out.push_str(">\n");
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Number;

    fn csv_row(fields: Vec<(&str, Value)>) -> Value {
        Value::Object(
            fields
                .into_iter()
                .map(|(key, value)| (key.to_string(), value))
                .collect(),
        )
    }

    fn csv_number(lexeme: &str) -> Value {
        Value::Number(Number::from_lexeme(lexeme.to_string()))
    }

    fn csv_assert_rejected_by_both(v: &Value, detail: &str) {
        for (name, encode) in [
            ("csv", encode_csv as fn(&Value) -> Result<String, Error>),
            ("tsv", encode_tsv as fn(&Value) -> Result<String, Error>),
        ] {
            assert_eq!(
                encode(v),
                Err(Error::UnsupportedShape(format!("encode {name}: {detail}")))
            );
        }
    }

    fn csv_assert_error(v: &Value, detail: &str) {
        assert_eq!(
            encode_csv(v),
            Err(Error::UnsupportedShape(format!("encode csv: {detail}")))
        );
    }

    #[test]
    fn csv_and_tsv_reject_non_array_root() {
        csv_assert_rejected_by_both(&Value::Object(vec![]), "object is not an array of objects");
    }

    #[test]
    fn csv_and_tsv_reject_empty_array() {
        csv_assert_rejected_by_both(&Value::Array(vec![]), "empty array has no header");
    }

    #[test]
    fn csv_rejects_zero_columns() {
        csv_assert_error(
            &Value::Array(vec![Value::Object(vec![])]),
            "row 0 has no keys",
        );
    }

    #[test]
    fn csv_rejects_non_object_row() {
        csv_assert_error(
            &Value::Array(vec![csv_number("1")]),
            "row 0 is number, not an object",
        );
    }

    #[test]
    fn csv_rejects_duplicate_keys() {
        let input = Value::Array(vec![csv_row(vec![
            ("a", Value::Null),
            ("a", Value::Bool(true)),
        ])]);
        csv_assert_error(&input, "row 0 duplicates key \"a\"");
    }

    #[test]
    fn csv_rejects_added_key() {
        let input = Value::Array(vec![
            csv_row(vec![("a", Value::Null), ("b", Value::Null)]),
            csv_row(vec![
                ("a", Value::Null),
                ("b", Value::Null),
                ("c", Value::Null),
            ]),
        ]);
        csv_assert_error(&input, "row 1 has 3 keys, header has 2");
    }

    #[test]
    fn csv_rejects_dropped_key() {
        let input = Value::Array(vec![
            csv_row(vec![("a", Value::Null), ("b", Value::Null)]),
            csv_row(vec![("a", Value::Null)]),
        ]);
        csv_assert_error(&input, "row 1 has 1 keys, header has 2");
    }

    #[test]
    fn csv_rejects_renamed_key() {
        let input = Value::Array(vec![
            csv_row(vec![("a", Value::Null), ("b", Value::Null)]),
            csv_row(vec![("a", Value::Null), ("c", Value::Null)]),
        ]);
        csv_assert_error(&input, "row 1 is missing key \"b\"");
    }

    #[test]
    fn csv_rejects_array_cell() {
        let input = Value::Array(vec![csv_row(vec![("a", Value::Array(vec![]))])]);
        csv_assert_error(&input, "row 0 key \"a\" holds array, not a scalar");
    }

    #[test]
    fn csv_rejects_object_cell() {
        let input = Value::Array(vec![csv_row(vec![("a", Value::Object(vec![]))])]);
        csv_assert_error(&input, "row 0 key \"a\" holds object, not a scalar");
    }

    #[test]
    fn csv_and_tsv_render_scalars_and_reordered_rows() {
        let input = Value::Array(vec![
            csv_row(vec![
                ("a", csv_number("12345678901234567890123456789")),
                ("b", Value::String("x,y".to_string())),
            ]),
            csv_row(vec![("b", Value::Bool(false)), ("a", Value::Null)]),
        ]);
        assert_eq!(
            encode_csv(&input),
            Ok("a,b\n12345678901234567890123456789,\"x,y\"\n,false".to_string())
        );
        assert_eq!(
            encode_tsv(&input),
            Ok("a\tb\n12345678901234567890123456789\tx,y\n\tfalse".to_string())
        );
    }

    #[test]
    fn csv_writer_matches_go_quoting_rules() {
        let header = [
            "plain",
            "",
            r"\.",
            " leading",
            "é",
            "a,b",
            "a\tb",
            "a\"b",
            "a\rb",
            "a\nb",
            "\u{00a0}x",
        ]
        .map(str::to_string);
        assert_eq!(
            csv_write_table(&header, &[], ','),
            "plain,,\"\\.\",\" leading\",é,\"a,b\",a\tb,\"a\"\"b\",\"a\rb\",\"a\nb\",\"\u{00a0}x\""
        );
        assert_eq!(
            csv_write_table(&header, &[], '\t'),
            "plain\t\t\"\\.\"\t\" leading\"\té\ta,b\t\"a\tb\"\t\"a\"\"b\"\t\"a\rb\"\t\"a\nb\"\t\"\u{00a0}x\""
        );
    }

    #[test]
    fn md_golden_renders_header_separator_and_rows() {
        let input = Value::Array(vec![
            csv_row(vec![
                ("name", Value::String("ada".to_string())),
                ("id", csv_number("1")),
                ("active", Value::Bool(true)),
                ("note", Value::Null),
                ("score", csv_number("2.5")),
            ]),
            csv_row(vec![
                ("name", Value::String("bob".to_string())),
                ("id", csv_number("2")),
                ("active", Value::Bool(false)),
                ("note", Value::String("x".to_string())),
                ("score", csv_number("0.125")),
            ]),
        ]);
        assert_eq!(
            encode_markdown(&input),
            Ok(
                "|name|id|active|note|score|\n|---|---|---|---|---|\n|ada|1|true||2.5|\n|bob|2|false|x|0.125|"
                    .to_string()
            )
        );
    }

    #[test]
    fn md_escapes_pipes_and_collapses_newlines() {
        let input = Value::Array(vec![csv_row(vec![
            ("a", Value::String("x|y".to_string())),
            ("b", Value::String("l1\nl2".to_string())),
            ("c", Value::String("crlf\r\nend".to_string())),
        ])]);
        assert_eq!(
            encode_markdown(&input),
            Ok("|a|b|c|\n|---|---|---|\n|x\\|y|l1<br>l2|crlf<br>end|".to_string())
        );
    }

    #[test]
    fn md_escapes_trailing_backslash_before_pipe() {
        let input = Value::Array(vec![csv_row(vec![
            ("p", Value::String("C:\\".to_string())),
            ("q", Value::String("x\\|y".to_string())),
        ])]);
        assert_eq!(
            encode_markdown(&input),
            Ok("|p|q|\n|---|---|\n|C:\\\\|x\\\\\\|y|".to_string())
        );
    }

    #[test]
    fn md_renders_bigint_cell_digit_exact() {
        let input = Value::Array(vec![csv_row(vec![(
            "n",
            csv_number("12345678901234567890123456789"),
        )])]);
        assert_eq!(
            encode_markdown(&input),
            Ok("|n|\n|---|\n|12345678901234567890123456789|".to_string())
        );
    }

    #[test]
    fn md_rejects_non_array_root() {
        assert_eq!(
            encode_markdown(&csv_number("7")),
            Err(Error::UnsupportedShape(
                "encode markdown: number is not an array of objects".to_string()
            ))
        );
    }

    #[test]
    fn md_rejects_array_cell() {
        let input = Value::Array(vec![csv_row(vec![("a", Value::Array(vec![]))])]);
        assert_eq!(
            encode_markdown(&input),
            Err(Error::UnsupportedShape(
                "encode markdown: row 0 key \"a\" holds array, not a scalar".to_string()
            ))
        );
    }

    #[test]
    fn prose_returns_bare_string_verbatim() {
        let body = "# Title\n\nFirst paragraph with several words.\nSecond line, still prose.";
        assert_eq!(
            encode_prose(&Value::String(body.to_string())),
            Ok(body.to_string())
        );
    }

    #[test]
    fn prose_tags_metadata_then_dominant_body() {
        let input = csv_row(vec![
            ("title", Value::String("Panic when config missing".to_string())),
            ("number", csv_number("1347")),
            ("state", Value::String("open".to_string())),
            ("locked", Value::Bool(false)),
            ("assignee", Value::Null),
            (
                "labels",
                Value::Array(vec![
                    Value::String("bug".to_string()),
                    Value::String("p1".to_string()),
                ]),
            ),
            (
                "body",
                Value::String(
                    "Running ccx with no config panics.\n\nSteps to reproduce:\n1. rm ~/.ccx.toml\n2. ccx repo overview\n\nExpected an error, got a panic.".to_string(),
                ),
            ),
        ]);
        assert_eq!(
            encode_prose(&input),
            Ok("<title>Panic when config missing</title>\n<number>1347</number>\n<state>open</state>\n<locked>false</locked>\n<assignee>null</assignee>\n<labels>[\"bug\",\"p1\"]</labels>\n\nRunning ccx with no config panics.\n\nSteps to reproduce:\n1. rm ~/.ccx.toml\n2. ccx repo overview\n\nExpected an error, got a panic.".to_string())
        );
    }

    #[test]
    fn prose_renders_nested_residual_as_compact_json() {
        let input = csv_row(vec![
            (
                "user",
                csv_row(vec![
                    ("login", Value::String("yasyf".to_string())),
                    ("id", csv_number("9223372036854775807")),
                ]),
            ),
            (
                "body",
                Value::String("A multi word prose body that dominates this payload.".to_string()),
            ),
        ]);
        assert_eq!(
            encode_prose(&input),
            Ok("<user>{\"login\":\"yasyf\",\"id\":9223372036854775807}</user>\n\nA multi word prose body that dominates this payload.".to_string())
        );
    }

    #[test]
    fn prose_only_body_has_no_blank_line() {
        let input = csv_row(vec![(
            "body",
            Value::String("just the prose body and nothing else at all".to_string()),
        )]);
        assert_eq!(
            encode_prose(&input),
            Ok("just the prose body and nothing else at all".to_string())
        );
    }

    #[test]
    fn prose_rejects_object_without_dominant_field() {
        let input = csv_row(vec![
            ("a", Value::String("single-token".to_string())),
            ("b", csv_number("42")),
        ]);
        assert_eq!(
            encode_prose(&input),
            Err(Error::UnsupportedShape(
                "encode prose: no dominant prose field".to_string()
            ))
        );
    }

    #[test]
    fn prose_rejects_array_root() {
        let input = Value::Array(vec![
            Value::String("one two three".to_string()),
            Value::String("four five six".to_string()),
        ]);
        assert_eq!(
            encode_prose(&input),
            Err(Error::UnsupportedShape(
                "encode prose: cannot unwrap array".to_string()
            ))
        );
    }

    #[test]
    fn prose_rejects_number_root() {
        assert_eq!(
            encode_prose(&csv_number("42")),
            Err(Error::UnsupportedShape(
                "encode prose: cannot unwrap number".to_string()
            ))
        );
    }

    #[test]
    fn prose_rejects_null_root() {
        assert_eq!(
            encode_prose(&Value::Null),
            Err(Error::UnsupportedShape(
                "encode prose: cannot unwrap null".to_string()
            ))
        );
    }
}

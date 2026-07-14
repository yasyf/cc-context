use crate::value::Value;

/// Serializes the ordered IR to minimal JSON, emitting number lexemes
/// verbatim. This is both the `Format::Json` encoder and the byte-net
/// baseline every auto candidate is measured against (port of json.go).
pub(crate) fn compact_json(v: &Value) -> String {
    let mut out = String::new();
    write_compact(&mut out, v);
    out
}

pub(crate) fn write_compact(out: &mut String, v: &Value) {
    match v {
        Value::Object(fields) => {
            out.push('{');
            for (i, (key, val)) in fields.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                write_json_string(out, key);
                out.push(':');
                write_compact(out, val);
            }
            out.push('}');
        }
        Value::Array(elems) => {
            out.push('[');
            for (i, e) in elems.iter().enumerate() {
                if i > 0 {
                    out.push(',');
                }
                write_compact(out, e);
            }
            out.push(']');
        }
        Value::Number(n) => out.push_str(n.as_str()),
        Value::String(s) => write_json_string(out, s),
        Value::Bool(b) => out.push_str(if *b { "true" } else { "false" }),
        Value::Null => out.push_str("null"),
    }
}

/// Emits a JSON string with Go encoding/json's escaping minus HTML escaping:
/// `<`, `>`, `&` pass through raw; U+2028/U+2029 are always escaped.
pub(crate) fn write_json_string(out: &mut String, s: &str) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => {
                out.push_str("\\u00");
                let b = c as u32;
                for shift in [4, 0] {
                    let d = b >> shift & 0xF;
                    out.push(char::from_digit(d, 16).unwrap_or('0'));
                }
            }
            '\u{2028}' => out.push_str("\\u2028"),
            '\u{2029}' => out.push_str("\\u2029"),
            c => out.push(c),
        }
    }
    out.push('"');
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn no_html_escaping() {
        let mut out = String::new();
        write_json_string(&mut out, "<a> & </a>");
        assert_eq!(out, "\"<a> & </a>\"");
    }

    #[test]
    fn control_chars_and_line_separators() {
        let mut out = String::new();
        write_json_string(&mut out, "\u{0008}\u{000C}\u{2028}\u{2029}");
        assert_eq!(out, "\"\\u0008\\u000c\\u2028\\u2029\"");
    }
}

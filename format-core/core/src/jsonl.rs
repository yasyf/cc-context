use crate::json::write_compact;
use crate::value::Value;
use crate::Error;

/// Renders an IR array as JSON Lines: one compact JSON document per element,
/// newline-separated, with no trailing newline. Any non-array root is an
/// error — NDJSON multi-document payloads arrive pre-folded into one array.
pub(crate) fn encode_jsonl(v: &Value) -> Result<String, Error> {
    let Value::Array(elems) = v else {
        return Err(Error::UnsupportedShape("encode jsonl: not an array".into()));
    };
    let mut out = String::new();
    for (i, e) in elems.iter().enumerate() {
        if i > 0 {
            out.push('\n');
        }
        write_compact(&mut out, e);
    }
    Ok(out)
}
